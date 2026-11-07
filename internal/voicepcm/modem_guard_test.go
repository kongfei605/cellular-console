package voicepcm

import (
	"encoding/json"
	"github.com/godbus/dbus/v5"
	"testing"
	"time"
)

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
