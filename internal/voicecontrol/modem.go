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
	found := 0
	for _, ifaces := range all {
		p := ifaces[modemIF]
		if stringValue(p, "DeviceIdentifier") != m.DeviceID {
			continue
		}
		found++
		s.DeviceID = m.DeviceID
		s.Model = stringValue(p, "Model")
		s.Firmware = stringValue(p, "Revision")
		s.Primary = stringValue(p, "PrimaryPort")
		paths, _ := ifaces[modemIF+".Voice"]["Calls"].Value().([]dbus.ObjectPath)
		for _, path := range paths {
			code, ok := all[path][destination+".Call"]["State"].Value().(int32)
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
