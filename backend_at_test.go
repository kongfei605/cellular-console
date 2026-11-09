package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kongfei605/cellular-console/internal/atmodem"
	"github.com/kongfei605/cellular-console/internal/hostconfig"
)

type fakeATSession struct {
	alive     bool
	calls     []string
	submitted int
	command   func(string, []byte) ([]string, error)
}

func (s *fakeATSession) Alive() bool  { return s.alive }
func (s *fakeATSession) Close() error { s.alive = false; return nil }
func (s *fakeATSession) Command(_ context.Context, cmd string, payload []byte) ([]string, error) {
	s.calls = append(s.calls, cmd)
	if strings.HasPrefix(cmd, "AT+CMGS=") {
		s.submitted++
	}
	return s.command(cmd, payload)
}
func atFixture(t *testing.T) (*ATBackend, *fakeATSession, *atmodem.Device, *[]string) {
	t.Helper()
	device := &atmodem.Device{Location: "01110000", Session: "synthetic-session", Network: "en99"}
	clcc := []string{}
	s := &fakeATSession{alive: true}
	s.command = func(cmd string, payload []byte) ([]string, error) {
		switch cmd {
		case "AT", "AT+CMGF=0", "ATA", "AT+CHUP":
			return nil, nil
		case "AT+CGMI":
			return []string{"Quectel"}, nil
		case "AT+CGMM":
			return []string{"EC20F"}, nil
		case "AT+CGMR":
			return []string{hostconfig.ReviewedFirmware}, nil
		case "AT+CGSN":
			return []string{"synthetic-modem-id"}, nil
		case "AT+CPIN?":
			return []string{"+CPIN: READY"}, nil
		case "AT+CEREG?":
			return []string{"+CEREG: 0,1"}, nil
		case "AT+CSQ":
			return []string{"+CSQ: 31,99"}, nil
		case "AT+COPS?":
			return []string{`+COPS: 0,0,"Example Carrier",7`}, nil
		case "AT+CLCC":
			return clcc, nil
		case "AT+CMGL=4":
			return []string{`+CMGL: 0,0,,26`, "00000B912120550521F300004211902143650005E8329BFD06"}, nil
		case "AT+QGPS?":
			return []string{"+QGPS: 0"}, nil
		}
		if strings.HasPrefix(cmd, "AT+CMGS=") && len(payload) > 0 {
			return []string{"+CMGS: 42"}, nil
		}
		return nil, errors.New("unexpected synthetic AT command")
	}
	b := newATBackend("unused", "", "")
	b.discover = func(context.Context) ([]atmodem.Device, error) { return []atmodem.Device{*device}, nil }
	b.open = func(string) (atSession, error) { s.alive = true; return s, nil }
	t.Cleanup(b.Close)
	return b, s, device, &clcc
}
func TestATSnapshotReadsRealProtocolWithoutDeletingMessages(t *testing.T) {
	b, s, _, _ := atFixture(t)
	r, m, c, err := b.Snapshot(context.Background())
	if err != nil || !r.Present || !r.SIMReady || r.Signal != 100 || !r.Voice || !r.Messaging || len(m) != 1 || m[0].Text != "hello" || len(c) != 0 {
		t.Fatal(r, len(m), c, err)
	}
	for _, cmd := range s.calls {
		if strings.HasPrefix(cmd, "AT+CMGD") || strings.HasPrefix(cmd, "ATD") {
			t.Fatal("snapshot mutated modem", cmd)
		}
	}
}
func TestATCallHandlesCannotActOnReusedCLCCIndex(t *testing.T) {
	b, s, _, clcc := atFixture(t)
	ctx := context.Background()
	*clcc = []string{`+CLCC: 1,1,4,0,0,"12025550123",129`}
	_, _, calls, err := b.Snapshot(ctx)
	if err != nil || len(calls) != 1 {
		t.Fatal(calls, err)
	}
	old := calls[0].ID
	*clcc = nil
	if _, _, _, err = b.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	*clcc = []string{`+CLCC: 1,1,4,0,0,"12025550123",129`}
	if err = b.Act(ctx, old, "accept"); err == nil {
		t.Fatal("stale handle accepted a new call")
	}
	for _, cmd := range s.calls {
		if cmd == "ATA" {
			t.Fatal("stale handle reached modem")
		}
	}
	current := b.calls[1].ID
	if current == old {
		t.Fatal("call generation reused")
	}
	if err = b.Act(ctx, current, "hangup"); err != nil || s.calls[len(s.calls)-1] != "AT+CHUP" {
		t.Fatal(err, s.calls)
	}
}
func TestATSMSSendsOnceAndDoesNotRetryAfterUSBChange(t *testing.T) {
	b, s, device, _ := atFixture(t)
	ctx := context.Background()
	ref, err := b.PrepareSMS(ctx, "12025550123", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SubmitSMS(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if state, err := b.ReadSMSState(ctx, ref); err != nil || state != smsStateSent {
		t.Fatal(state, err)
	}
	if err = b.SubmitSMS(ctx, ref); err == nil || s.submitted != 1 {
		t.Fatal("duplicate SMS submitted", err, s.submitted)
	}
	device.Session = "new-synthetic-session"
	if err = b.SubmitSMS(ctx, ref); err == nil || s.submitted != 1 {
		t.Fatal("USB reconnect retried old SMS", err)
	}
}
func TestATSMSTimeoutRemainsUnknown(t *testing.T) {
	b, s, _, _ := atFixture(t)
	ctx := context.Background()
	ref, err := b.PrepareSMS(ctx, "12025550123", "hello")
	if err != nil {
		t.Fatal(err)
	}
	original := s.command
	s.command = func(cmd string, p []byte) ([]string, error) {
		if strings.HasPrefix(cmd, "AT+CMGS=") {
			return nil, context.DeadlineExceeded
		}
		return original(cmd, p)
	}
	if err = b.SubmitSMS(ctx, ref); err == nil {
		t.Fatal("timed out submission accepted")
	}
	if _, err = b.ReadSMSState(ctx, ref); err == nil {
		t.Fatal("unconfirmed submission marked sent")
	}
	if err = b.SubmitSMS(ctx, ref); err == nil || s.submitted != 1 {
		t.Fatal("ambiguous submission retried")
	}
}
func TestATUnknownFirmwareBlocksActionsAndMultipleDevicesFailClosed(t *testing.T) {
	b, s, device, _ := atFixture(t)
	original := s.command
	s.command = func(cmd string, p []byte) ([]string, error) {
		if cmd == "AT+CGMR" {
			return []string{"unknown-firmware"}, nil
		}
		return original(cmd, p)
	}
	if _, err := b.Dial(context.Background(), "12025550123"); err == nil {
		t.Fatal("unknown firmware dialed")
	}
	if _, err := b.PrepareSMS(context.Background(), "12025550123", "hello"); err == nil {
		t.Fatal("unknown firmware sent SMS")
	}
	b.Close()
	b.location = ""
	b.discover = func(context.Context) ([]atmodem.Device, error) {
		second := *device
		second.Location = "02220000"
		return []atmodem.Device{*device, second}, nil
	}
	if _, _, _, err := b.Snapshot(context.Background()); err == nil {
		t.Fatal("ambiguous device accepted")
	}
}

func TestATGNSSPreservesSourceAndConfirmsState(t *testing.T) {
	b, s, _, clcc := atFixture(t)
	ctx := context.Background()
	original := s.command
	on := false
	s.command = func(cmd string, p []byte) ([]string, error) {
		switch cmd {
		case "AT+QGPS?":
			if on {
				return []string{"+QGPS: 1"}, nil
			}
			return []string{"+QGPS: 0"}, nil
		case `AT+QGPSCFG="nmeasrc"`:
			return []string{`+QGPSCFG: "nmeasrc",1`}, nil
		case "AT+QGPS=1":
			on = true
			return nil, nil
		case "AT+QGPSEND":
			on = false
			return nil, nil
		}
		if strings.HasPrefix(cmd, "AT+QGPSGNMEA=") {
			return nil, &atmodem.Error{Result: "+CME ERROR: 516"}
		}
		return original(cmd, p)
	}
	state, err := b.ReadLocation(ctx)
	if err != nil || !state.Available || state.GNSSOn {
		t.Fatal(state, err)
	}
	state, err = b.SetGNSS(ctx, true)
	if err != nil || !state.GNSSOn || state.Fix {
		t.Fatal("engine without fix", state, err)
	}
	state, err = b.SetGNSS(ctx, false)
	if err != nil || state.GNSSOn {
		t.Fatal(state, err)
	}
	*clcc = []string{`+CLCC: 1,1,4,0,0,"12025550123",129`}
	if _, err = b.SetGNSS(ctx, true); err == nil || on {
		t.Fatal("GNSS enabled during call")
	}
	for _, cmd := range s.calls {
		if strings.HasPrefix(cmd, `AT+QGPSCFG="nmeasrc",`) {
			t.Fatal("source configuration changed")
		}
	}
}
