package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kongfei605/ec20-toolbox/internal/audioloop"
)

func main() {
	listen := flag.String("listen", "192.0.2.10:8788", "HTTPS IPv4 listen address")
	mediaBind := flag.String("media", "192.0.2.10:8789", "single VPN UDP media address")
	origin := flag.String("origin", "https://192.0.2.10:8788", "exact browser origin")
	cert := flag.String("cert", "/etc/ec20-audio-loopback/server.crt", "TLS leaf certificate")
	key := flag.String("key", "/etc/ec20-audio-loopback/server.key", "TLS private key")
	credsPath := flag.String("credentials", "/etc/ec20-console/credentials.json", "existing administrator credentials")
	flag.Parse()
	var creds audioloop.Credentials
	b, err := os.ReadFile(*credsPath)
	if err != nil {
		log.Fatal(err)
	}
	if json.Unmarshal(b, &creds) != nil || creds.Username == "" || len(creds.Password) < 12 {
		log.Fatal("invalid credentials")
	}
	pair, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var media *audioloop.Media
	var listener net.Listener
	for {
		listener, err = net.Listen("tcp4", *listen)
		if err == nil {
			media, err = audioloop.NewMedia(*mediaBind)
			if err == nil {
				break
			}
			listener.Close()
		}
		if !errors.Is(err, syscall.EADDRNOTAVAIL) {
			log.Fatal(err)
		}
		log.Print("VPN address unavailable; waiting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	defer media.Close()
	app := audioloop.NewServer(media, *origin, creds)
	creds.Password = ""
	srv := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				srv.Shutdown(c)
				return
			case now := <-ticker.C:
				media.Sweep(now)
			}
		}
	}()
	log.Printf("EC20 audio loopback %s: HTTPS %s, UDP %s, no hardware access", audioloop.Version, *listen, *mediaBind)
	err = srv.ServeTLS(listener, "", "")
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
