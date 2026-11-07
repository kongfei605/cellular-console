package gnsscontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type NetworkGuard struct{ Failover, Outbox string }

func (g NetworkGuard) Check() error {
	var f struct {
		Updated, Active, CellularIP string
		WiredHealthy                bool `json:"wired_healthy"`
		CellularHealthy             bool `json:"cellular_healthy"`
		CellularPresent             bool `json:"cellular_present"`
		VPNs                        map[string]bool
	}
	raw, e := os.ReadFile(g.Failover)
	if e != nil {
		return e
	}
	if e = json.Unmarshal(raw, &f); e != nil {
		return e
	}
	// The failover producer uses cellular_ip, while Updated/Active/VPNs are case-insensitive.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	_ = json.Unmarshal(fields["cellular_ip"], &f.CellularIP)
	t, e := time.Parse("2006-01-02T15:04:05-0700", f.Updated)
	age := time.Since(t)
	if e != nil || age > 30*time.Second || age < -time.Second || f.Active != "wired" || !f.WiredHealthy || !f.CellularPresent || !f.CellularHealthy || f.CellularIP == "" || len(f.VPNs) != 3 {
		return errors.New("network guard")
	}
	for _, up := range f.VPNs {
		if !up {
			return errors.New("VPN guard")
		}
	}
	iface, e := net.InterfaceByName("ppp0")
	if e != nil {
		return e
	}
	addrs, e := iface.Addrs()
	if e != nil {
		return e
	}
	found := false
	for _, a := range addrs {
		if strings.Split(a.String(), "/")[0] == f.CellularIP {
			found = true
		}
	}
	if !found {
		return errors.New("PPP address guard")
	}
	if _, e := os.ReadFile("/sys/bus/usb/devices/1-2/devnum"); e != nil {
		return e
	}
	if g.Outbox != "" {
		b, e := os.ReadFile(g.Outbox)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil {
			var records []struct{ Status string }
			if e = json.Unmarshal(b, &records); e != nil {
				return e
			}
			for _, v := range records {
				switch v.Status {
				case "pending", "creating", "ready", "submitting":
					return errors.New("SMS active")
				}
			}
		}
	}
	return nil
}
func (g NetworkGuard) Signature() (string, error) {
	// Includes PPP identity, address, USB enumeration and running PPP process IDs.
	parts := []string{}
	for _, p := range []string{"/sys/class/net/ppp0/ifindex", "/sys/bus/usb/devices/1-2/devnum"} {
		b, e := os.ReadFile(p)
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		parts = append(parts, p+"="+string(b))
	}
	if iface, e := net.InterfaceByName("ppp0"); e == nil {
		a, e := iface.Addrs()
		if e != nil {
			return "", e
		}
		for _, v := range a {
			parts = append(parts, "ppp_address="+v.String())
		}
	}
	processes, e := filepath.Glob("/proc/[0-9]*/comm")
	if e != nil {
		return "", e
	}
	for _, p := range processes {
		b, e := os.ReadFile(p)
		if e == nil && strings.TrimSpace(string(b)) == "pppd" {
			parts = append(parts, "ppp_pid="+filepath.Base(filepath.Dir(p)))
		}
	}
	sort.Strings(parts)
	return fmt.Sprint(parts), nil
}
