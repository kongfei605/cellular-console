// Package hostconfig describes local device identity and network dependencies.
// It does not make arbitrary modem models/firmware safe to control.
package hostconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

const DefaultPath = "/etc/ec20-toolbox/host.json"
const ReviewedProfile = "ec20f-r08a03-ppp"
const ReviewedFirmware = "EC20CEFAGR08A03M4G"

type Config struct {
	SchemaVersion  int      `json:"schema_version"`
	Profile        string   `json:"profile"`
	DeviceID       string   `json:"device_id"`
	USBPath        string   `json:"usb_path"`
	WiredInterface string   `json:"wired_interface"`
	CellInterface  string   `json:"cell_interface"`
	VPNInterfaces  []string `json:"vpn_interfaces"`
}

func (c Config) Validate() error {
	if c.SchemaVersion != 1 || c.Profile != ReviewedProfile {
		return errors.New("unsupported hardware profile")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`).MatchString(c.DeviceID) || strings.Contains(strings.ToUpper(c.DeviceID), "REPLACE") || strings.HasPrefix(strings.ToLower(c.DeviceID), "example") || strings.Trim(c.DeviceID, "0") == "" {
		return errors.New("explicit local ModemManager device_id required")
	}
	if !regexp.MustCompile(`^[0-9]+-[0-9]+(?:\.[0-9]+)*$`).MatchString(c.USBPath) {
		return errors.New("invalid pinned USB topology")
	}
	iface := regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)
	if !iface.MatchString(c.WiredInterface) || !iface.MatchString(c.CellInterface) || c.WiredInterface == c.CellInterface {
		return errors.New("distinct wired and cellular interfaces required")
	}
	if len(c.VPNInterfaces) < 1 || len(c.VPNInterfaces) > 8 {
		return errors.New("one to eight explicitly monitored VPN interfaces required")
	}
	seen := map[string]bool{c.WiredInterface: true, c.CellInterface: true}
	for _, name := range c.VPNInterfaces {
		if !iface.MatchString(name) || seen[name] {
			return errors.New("invalid or duplicate VPN interface")
		}
		seen[name] = true
	}
	return nil
}

func Parse(raw []byte) (Config, error) {
	var c Config
	if len(raw) > 8192 {
		return c, errors.New("host profile too large")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&c); e != nil {
		return c, e
	}
	var trailing any
	if e := d.Decode(&trailing); e != io.EOF {
		return c, errors.New("trailing host profile data")
	}
	return c, c.Validate()
}

// Load requires a root-owned regular file with no group/world write access.
// Privileged helpers and unprivileged services must agree on this same file.
func Load(path string) (Config, error) {
	var c Config
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return c, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return c, e
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 {
		return c, errors.New("host profile must be root-owned, regular and not group/world writable")
	}
	b, e := io.ReadAll(io.LimitReader(f, 8193))
	if e != nil {
		return c, e
	}
	c, e = Parse(b)
	if e != nil {
		return c, fmt.Errorf("host profile: %w", e)
	}
	return c, nil
}
