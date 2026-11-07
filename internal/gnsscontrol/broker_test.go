package gnsscontrol

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type driverFake struct {
	s           State
	sets        []uint32
	fail        bool
	changeOwner bool
}

func (d *driverFake) Read(context.Context) (State, error) {
	if d.changeOwner && len(d.sets) > 0 {
		v := d.s
		v.Owner = ":new"
		return v, nil
	}
	return d.s, nil
}
func (d *driverFake) Setup(_ context.Context, _ State, mask uint32) error {
	d.sets = append(d.sets, mask)
	if d.fail {
		return errors.New("timeout")
	}
	d.s.Sources = mask
	return nil
}

type guardFake struct {
	bad        bool
	checks     int
	after      bool
	signatures int
}

func (g *guardFake) Check() error {
	g.checks++
	if g.bad || (g.after && g.checks > 1) {
		return errors.New("network unsafe")
	}
	return nil
}
func (g *guardFake) Signature() (string, error) { g.signatures++; return "protected", nil }
func brokerFixture() (*Server, *driverFake, *guardFake) {
	d := &driverFake{s: State{Owner: ":original", Path: "/modem/0", DeviceID: "expected", Model: "EC20F", Firmware: "EC20CEFAGR08A03M4G", Version: "1.20.4", Sources: 1, Capabilities: 23}}
	g := &guardFake{}
	return &Server{Driver: d, Guard: g, DeviceID: "expected"}, d, g
}
func invoke(s *Server, method, path, body string) int {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Code
}
func TestBrokerStrictInterface(t *testing.T) {
	s, d, _ := brokerFixture()
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{{"GET", "/gnss", "", 404}, {"POST", "/reset", `{"enabled":true}`, 404}, {"POST", "/gnss?reset=1", `{"enabled":true}`, 404}, {"POST", "/gnss", `{}`, 400}, {"POST", "/gnss", `{"enabled":true,"command":"AT+CFUN=1,1"}`, 400}, {"POST", "/gnss", `{"enabled":true} {}`, 400}, {"POST", "/gnss", strings.Repeat("x", 200), 400}} {
		if n := invoke(s, tc.method, tc.path, tc.body); n != tc.code {
			t.Fatal(tc, n)
		}
	}
	if len(d.sets) != 0 {
		t.Fatal("invalid input reached MM")
	}
}
func TestBrokerReviewedProfileOnly(t *testing.T) {
	for _, change := range []func(*State){func(s *State) { s.DeviceID = "other" }, func(s *State) { s.Version = "1.22.0" }, func(s *State) { s.Firmware = "other" }, func(s *State) { s.Model = "EC25" }, func(s *State) { s.Sources = 17 }, func(s *State) { s.Sources = 3 }, func(s *State) { s.Signals = true }, func(s *State) { s.Capabilities = 1 }, func(s *State) { s.CallActive = true }} {
		s, d, _ := brokerFixture()
		change(&d.s)
		if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 409 || len(d.sets) != 0 {
			t.Fatal(n, d.sets)
		}
	}
}
func TestBrokerNetworkGuardAndIndependentStop(t *testing.T) {
	s, d, g := brokerFixture()
	g.bad = true
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 409 || len(d.sets) != 0 {
		t.Fatal(n)
	}
	d.s.Sources = 7
	if n := invoke(s, "POST", "/gnss", `{"enabled":false}`); n != 200 || d.s.Sources != 1 {
		t.Fatal(n, d.s)
	}
	if g.checks != 1 {
		t.Fatal("stop incorrectly requires wired network")
	}
}
func TestBrokerEnablePreservesSourceAndDoesNotRetry(t *testing.T) {
	s, d, _ := brokerFixture()
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 200 || d.s.Sources != 7 {
		t.Fatal(n, d.s)
	}
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 200 || len(d.sets) != 1 {
		t.Fatal("idempotent request changes hardware")
	}
	d.s.Sources = 1
	d.fail = true
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 502 || len(d.sets) != 2 {
		t.Fatal("unsafe retry", n, d.sets)
	}
}
func TestBrokerRestoresOnlySameOwnerAfterNetworkFailure(t *testing.T) {
	s, d, g := brokerFixture()
	g.after = true
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 503 || len(d.sets) != 2 || d.s.Sources != 1 {
		t.Fatal(n, d.sets)
	}
	s, d, g = brokerFixture()
	d.changeOwner = true
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 502 || len(d.sets) != 1 {
		t.Fatal("restore targeted new MM", n, d.sets)
	}
}
func TestBrokerConcurrentControlRejected(t *testing.T) {
	s, d, _ := brokerFixture()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := invoke(s, "POST", "/gnss", `{"enabled":true}`); n != 409 || len(d.sets) != 0 {
		t.Fatal(n)
	}
}
