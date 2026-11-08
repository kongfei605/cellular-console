package hostconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLoadRejectsSymlinksAndNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "profile.json")
	raw, _ := json.Marshal(fixture())
	if e := os.WriteFile(target, raw, 0666); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "link")
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	if _, e := Load(link); e == nil {
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Load(fifo); e == nil {
		t.Fatal("non-regular profile accepted")
	}
}

func fixture() Config {
	return Config{1, ReviewedProfile, "test-device-identifier", "2-3.4", "eth9", "ppp7", []string{"wg0", "tun7"}}
}
func TestExplicitIdentityAndReviewedProfileRequired(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.Profile = "unknown-firmware" }, func(c *Config) { c.DeviceID = "REPLACE_WITH_LOCAL_DEVICE_IDENTIFIER" }, func(c *Config) { c.DeviceID = "" }, func(c *Config) { c.USBPath = "../../dev" }, func(c *Config) { c.VPNInterfaces = nil }, func(c *Config) { c.VPNInterfaces = []string{"wg0", "wg0"} }, func(c *Config) { c.CellInterface = c.WiredInterface }} {
		c := fixture()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("unsafe profile accepted: %+v", c)
		}
	}
	c := fixture()
	b, _ := json.Marshal(c)
	parsed, e := Parse(b)
	if e != nil || parsed.USBPath != c.USBPath {
		t.Fatal(parsed, e)
	}
	for _, raw := range [][]byte{append(b, []byte(` {}`)...), []byte(`{"unreviewed_option":true}`)} {
		if _, e = Parse(raw); e == nil {
			t.Fatal("unknown/trailing data accepted")
		}
	}
}
