package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NetworkConfig struct {
	WiredInterface  string   `json:"wired_interface"`
	CellInterface   string   `json:"cell_interface"`
	IntervalSeconds int      `json:"interval_seconds"`
	FailureCount    int      `json:"failure_count"`
	RecoverySeconds int      `json:"recovery_seconds"`
	ProbeTargets    []string `json:"probe_targets"`
}

func defaultNetworkConfig() NetworkConfig {
	return NetworkConfig{WiredInterface: "eth0", CellInterface: "ppp0", IntervalSeconds: 5, FailureCount: 3, RecoverySeconds: 30, ProbeTargets: []string{"223.5.5.5", "119.29.29.29"}}
}

type InterfaceStats struct {
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	Present       bool      `json:"present"`
	Up            bool      `json:"up"`
	Index         uint64    `json:"index"`
	RXBytes       uint64    `json:"rx_bytes"`
	TXBytes       uint64    `json:"tx_bytes"`
	RXBPS         float64   `json:"rx_bps"`
	TXBPS         float64   `json:"tx_bps"`
	RateAvailable bool      `json:"rate_available"`
	ObservedSince time.Time `json:"observed_since"`
}
type TrafficSample struct {
	Time    time.Time `json:"time"`
	WiredRX float64   `json:"wired_rx"`
	WiredTX float64   `json:"wired_tx"`
	CellRX  float64   `json:"cell_rx"`
	CellTX  float64   `json:"cell_tx"`
}
type NetworkEvent struct {
	Time     time.Time `json:"time"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Reason   string    `json:"reason"`
	Inferred bool      `json:"inferred"`
}
type NetworkStatus struct {
	Config            NetworkConfig    `json:"config"`
	ConfigReadable    bool             `json:"config_readable"`
	Fresh             bool             `json:"fresh"`
	RecoveryRemaining *float64         `json:"recovery_remaining"`
	Interfaces        []InterfaceStats `json:"interfaces"`
	Samples           []TrafficSample  `json:"samples"`
	Events            []NetworkEvent   `json:"events"`
	HistoryError      string           `json:"history_error,omitempty"`
	LastTransition    *NetworkEvent    `json:"last_transition,omitempty"`
}
type counterReading struct {
	Index, RX, TX uint64
	Up            bool
}
type previousCounter struct {
	Reading     counterReading
	Time, Since time.Time
}
type networkHistory struct {
	Events     []NetworkEvent `json:"events"`
	LastActive string         `json:"last_active"`
	LastSwitch string         `json:"last_switch"`
}
type NetworkMonitor struct {
	mu           sync.Mutex
	configPath   string
	historyPath  string
	read         func(string) (counterReading, error)
	previous     map[string]previousCounter
	samples      []TrafficSample
	history      networkHistory
	historyError string
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,32}$`)

func readCounters(name string) (counterReading, error) {
	var c counterReading
	if !interfaceName.MatchString(name) {
		return c, fmt.Errorf("invalid interface name")
	}
	dir := filepath.Join("/sys/class/net", name)
	read := func(p string, base int) (uint64, error) {
		b, e := os.ReadFile(filepath.Join(dir, p))
		if e != nil {
			return 0, e
		}
		return strconv.ParseUint(strings.TrimSpace(string(b)), base, 64)
	}
	var err error
	if c.Index, err = read("ifindex", 10); err != nil {
		return c, err
	}
	if c.RX, err = read("statistics/rx_bytes", 10); err != nil {
		return c, err
	}
	if c.TX, err = read("statistics/tx_bytes", 10); err != nil {
		return c, err
	}
	flags, err := read("flags", 0)
	c.Up = flags&1 != 0
	return c, err
}

func newNetworkMonitor(dir, configPath string) *NetworkMonitor {
	n := &NetworkMonitor{configPath: configPath, historyPath: filepath.Join(dir, "network-history.json"), read: readCounters, previous: map[string]previousCounter{}, samples: []TrafficSample{}, history: networkHistory{Events: []NetworkEvent{}}}
	b, e := os.ReadFile(n.historyPath)
	if e == nil {
		if e = json.Unmarshal(b, &n.history); e != nil {
			n.historyError = "旧切换记录无法读取，正在显示本次观测"
			n.history = networkHistory{Events: []NetworkEvent{}}
			log.Printf("network history read: %v", e)
		}
	} else if !os.IsNotExist(e) {
		n.historyError = "切换记录暂时无法读取"
		log.Printf("network history read: %v", e)
	}
	return n
}

func (n *NetworkMonitor) sampleInterface(name, role string, now time.Time) InterfaceStats {
	s := InterfaceStats{Name: name, Role: role}
	c, e := n.read(name)
	if e != nil {
		delete(n.previous, name)
		return s
	}
	s.Present = true
	s.Up = c.Up
	s.Index = c.Index
	s.RXBytes = c.RX
	s.TXBytes = c.TX
	s.ObservedSince = now
	if p, ok := n.previous[name]; ok && p.Reading.Index == c.Index && c.RX >= p.Reading.RX && c.TX >= p.Reading.TX {
		s.ObservedSince = p.Since
		if elapsed := now.Sub(p.Time).Seconds(); elapsed > 0 {
			s.RXBPS = float64(c.RX-p.Reading.RX) / elapsed
			s.TXBPS = float64(c.TX-p.Reading.TX) / elapsed
			s.RateAvailable = true
		}
	}
	n.previous[name] = previousCounter{Reading: c, Time: now, Since: s.ObservedSince}
	return s
}

func (n *NetworkMonitor) saveHistory() error {
	// Preserve unreadable old history for manual recovery rather than overwrite it.
	if n.historyError != "" {
		return nil
	}
	b, e := json.MarshalIndent(n.history, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(n.historyPath), ".network-history-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(name, n.historyPath)
}

func (n *NetworkMonitor) observeSwitch(f Failover, c NetworkConfig, fresh bool) {
	if !fresh || f.LastSwitch == "" || f.LastSwitch == n.history.LastSwitch || (f.Active != "wired" && f.Active != "cellular") {
		return
	}
	t, e := time.Parse("2006-01-02T15:04:05-0700", f.LastSwitch)
	if e != nil {
		return
	}
	event := NetworkEvent{Time: t, From: n.history.LastActive, To: f.Active, Inferred: true}
	if n.history.LastSwitch == "" {
		event.From = "unknown"
		event.Reason = "首次记录，历史切换原因未观测"
		event.Inferred = false
	} else if f.Active == "cellular" {
		if !f.WiredLink || f.WiredIP == "" {
			event.Reason = "有线链路或地址不可用"
		} else if f.WiredFailures >= c.FailureCount {
			event.Reason = "有线上游连续探测失败"
		} else {
			event.Reason = "自动主备服务切换到蜂窝"
		}
	} else if !f.CellularPresent || f.CellularIP == "" {
		event.Reason = "蜂窝不可用，有线健康，立即回切"
	} else {
		event.Reason = "有线恢复稳定，自动回切"
	}
	n.history.Events = append([]NetworkEvent{event}, n.history.Events...)
	if len(n.history.Events) > 200 {
		n.history.Events = n.history.Events[:200]
	}
	n.history.LastSwitch = f.LastSwitch
	n.history.LastActive = f.Active
	if e := n.saveHistory(); e != nil {
		n.historyError = "切换历史保存失败"
		log.Printf("network history write: %v", e)
	}
}

func (n *NetworkMonitor) Collect(f Failover, fresh bool, now time.Time) NetworkStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	cfg := defaultNetworkConfig()
	readable := false
	if b, e := os.ReadFile(n.configPath); e == nil {
		var parsed NetworkConfig
		if json.Unmarshal(b, &parsed) == nil && interfaceName.MatchString(parsed.WiredInterface) && interfaceName.MatchString(parsed.CellInterface) && parsed.IntervalSeconds > 0 && parsed.FailureCount > 0 && parsed.RecoverySeconds > 0 && len(parsed.ProbeTargets) > 0 {
			cfg = parsed
			readable = true
		}
	}
	n.observeSwitch(f, cfg, fresh)
	wired := n.sampleInterface(cfg.WiredInterface, "wired", now)
	cell := n.sampleInterface(cfg.CellInterface, "cellular", now)
	sample := TrafficSample{Time: now, WiredRX: wired.RXBPS, WiredTX: wired.TXBPS, CellRX: cell.RXBPS, CellTX: cell.TXBPS}
	n.samples = append(n.samples, sample)
	if len(n.samples) > 60 {
		n.samples = n.samples[len(n.samples)-60:]
	}
	status := NetworkStatus{Config: cfg, ConfigReadable: readable, Fresh: fresh, Interfaces: []InterfaceStats{wired, cell}, Samples: append([]TrafficSample{}, n.samples...), Events: append([]NetworkEvent{}, n.history.Events...), HistoryError: n.historyError}
	if len(status.Events) > 0 {
		e := status.Events[0]
		status.LastTransition = &e
	}
	if fresh && f.Active == "cellular" && f.WiredHealthy && f.CellularPresent && f.CellularIP != "" {
		remaining := float64(cfg.RecoverySeconds) - f.WiredRecoverySeconds
		if remaining < 0 {
			remaining = 0
		}
		status.RecoveryRemaining = &remaining
	}
	return status
}
