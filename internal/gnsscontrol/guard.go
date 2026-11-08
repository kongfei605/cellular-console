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

type NetworkGuard struct {
	Failover               string
	CellInterface, USBPath string
	VPNInterfaces          []string
}

func (g NetworkGuard) configured() error {
	if g.CellInterface == "" || g.USBPath == "" || len(g.VPNInterfaces) == 0 {
		return errors.New("explicit cellular, USB and VPN guard configuration required")
	}
	return nil
}

func (g NetworkGuard) Check() error {
	if e := g.configured(); e != nil {
		return e
	}
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
	if e != nil || age > 30*time.Second || age < -time.Second || f.Active != "wired" || !f.WiredHealthy || !f.CellularPresent || !f.CellularHealthy || f.CellularIP == "" || len(f.VPNs) != len(g.VPNInterfaces) {
		return errors.New("network guard")
	}
	for _, name := range g.VPNInterfaces {
		if !f.VPNs[name] {
			return errors.New("VPN guard")
		}
	}
	iface, e := net.InterfaceByName(g.CellInterface)
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
	if _, e := os.ReadFile(filepath.Join("/sys/bus/usb/devices", g.USBPath, "devnum")); e != nil {
		return e
	}
	return nil
}
func (g NetworkGuard) Signature() (string, error) {
	if e := g.configured(); e != nil {
		return "", e
	}
	// Includes PPP identity, address, USB enumeration and running PPP process IDs.
	parts := []string{}
	for _, p := range []string{filepath.Join("/sys/class/net", g.CellInterface, "ifindex"), filepath.Join("/sys/bus/usb/devices", g.USBPath, "devnum")} {
		b, e := os.ReadFile(p)
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		parts = append(parts, p+"="+string(b))
	}
	if iface, e := net.InterfaceByName(g.CellInterface); e == nil {
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
