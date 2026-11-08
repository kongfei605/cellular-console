package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/kongfei605/ec20-toolbox/internal/voicecontrol"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8787", "HTTP listen address")
	data := flag.String("data-dir", "/var/lib/ec20-console", "private inbox directory")
	credsPath := flag.String("credentials", "/etc/ec20-console/credentials.json", "private administrator credentials file")
	failover := flag.String("failover-state", "/run/ec20-failover/status.json", "failover controller state")
	failoverConfig := flag.String("failover-config", "/etc/ec20-failover.json", "read-only failover configuration")
	deviceID := flag.String("device-id", "", "ModemManager DeviceIdentifier to select")
	demo := flag.Bool("demo", false, "loopback-only simulated modem; no hardware operations")
	tlsListen := flag.String("tls-listen", "", "unified HTTPS listen address; HTTP becomes redirects only")
	tlsCert := flag.String("tls-cert", "", "TLS certificate")
	tlsKey := flag.String("tls-key", "", "TLS private key")
	publicOrigin := flag.String("public-origin", "", "canonical HTTPS origin")
	audioBackend := flag.String("audio-backend", "", "fixed loopback audio control URL")
	gatewayKey := flag.String("audio-gateway-key", "", "private audio gateway credential file")
	gnssControl := flag.Bool("gnss-control", false, "allow guarded GNSS setup for the locally verified firmware profile")
	voiceControl := flag.Bool("ec20-voice", false, "prepare guarded USB voice before dial/answer")
	flag.Parse()
	if *demo && !strings.HasPrefix(*listen, "127.0.0.1:") {
		log.Fatal("demo mode must listen on 127.0.0.1")
	}
	b, err := os.ReadFile(*credsPath)
	if err != nil {
		log.Fatal("read credentials: ", err)
	}
	var creds credentials
	if err = json.Unmarshal(b, &creds); err != nil || len(creds.Password) < 12 || creds.Username == "" {
		log.Fatal("credentials must contain username and a password of at least 12 characters")
	}
	inbox, err := openInbox(*data)
	if err != nil {
		log.Fatal("open private inbox: ", err)
	}
	var backend Backend = &ModemBackend{expected: *deviceID}
	if *demo {
		backend = &DemoBackend{calls: []Call{{ID: "900", Number: "13800000000", Direction: "incoming", State: "来电", StateCode: 3}}}
	}
	defer backend.Close()
	app := newApp(backend, inbox, creds, *failover, *demo)
	app.network.configPath = *failoverConfig
	app.locationControl = *gnssControl
	if *voiceControl && !*demo {
		app.voicePrepare = func(ctx context.Context, phase, call string) error {
			_, e := (voicecontrol.Client{}).Prepare(ctx, phase, call)
			return e
		}
	}
	var tlsPair tls.Certificate
	if *tlsListen != "" {
		if *publicOrigin != "https://"+*tlsListen {
			log.Fatal("public-origin must exactly match HTTPS listen address")
		}
		tlsPair, err = tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatal("load TLS credentials: ", err)
		}
		app.secure = true
		app.publicOrigin = *publicOrigin
	}
	if *audioBackend != "" {
		b, e := os.ReadFile(*gatewayKey)
		if e != nil {
			log.Fatal("read private audio credential: ", e)
		}
		app.audio, e = newAudioProxy(*audioBackend, strings.TrimSpace(string(b)))
		if e != nil {
			log.Fatal(e)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.background(ctx)
	var listener net.Listener
	for {
		listener, err = net.Listen("tcp", *listen)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EADDRNOTAVAIL) {
			log.Fatal("listen: ", err)
		}
		log.Printf("VPN address not ready; retrying listen in 5 seconds")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	server := &http.Server{Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	var redirect *http.Server
	if app.secure {
		var secureListener net.Listener
		for {
			secureListener, err = net.Listen("tcp4", *tlsListen)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EADDRNOTAVAIL) {
				log.Fatal("HTTPS listen: ", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		redirect = &http.Server{Handler: canonicalRedirect(*publicOrigin), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		go func(redirectListener net.Listener) {
			if e := redirect.Serve(redirectListener); e != nil && !errors.Is(e, http.ErrServerClosed) {
				log.Printf("legacy redirect listener failed: %v", e)
				stop()
			}
		}(listener)
		listener = secureListener
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{tlsPair}}
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if redirect != nil {
			redirect.Shutdown(c)
		}
		server.Shutdown(c)
	}()
	if app.secure {
		log.Printf("EC20 Console v%s: HTTPS %s; legacy HTTP redirects %s", version, *tlsListen, *listen)
		err = server.ServeTLS(listener, "", "")
	} else {
		log.Printf("EC20 Console v%s listening at http://%s (demo=%t)", version, *listen, *demo)
		err = server.Serve(listener)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("HTTP server: %w", err))
	}
}
