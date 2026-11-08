package voicecontrol

import (
	"context"
	"errors"
	"github.com/godbus/dbus/v5"
)

const destination = "org.freedesktop.ModemManager1"
const modemIF = destination + ".Modem"

type objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
type Modem struct {
	Conn     *dbus.Conn
	DeviceID string
	Check    func() error
}

func stringValue(p map[string]dbus.Variant, k string) string { v, _ := p[k].Value().(string); return v }
func (m *Modem) Read(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	s.Calls = map[string]int32{}
	if e := m.Conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, destination).Store(&s.Owner); e != nil {
		return s, e
	}
	var all objects
	if e := m.Conn.Object(s.Owner, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", dbus.FlagNoAutoStart).Store(&all); e != nil {
		return s, e
	}
	return snapshot(all, m.DeviceID, s.Owner, func(path dbus.ObjectPath) (map[string]dbus.Variant, error) {
		var p map[string]dbus.Variant
		e := m.Conn.Object(s.Owner, path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, destination+".Call").Store(&p)
		return p, e
	})
}

// Calls are referenced by the modem, but MM need not export them through its
// ObjectManager. Read their properties from the same unique bus owner.
func snapshot(all objects, deviceID, owner string, readCall func(dbus.ObjectPath) (map[string]dbus.Variant, error)) (Snapshot, error) {
	s := Snapshot{Owner: owner, Calls: map[string]int32{}}
	found := 0
	for _, ifaces := range all {
		p := ifaces[modemIF]
		if stringValue(p, "DeviceIdentifier") != deviceID {
			continue
		}
		found++
		s.DeviceID = deviceID
		s.Model = stringValue(p, "Model")
		s.Firmware = stringValue(p, "Revision")
		s.Primary = stringValue(p, "PrimaryPort")
		paths, ok := ifaces[modemIF+".Voice"]["Calls"].Value().([]dbus.ObjectPath)
		if !ok {
			return s, errors.New("call list unavailable")
		}
		for _, path := range paths {
			if !CallPath.MatchString(string(path)) {
				return s, errors.New("invalid call path")
			}
			properties, err := readCall(path)
			if err != nil {
				return s, err
			}
			code, ok := properties["State"].Value().(int32)
			if !ok {
				return s, errors.New("call state unavailable")
			}
			s.Calls[string(path)] = code
		}
	}
	if found != 1 {
		return s, errors.New("expected modem unavailable")
	}
	return s, nil
}
