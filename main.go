package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	deviceID := flag.String("device-id", "", "ModemManager DeviceIdentifier to select")
	demo := flag.Bool("demo", false, "loopback-only simulated modem; no hardware operations")
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
	server := &http.Server{Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		server.Shutdown(c)
	}()
	log.Printf("EC20 Console v%s listening at http://%s (demo=%t)", version, *listen, *demo)
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("HTTP server: %w", err))
	}
}
