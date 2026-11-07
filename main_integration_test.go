package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConsoleProcessHelper(t *testing.T) {
	if os.Getenv("EC20_TEST_MAIN") != "1" {
		return
	}
	at := -1
	for i, arg := range os.Args {
		if arg == "--" {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatal("missing child flags")
	}
	os.Args = append([]string{os.Args[0]}, os.Args[at+1:]...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	main()
}

// Exercise the actual CLI and both listeners concurrently. A handler-only TLS
// test cannot catch the wrong listener being captured by a startup goroutine.
func TestConsoleProcessHTTPSAndLegacyListeners(t *testing.T) {
	dir := t.TempDir()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	pair := fixture.TLS.Certificates[0]
	fixture.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]})
	key, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	for name, data := range map[string][]byte{"cert.pem": cert, "key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), "credentials.json": []byte(`{"username":"admin","password":"test-only-password-2026"}`)} {
		if e = os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	l1, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	l2, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		l1.Close()
		t.Fatal(e)
	}
	plain, secure := l1.Addr().String(), l2.Addr().String()
	l1.Close()
	l2.Close()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, "-test.run=^TestConsoleProcessHelper$", "--", "-demo", "-listen", plain, "-tls-listen", secure, "-public-origin", "https://"+secure, "-tls-cert", filepath.Join(dir, "cert.pem"), "-tls-key", filepath.Join(dir, "key.pem"), "-credentials", filepath.Join(dir, "credentials.json"), "-data-dir", filepath.Join(dir, "data"))
	cmd.Env = append(os.Environ(), "EC20_TEST_MAIN=1")
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("console child exit: %v: %s", e, logs.String())
			}
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Error("console child shutdown timed out")
		}
	}()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(cert)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		res, err := client.Get("https://" + secure + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("CLI HTTPS never became ready")
	}
	var wg sync.WaitGroup
	problems := make(chan string, 80)
	for i := 0; i < 40; i++ {
		for _, scheme := range []string{"http", "https"} {
			wg.Add(1)
			go func(scheme string) {
				defer wg.Done()
				host := plain
				want := 308
				if scheme == "https" {
					host = secure
					want = 200
				}
				res, err := client.Get(scheme + "://" + host + "/healthz")
				if err != nil {
					problems <- err.Error()
					return
				}
				defer res.Body.Close()
				body, _ := io.ReadAll(res.Body)
				if res.StatusCode != want {
					problems <- string(body)
				}
				if scheme == "http" && !strings.HasPrefix(res.Header.Get("Location"), "https://"+secure+"/") {
					problems <- "wrong redirect"
				}
			}(scheme)
		}
	}
	wg.Wait()
	close(problems)
	for problem := range problems {
		t.Error(problem)
	}
}
