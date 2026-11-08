package voicecontrol

import (
	"context"
	"testing"
)

func validSnapshot() Snapshot {
	return Snapshot{Owner: ":1.9", DeviceID: "selected", Model: "EC20F", Firmware: "EC20CEFAGR08A03M4G", Primary: "ttyUSB2", Calls: map[string]int32{}}
}
func TestVoiceOperationPinsProfileCallAndPhase(t *testing.T) {
	s := validSnapshot()
	path := "/org/freedesktop/ModemManager1/Call/42"
	if Validate(s, Request{Phase: "dial"}, "selected") != nil {
		t.Fatal("dial denied")
	}
	for _, phase := range []string{"active", "accept"} {
		if Validate(s, Request{Phase: phase, Call: path}, "selected") == nil {
			t.Fatal("absent call accepted")
		}
	}
	s.Calls[path] = 3
	if Validate(s, Request{Phase: "accept", Call: path}, "selected") != nil {
		t.Fatal("incoming denied")
	}
	if Validate(s, Request{Phase: "active", Call: path}, "selected") == nil {
		t.Fatal("ringing accepted as active")
	}
	s.Calls[path] = 4
	if Validate(s, Request{Phase: "active", Call: path}, "selected") != nil {
		t.Fatal("active denied")
	}
	if Validate(s, Request{Phase: "dial"}, "selected") == nil {
		t.Fatal("second call allowed")
	}
	s.Calls["/org/freedesktop/ModemManager1/Call/43"] = 3
	if Validate(s, Request{Phase: "active", Call: path}, "selected") == nil {
		t.Fatal("another call ignored")
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.Firmware = "unknown" }, func(s *Snapshot) { s.DeviceID = "other" }, func(s *Snapshot) { s.Owner = "" }} {
		v := validSnapshot()
		change(&v)
		if Validate(v, Request{Phase: "dial"}, "selected") == nil {
			t.Fatal("unknown profile")
		}
	}
}

func TestLeasePinsVerifiedUSBGenerationAndBoot(t *testing.T) {
	for _, tc := range []struct {
		raw, fp, boot string
		valid         bool
	}{
		{`{"fingerprint":"usb:42","boot":"boot-a","verified":true}`, "usb:42", "boot-a", true},
		{`{"fingerprint":"usb:42","boot":"boot-a","verified":true}`, "usb:43", "boot-a", false},
		{`{"fingerprint":"usb:42","boot":"boot-a","verified":true}`, "usb:42", "boot-b", false},
		{`{"fingerprint":"usb:42","boot":"boot-a","verified":false}`, "usb:42", "boot-a", false},
		{`{}`, "usb:42", "boot-a", false},
	} {
		e := validateLease([]byte(tc.raw), tc.fp, tc.boot)
		if (e == nil) != tc.valid {
			t.Fatal(tc, e)
		}
	}
	if _, e := (Client{}).Prepare(context.Background(), "AT", ""); e == nil {
		t.Fatal("arbitrary operation")
	}
}
