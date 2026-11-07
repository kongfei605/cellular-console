package gnsscontrol

import (
	"context"
	"errors"
	"github.com/godbus/dbus/v5"
)

const dest = "org.freedesktop.ModemManager1"
const mif = dest + ".Modem"
const lif = mif + ".Location"

type Objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
type Modem struct {
	Conn     *dbus.Conn
	DeviceID string
}

func str(p map[string]dbus.Variant, k string) string    { v, _ := p[k].Value().(string); return v }
func number(p map[string]dbus.Variant, k string) uint32 { v, _ := p[k].Value().(uint32); return v }
func flag(p map[string]dbus.Variant, k string) bool     { v, _ := p[k].Value().(bool); return v }
func (m *Modem) Read(ctx context.Context) (State, error) {
	var s State
	if e := m.Conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, dest).Store(&s.Owner); e != nil {
		return s, e
	}
	var all Objects
	if e := m.Conn.Object(s.Owner, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", dbus.FlagNoAutoStart).Store(&all); e != nil {
		return s, e
	}
	for path, ifaces := range all {
		if p := ifaces[mif]; p != nil && str(p, "DeviceIdentifier") == m.DeviceID {
			l := ifaces[lif]
			s.Path = string(path)
			s.DeviceID = m.DeviceID
			s.Model = str(p, "Model")
			s.Firmware = str(p, "Revision")
			s.Sources = number(l, "Enabled")
			s.Capabilities = number(l, "Capabilities")
			s.Signals = flag(l, "SignalsLocation")
		}
		if p := ifaces[mif+".Voice.Call"]; p != nil && number(p, "State") != 7 {
			s.CallActive = true
		}
		if p := ifaces[dest+".Call"]; p != nil && number(p, "State") != 7 {
			s.CallActive = true
		}
	}
	if s.Path == "" {
		return s, errors.New("expected modem absent")
	}
	var props map[string]dbus.Variant
	if e := m.Conn.Object(s.Owner, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, dest).Store(&props); e != nil {
		return s, e
	}
	s.Version = str(props, "Version")
	return s, nil
}
func (m *Modem) Setup(ctx context.Context, s State, mask uint32) error {
	// Caller selects a previously reviewed source mask. No other D-Bus mutator is exposed.
	if (mask != 1 && mask != 7) || s.Signals {
		return errors.New("invalid GNSS target")
	}
	current, e := m.Read(ctx)
	if e != nil {
		return e
	}
	if current.Owner != s.Owner || current.Path != s.Path || current.DeviceID != s.DeviceID || current.Sources != s.Sources || current.Signals != s.Signals {
		return errors.New("location owner or configuration changed")
	}
	if mask == 7 && current.CallActive {
		return errors.New("call became active")
	}
	return m.Conn.Object(s.Owner, dbus.ObjectPath(s.Path)).CallWithContext(ctx, lif+".Setup", dbus.FlagNoAutoStart, mask, false).Err
}
