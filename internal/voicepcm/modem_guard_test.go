package voicepcm

import (
	"encoding/json"
	"errors"
	"github.com/godbus/dbus/v5"
	"testing"
	"time"
)

func TestConfiguredVPNSetCannotBeReplacedByDifferentHealthyTunnels(t *testing.T) {
	if !vpnsHealthy(map[string]bool{"wg0": true, "tun7": true}, []string{"wg0", "tun7"}) {
		t.Fatal("configured VPNs rejected")
	}
	for _, actual := range []map[string]bool{{"tun0": true, "tun1": true}, {"wg0": true}, {"wg0": true, "tun7": false}, {"wg0": true, "tun7": true, "unexpected": true}} {
		if vpnsHealthy(actual, []string{"wg0", "tun7"}) {
			t.Fatal("mismatched or unhealthy VPN set accepted")
		}
	}
}

func TestGuardHydratesCallsAbsentFromObjectManager(t *testing.T) {
	current := dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/1")
	ended := dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/0")
	all := managedObjects{"/org/freedesktop/ModemManager1/Modem/0": {modemIF: {"DeviceIdentifier": dbus.MakeVariant("selected")}, voiceIF: {"Calls": dbus.MakeVariant([]dbus.ObjectPath{current, ended})}}}
	states := map[dbus.ObjectPath]int32{current: 4, ended: 7}
	read := func(p dbus.ObjectPath) (map[string]dbus.Variant, error) {
		return map[string]dbus.Variant{"State": dbus.MakeVariant(states[p])}, nil
	}
	if e := hydrateCalls(all, "selected", read); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f, _ := json.Marshal(map[string]any{"updated": now.Format(time.RFC3339), "active": "wired", "wired_healthy": true, "cellular_healthy": true, "cellular_present": true, "vpns": map[string]bool{"tun0": true, "tun1": true, "tun2": true}})
	if _, e := readState(all, all[current][callIF], f, "selected", current, now); e != nil {
		t.Fatal(e)
	}
	states[ended] = 4
	if e := hydrateCalls(all, "selected", read); e != nil {
		t.Fatal(e)
	}
	if _, e := readState(all, all[current][callIF], f, "selected", current, now); e == nil {
		t.Fatal("other active call accepted")
	}
	if e := hydrateCalls(all, "selected", func(dbus.ObjectPath) (map[string]dbus.Variant, error) { return nil, errors.New("removed") }); e == nil {
		t.Fatal("removed call accepted")
	}
}

func TestReadOnlyModemGuardPinsCallOwnershipAndFreshFailover(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/42")
	objects := managedObjects{"/org/freedesktop/ModemManager1/Modem/0": {
		modemIF: {"DeviceIdentifier": dbus.MakeVariant("selected")}, voiceIF: {"Calls": dbus.MakeVariant([]dbus.ObjectPath{path})}}}
	call := map[string]dbus.Variant{"State": dbus.MakeVariant(int32(4))}
	f, _ := json.Marshal(map[string]any{"updated": now.Format("2006-01-02T15:04:05-0700"), "active": "wired", "wired_healthy": true, "cellular_healthy": true, "cellular_present": true, "vpns": map[string]bool{"tun0": true, "tun1": true, "tun2": true}})
	s, err := readState(objects, call, f, "selected", path, now)
	if err != nil || CheckState(s, "selected", string(path), now) != nil {
		t.Fatalf("valid guard failed: %v %+v", err, s)
	}
	if _, err = readState(objects, call, f, "other", path, now); err == nil {
		t.Fatal("other modem accepted")
	}
	if _, err = readState(objects, call, f, "selected", "/org/freedesktop/ModemManager1/Call/99", now); err == nil {
		t.Fatal("other call accepted")
	}
	call["State"] = dbus.MakeVariant(int32(7))
	s, err = readState(objects, call, f, "selected", path, now)
	if err != nil {
		t.Fatal(err)
	}
	if CheckState(s, "selected", string(path), now) == nil {
		t.Fatal("ended call accepted")
	}
	if _, err = readState(objects, call, []byte(`{"updated":"not-a-time"}`), "selected", path, now); err == nil {
		t.Fatal("invalid failover accepted")
	}
}
