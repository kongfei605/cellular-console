package main

import (
	"context"
	"github.com/kongfei605/cellular-console/internal/atmodem"
	"testing"
	"time"
)

func TestLocalNetworkRejectsChangedRouteUSBAndStaleHealth(t *testing.T) {
	n, err := newLocalNetwork("unused", "01110000", "en8", "https://example.com/health")
	if err != nil {
		t.Fatal(err)
	}
	n.state = Failover{Source: "macos-local", Active: "wired", WiredHealthy: true, CellularHealthy: true}
	n.config = NetworkConfig{WiredInterface: "en8", CellInterface: "en10"}
	n.observed = time.Now()
	n.session = "first"
	n.cell = "en10"
	route := "en8"
	session := "first"
	n.readRoute = func(context.Context) (string, string, error) { return route, "", nil }
	n.discover = func(context.Context) ([]atmodem.Device, error) {
		return []atmodem.Device{{Location: "01110000", Session: session, Network: "en10"}}, nil
	}
	if _, err = n.status(); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{"en0", "en10", "utun0"} {
		route = changed
		if _, err = n.status(); err == nil {
			t.Fatal("route change accepted", changed)
		}
	}
	route = "en8"
	session = "second"
	if _, err = n.status(); err == nil {
		t.Fatal("USB generation change accepted")
	}
	session = "first"
	n.observed = time.Now().Add(-16 * time.Second)
	if _, err = n.status(); err == nil {
		t.Fatal("stale health accepted")
	}
}
func TestLocalProbeEndpointValidation(t *testing.T) {
	for _, url := range []string{"http://example.com", "https://user:secret@example.com/", "https://example.com/?token=secret", "https://example.com/#secret"} {
		if _, err := newLocalNetwork("unused", "", "", url); err == nil {
			t.Fatal("unsafe probe endpoint accepted")
		}
	}
}
