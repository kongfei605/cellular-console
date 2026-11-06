package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func testApp(t *testing.T) (*App, *DemoBackend, string) {
	t.Helper()
	dir := t.TempDir()
	inbox, err := openInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "failover.json")
	writeFailover(t, f, demoFailover())
	b := &DemoBackend{calls: []Call{{ID: "900", Number: "13800000000", Direction: "incoming", StateCode: 3, State: "来电"}}}
	a := newApp(b, inbox, credentials{"admin", "test-password-12345"}, f, false)
	a.poll(context.Background())
	return a, b, f
}
func writeFailover(t *testing.T, path string, f Failover) {
	t.Helper()
	b, _ := json.Marshal(f)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func request(a *App, method, path, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://console.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	return w
}
func login(t *testing.T, a *App) (*http.Cookie, string) {
	t.Helper()
	r := request(a, "POST", "/api/login", `{"username":"admin","password":"test-password-12345"}`, nil, "", "http://console.test")
	if r.Code != 200 {
		t.Fatalf("login status %d", r.Code)
	}
	var v map[string]string
	json.Unmarshal(r.Body.Bytes(), &v)
	c := r.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	return c, v["csrf"]
}

func TestAuthenticationAndCSRF(t *testing.T) {
	a, _, _ := testApp(t)
	for _, path := range []string{"/api/status", "/api/messages", "/api/session"} {
		if r := request(a, "GET", path, "", nil, "", ""); r.Code != 401 {
			t.Fatalf("%s leaked without auth", path)
		}
	}
	c, csrf := login(t, a)
	if r := request(a, "GET", "/api/messages", "", c, "", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if r := request(a, "POST", "/api/calls/900/accept", `{}`, c, "", ""); r.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if r := request(a, "POST", "/api/calls/900/accept", `{}`, c, csrf, "http://evil.test"); r.Code != 403 {
		t.Fatal("cross-origin accepted")
	}
	if r := request(a, "POST", "/api/calls/900/accept", `{}`, c, csrf, "http://console.test"); r.Code != 200 {
		t.Fatalf("answer failed: %d %s", r.Code, r.Body)
	}
	if a.state().Calls[0].StateCode != 4 {
		t.Fatal("answer did not update state")
	}
	if r := request(a, "POST", "/api/calls/900/hangup", `{}`, c, csrf, ""); r.Code != 200 {
		t.Fatal("hangup failed")
	}
	if r := request(a, "POST", "/api/logout", `{}`, c, csrf, ""); r.Code != 200 {
		t.Fatal("logout failed")
	}
	if r := request(a, "GET", "/api/status", "", c, "", ""); r.Code != 401 {
		t.Fatal("logged-out session remained valid")
	}
}

func TestFailoverGuardUsesFreshState(t *testing.T) {
	a, b, path := testApp(t)
	c, csrf := login(t, a)
	// The UI snapshot still says wired; action authorization must reread disk.
	f := demoFailover()
	f.Active = "cellular"
	writeFailover(t, path, f)
	if r := request(a, "POST", "/api/calls", `{"number":"13800000000"}`, c, csrf, ""); r.Code != 409 {
		t.Fatalf("cellular protection bypassed: %d", r.Code)
	}
	if r := request(a, "POST", "/api/calls/900/accept", `{}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("answer protection bypassed")
	}
	if b.serial != 0 {
		t.Fatal("dial reached backend despite protection")
	}
	if r := request(a, "POST", "/api/calls/900/hangup", `{}`, c, csrf, ""); r.Code != 200 {
		t.Fatal("hangup must remain available on cellular")
	}
	f = demoFailover()
	f.Updated = time.Now().Add(-time.Minute).Format("2006-01-02T15:04:05-0700")
	writeFailover(t, path, f)
	if r := request(a, "POST", "/api/calls", `{"number":"13800000000"}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("stale state permitted dial")
	}
	os.Remove(path)
	if r := request(a, "POST", "/api/calls", `{"number":"13800000000"}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("missing state permitted dial")
	}
}

func TestDialValidationAndSimulatedLifecycle(t *testing.T) {
	a, b, _ := testApp(t)
	c, csrf := login(t, a)
	for _, n := range []string{"112", "13800000000;ATH", "+1234567\rAT", "../../../1", ""} {
		body, _ := json.Marshal(map[string]string{"number": n})
		if r := request(a, "POST", "/api/calls", string(body), c, csrf, ""); r.Code != 400 {
			t.Fatalf("invalid input accepted: %q", n)
		}
	}
	if b.serial != 0 {
		t.Fatal("invalid number reached hardware backend")
	}
	r := request(a, "POST", "/api/calls", `{"number":"+86 138-0000-0000"}`, c, csrf, "")
	if r.Code != 200 {
		t.Fatalf("simulated dial failed %s", r.Body)
	}
	if b.calls[0].Number != "+8613800000000" {
		t.Fatal("number not normalized")
	}
	if r := request(a, "POST", "/api/calls/1/hangup", `{}`, c, csrf, ""); r.Code != 200 {
		t.Fatal("simulated hangup failed")
	}
	if b.calls[0].StateCode != 7 {
		t.Fatal("simulated call remained active")
	}
}

func TestInboxPersistsAndDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	s, _ := openInbox(dir)
	m := Message{Sender: "10010", Text: "测试 <script>alert(1)</script> 短信", Timestamp: "2026-11-06T10:00:00+08:00", Source: "/SMS/1"}
	if n, e := s.Merge("device-a", []Message{m}); n != 1 || e != nil {
		t.Fatal(n, e)
	}
	id := s.List()[0].ID
	if e := s.MarkRead(id); e != nil {
		t.Fatal(e)
	}
	m.Source = "/SMS/42"
	if n, e := s.Merge("device-a", []Message{m}); n != 0 || e != nil {
		t.Fatal("hotplug duplicated archived SMS", n, e)
	}
	reopened, e := openInbox(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(reopened.List()) != 1 || !reopened.List()[0].Read {
		t.Fatal("read state or message lost on restart")
	}
	if n, e := s.Merge("device-b", []Message{m}); n != 1 || e != nil {
		t.Fatal("different modem SMS deduplicated incorrectly")
	}
	info, _ := os.Stat(filepath.Join(dir, "inbox.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("inbox permissions too broad")
	}
	os.WriteFile(filepath.Join(dir, "inbox.json"), []byte("not valid JSON"), 0600)
	if _, e := openInbox(dir); e == nil {
		t.Fatal("corrupt archive silently overwritten")
	}
}

func v(x any) dbus.Variant { return dbus.MakeVariant(x) }
func TestObjectParsingIsolatesSelectedModem(t *testing.T) {
	mp := dbus.ObjectPath("/org/freedesktop/ModemManager1/Modem/2")
	sp := dbus.ObjectPath("/org/freedesktop/ModemManager1/SMS/4")
	cp := dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/9")
	all := objects{mp: {modemIF: {"DeviceIdentifier": v("expected"), "Model": v("EC20F"), "State": v(int32(11)), "UnlockRequired": v(uint32(1)), "SignalQuality": v([]interface{}{uint32(80), true}), "AccessTechnologies": v(uint32(16384))}, modemIF + ".Modem3gpp": {"OperatorName": v("CHN-UNICOM")}, modemIF + ".Messaging": {"Messages": v([]dbus.ObjectPath{sp})}, voiceIF: {"Calls": v([]dbus.ObjectPath{cp}), "EmergencyOnly": v(false)}},
		sp:                                       {smsIF: {"State": v(uint32(3)), "PduType": v(uint32(1)), "Number": v("10010"), "Text": v("测试"), "Timestamp": v("2026-11-06T10:00:00+08:00")}},
		cp:                                       {callIF: {"State": v(int32(3)), "Direction": v(int32(1)), "Number": v("13800000000")}},
		"/org/freedesktop/ModemManager1/SMS/999": {smsIF: {"State": v(uint32(3)), "PduType": v(uint32(1)), "Text": v("other modem private SMS")}},
		"/org/freedesktop/ModemManager1/Call/999": {callIF: {"State": v(int32(4)), "Number": v("other modem")}},
	}
	_, radio, m, c, err := parseObjects(all, "expected")
	if err != nil || !radio.Present || radio.Signal != 80 || radio.Operator != "CHN-UNICOM" || !radio.SIMReady {
		t.Fatal("radio parsing failed", radio, err)
	}
	if len(m) != 1 || m[0].Text != "测试" || len(c) != 1 || c[0].ID != "9" {
		t.Fatal("unrelated modem objects included")
	}
	_, radio, m, c, err = parseObjects(all, "missing")
	if err != nil || radio.Present || len(m) > 0 || len(c) > 0 {
		t.Fatal("wrong modem selected")
	}
}

func TestRateLimitAndBodyLimit(t *testing.T) {
	a, _, _ := testApp(t)
	for range 8 {
		request(a, "POST", "/api/login", `{"username":"admin","password":"wrong"}`, nil, "", "")
	}
	if r := request(a, "POST", "/api/login", `{"username":"admin","password":"test-password-12345"}`, nil, "", ""); r.Code != 429 {
		t.Fatal("login limit not enforced")
	}
	a, _, _ = testApp(t)
	r := request(a, "POST", "/api/login", string(bytes.Repeat([]byte("x"), 5000)), nil, "", "")
	if r.Code != 400 {
		t.Fatal("oversized body accepted")
	}
}

func TestStandaloneSMSAndCallPropertiesAreLoaded(t *testing.T) {
	mp := dbus.ObjectPath("/org/freedesktop/ModemManager1/Modem/2")
	sp := dbus.ObjectPath("/org/freedesktop/ModemManager1/SMS/4")
	cp := dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/0")
	all := objects{mp: {modemIF: {"DeviceIdentifier": v("expected"), "Model": v("EC20F")}, modemIF + ".Messaging": {"Messages": v([]dbus.ObjectPath{sp})}, voiceIF: {"Calls": v([]dbus.ObjectPath{cp})}}}
	fetched := map[dbus.ObjectPath]bool{}
	err := hydrateChildren(all, mp, func(path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
		fetched[path] = true
		if path == sp && iface == smsIF {
			return map[string]dbus.Variant{"State": v(uint32(3)), "PduType": v(uint32(1)), "Text": v("独立短信对象"), "Number": v("10010")}, nil
		}
		if path == cp && iface == callIF {
			return map[string]dbus.Variant{"State": v(int32(2)), "Direction": v(int32(2)), "Number": v("13800000000")}, nil
		}
		t.Fatal("unrelated object fetched")
		return nil, nil
	})
	if err != nil || len(fetched) != 2 {
		t.Fatal(err, fetched)
	}
	_, _, m, c, err := parseObjects(all, "expected")
	if err != nil || len(m) != 1 || len(c) != 1 || c[0].StateCode != 2 {
		t.Fatal("standalone objects missing", err)
	}
}
