package voicecontrol

import (
	"errors"
	"github.com/godbus/dbus/v5"
	"testing"
)

func TestReferencedCallsNeedNotBeManagedObjects(t *testing.T) {
	path := dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/0")
	all := objects{"/org/freedesktop/ModemManager1/Modem/0": {modemIF: {"DeviceIdentifier": dbus.MakeVariant("selected"), "Model": dbus.MakeVariant("EC20F"), "Revision": dbus.MakeVariant("EC20CEFAGR08A03M4G")}, modemIF + ".Voice": {"Calls": dbus.MakeVariant([]dbus.ObjectPath{path})}}}
	for _, state := range []int32{4, 7} {
		s, e := snapshot(all, "selected", ":1.42", func(p dbus.ObjectPath) (map[string]dbus.Variant, error) {
			if p != path {
				t.Fatal(p)
			}
			return map[string]dbus.Variant{"State": dbus.MakeVariant(state)}, nil
		})
		if e != nil || s.Calls[string(path)] != state {
			t.Fatalf("state %d: %+v %v", state, s, e)
		}
		e = Validate(s, Request{Phase: "dial"}, "selected")
		if (e == nil) != (state == 7) {
			t.Fatalf("ended calls should permit redial; active calls must block it: %v", e)
		}
	}
	if _, e := snapshot(all, "selected", ":1.42", func(dbus.ObjectPath) (map[string]dbus.Variant, error) { return nil, errors.New("removed") }); e == nil {
		t.Fatal("missing call silently accepted")
	}
}
