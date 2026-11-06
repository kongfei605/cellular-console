package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func networkFixture(t *testing.T) (*NetworkMonitor, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(defaultNetworkConfig())
	os.WriteFile(cfg, b, 0600)
	n := newNetworkMonitor(dir, cfg)
	return n, dir
}

func TestNetworkRatesHandleInterfaceRemovalAndCounterReset(t *testing.T) {
	n, _ := networkFixture(t)
	c := counterReading{Index: 1, RX: 1000, TX: 500, Up: true}
	present := true
	n.read = func(string) (counterReading, error) {
		if !present {
			return counterReading{}, errors.New("removed")
		}
		return c, nil
	}
	now := time.Now()
	first := n.sampleInterface("ppp0", "cellular", now)
	if first.RateAvailable {
		t.Fatal("invented rate on first observation")
	}
	c.RX += 500
	c.TX += 100
	second := n.sampleInterface("ppp0", "cellular", now.Add(5*time.Second))
	if second.RXBPS != 100 || second.TXBPS != 20 || !second.RateAvailable {
		t.Fatal("rate math wrong", second)
	}
	present = false
	if n.sampleInterface("ppp0", "cellular", now.Add(10*time.Second)).Present {
		t.Fatal("removed interface still present")
	}
	present = true
	c = counterReading{Index: 2, RX: 10, TX: 5, Up: true}
	if n.sampleInterface("ppp0", "cellular", now.Add(15*time.Second)).RateAvailable {
		t.Fatal("hotplug caused a traffic spike")
	}
	c.RX = 2
	c.TX = 1
	if n.sampleInterface("ppp0", "cellular", now.Add(20*time.Second)).RateAvailable {
		t.Fatal("counter reset underflowed rate")
	}
}

func TestNetworkHistoryPersistsAndDoesNotInventReasons(t *testing.T) {
	n, dir := networkFixture(t)
	n.read = func(string) (counterReading, error) { return counterReading{Index: 1, Up: true}, nil }
	now := time.Now().Truncate(time.Second)
	f := demoFailover()
	f.LastSwitch = now.Add(-time.Minute).Format("2006-01-02T15:04:05-0700")
	s := n.Collect(f, true, now)
	if len(s.Events) != 1 || s.Events[0].Inferred || s.Events[0].From != "unknown" {
		t.Fatal("invented initial transition reason", s.Events)
	}
	f.Active = "cellular"
	f.WiredHealthy = false
	f.WiredFailures = 3
	f.LastSwitch = now.Add(5 * time.Second).Format("2006-01-02T15:04:05-0700")
	s = n.Collect(f, true, now.Add(5*time.Second))
	if len(s.Events) != 2 || !s.Events[0].Inferred || s.Events[0].From != "wired" {
		t.Fatal("observed transition missing")
	}
	reloaded := newNetworkMonitor(dir, n.configPath)
	reloaded.read = n.read
	s = reloaded.Collect(f, true, now.Add(10*time.Second))
	if len(s.Events) != 2 {
		t.Fatal("restart duplicated transition history")
	}
	f.Active = "wired"
	f.LastSwitch = now.Add(15 * time.Second).Format("2006-01-02T15:04:05-0700")
	if got := reloaded.Collect(f, false, now.Add(15*time.Second)); len(got.Events) != 2 {
		t.Fatal("stale failover state generated a transition")
	}
	info, e := os.Stat(filepath.Join(dir, "network-history.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("history permissions wrong", e)
	}
}

func TestRecoveryCountdownAndInvalidHistory(t *testing.T) {
	n, dir := networkFixture(t)
	n.read = func(string) (counterReading, error) { return counterReading{}, errors.New("offline") }
	f := demoFailover()
	f.Active = "cellular"
	f.WiredRecoverySeconds = 12
	s := n.Collect(f, true, time.Now())
	if s.RecoveryRemaining == nil || *s.RecoveryRemaining != 18 {
		t.Fatal("wrong recovery countdown")
	}
	f.CellularPresent = false
	if n.Collect(f, true, time.Now()).RecoveryRemaining != nil {
		t.Fatal("countdown shown after modem removal")
	}
	path := filepath.Join(dir, "network-history.json")
	os.WriteFile(path, []byte("broken history"), 0600)
	bad := newNetworkMonitor(dir, n.configPath)
	bad.read = n.read
	s = bad.Collect(f, true, time.Now())
	data, _ := os.ReadFile(path)
	if s.HistoryError == "" || string(data) != "broken history" {
		t.Fatal("corrupt history silently overwritten")
	}
}
