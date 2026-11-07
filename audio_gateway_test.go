package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPSAudioGatewayUnifiedSessionAndIsolation(t *testing.T) {
	key := strings.Repeat("a", 64)
	var calls atomic.Int32
	var revoked atomic.Bool
	var owner atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-EC20-Gateway-Key") != key {
			t.Error("wrong key")
		}
		id := r.Header.Get("X-EC20-Owner")
		if id == "spoofed" || len(id) != 64 {
			t.Error("caller identity forwarded")
		}
		owner.Store(id)
		if r.URL.Path == "/internal/revoke" {
			revoked.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"active":false}`)
	}))
	defer backend.Close()
	a, _, _ := testApp(t)
	a.audio, _ = newAudioProxy(backend.URL, key)
	a.secure = true
	ts := httptest.NewTLSServer(a.handler())
	defer ts.Close()
	a.publicOrigin = ts.URL
	client := ts.Client()
	client.Jar, _ = cookiejar.New(nil)
	req := func(method, path, origin, csrf, body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("X-EC20-Owner", "spoofed")
		r.Header.Set("X-EC20-Gateway-Key", "spoofed")
		res, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	check := func(res *http.Response, code int) {
		t.Helper()
		defer res.Body.Close()
		if res.StatusCode != code {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("status %d expected %d: %s", res.StatusCode, code, b)
		}
	}
	check(req("GET", "/api/audio/status", "", "", ""), 401)
	if calls.Load() != 0 {
		t.Fatal("unauth reached backend")
	}
	check(req("POST", "/api/login", "http://"+strings.TrimPrefix(ts.URL, "https://"), "", `{"username":"admin","password":"test-password-12345"}`), 403)
	res := req("POST", "/api/login", ts.URL, "", `{"username":"admin","password":"test-password-12345"}`)
	var loginData struct {
		CSRF string `json:"csrf"`
	}
	json.NewDecoder(res.Body).Decode(&loginData)
	res.Body.Close()
	cookies := res.Cookies()
	if res.StatusCode != 200 || len(cookies) != 1 || cookies[0].Name != "__Host-ec20_session" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unified secure cookie missing")
	}
	check(req("GET", "/api/audio/status", "", "", ""), 200)
	if owner.Load() != cookies[0].Value {
		t.Fatal("audio owner differs from login")
	}
	check(req("POST", "/api/audio/offer", ts.URL, "forged", `{}`), 403)
	check(req("POST", "/api/audio/offer", "https://evil.invalid", loginData.CSRF, `{}`), 403)
	before := calls.Load()
	check(req("POST", "/api/audio/offer", ts.URL, loginData.CSRF, strings.Repeat("x", 65537)), 413)
	if calls.Load() != before {
		t.Fatal("oversized SDP forwarded")
	}
	check(req("POST", "/api/audio/offer", ts.URL, loginData.CSRF, `{}`), 200)
	check(req("GET", "/api/loopback", "", "", ""), 200)
	check(req("POST", "/api/logout", ts.URL, loginData.CSRF, `{}`), 200)
	if !revoked.Load() {
		t.Fatal("logout did not revoke audio")
	}
	check(req("GET", "/api/audio/status", "", "", ""), 401)
	// Backend failure affects only audio; console data stays available.
	res = req("POST", "/api/login", ts.URL, "", `{"username":"admin","password":"test-password-12345"}`)
	res.Body.Close()
	backend.Close()
	check(req("GET", "/api/audio/status", "", "", ""), 503)
	check(req("GET", "/api/status", "", "", ""), 200)
}
func TestLegacyHTTPRedirectAndPrivateBackendValidation(t *testing.T) {
	handler := canonicalRedirect("https://192.0.2.10:8788")
	for _, path := range []string{"/", "/audio/", "/?q=test", "/api/status"} {
		r := httptest.NewRequest("GET", "http://evil.invalid"+path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 308 || w.Header().Get("Location") != "https://192.0.2.10:8788"+path {
			t.Fatal("unsafe or broken redirect")
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "http://console.test/api/login", bytes.NewBufferString(`{}`)))
	if w.Code != 426 || w.Header().Get("Location") != "" {
		t.Fatal("plaintext mutation accepted")
	}
	for _, base := range []string{"http://evil.invalid:8790", "http://192.0.2.10:8790", "http://127.0.0.1:8790/api", "https://127.0.0.1:8790", "http://user@127.0.0.1:8790"} {
		if _, e := newAudioProxy(base, strings.Repeat("a", 64)); e == nil {
			t.Fatal("non-private upstream accepted")
		}
	}
	a, _, _ := testApp(t)
	c, csrf := login(t, a)
	a.mu.Lock()
	a.sessions[c.Value] = session{CSRF: csrf, Expires: time.Now().Add(-time.Second)}
	a.mu.Unlock()
	if request(a, "GET", "/api/audio/status", "", c, "", "").Code != 401 {
		t.Fatal("expired session accepted")
	}
}
