package audioloop

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTLSLoginOriginCSRFAndCookieIsolation(t *testing.T) {
	m := testMedia(t)
	s := NewServer(m, "", Credentials{Username: "admin", Password: "test-only-password-2026"})
	ts := httptest.NewTLSServer(s.Handler())
	defer ts.Close()
	s.origin = ts.URL
	client := ts.Client()
	client.Jar, _ = cookiejar.New(nil)
	request := func(path, method, origin, csrf, body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("X-CSRF-Token", csrf)
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := request("/api/loopback", "GET", "", "", "")
	if res.StatusCode != 401 {
		t.Fatal("unauthenticated media")
	}
	res.Body.Close()
	body := `{"username":"admin","password":"test-only-password-2026"}`
	res = request("/api/login", "POST", "https://evil.invalid", "", body)
	if res.StatusCode != 403 {
		t.Fatal("cross-origin login")
	}
	res.Body.Close()
	res = request("/api/login", "POST", ts.URL, "", body)
	if res.StatusCode != 200 {
		t.Fatal("login failed")
	}
	var v struct {
		CSRF string `json:"csrf"`
	}
	json.NewDecoder(res.Body).Decode(&v)
	res.Body.Close()
	if len(v.CSRF) < 32 {
		t.Fatal("missing CSRF")
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatal("unsafe cookie")
	}
	for _, tc := range []struct{ origin, csrf string }{{ts.URL, ""}, {"https://evil.invalid", v.CSRF}} {
		res = request("/api/loopback/stop", "POST", tc.origin, tc.csrf, `{"id":"none"}`)
		if res.StatusCode != 403 {
			t.Fatal("CSRF accepted")
		}
		res.Body.Close()
	}
	res = request("/api/loopback/stop", "POST", ts.URL, v.CSRF, `{"id":"none"}`)
	if res.StatusCode != 200 {
		t.Fatal("valid stop rejected")
	}
	res.Body.Close()
	res = request("/api/loopback", "POST", ts.URL, v.CSRF, `{"type":"offer","sdp":"bad"}`)
	if res.StatusCode != 400 {
		t.Fatal("bad SDP accepted")
	}
	res.Body.Close()
	res = request("/api/logout", "POST", ts.URL, v.CSRF, `{}`)
	if res.StatusCode != 200 {
		t.Fatal("logout failed")
	}
	res.Body.Close()
	res = request("/api/session", "GET", "", "", "")
	if res.StatusCode != 401 {
		t.Fatal("logout retained session")
	}
	res.Body.Close()
	r := httptest.NewRequest("GET", "http://example/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("plaintext accepted")
	}
	r = httptest.NewRequest("GET", ts.URL+"/healthz", nil)
	r.Host = "wrong.invalid"
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 421 {
		t.Fatalf("wrong host accepted: %d", w.Code)
	}
}

func TestLoginRateLimitAndExpiredSession(t *testing.T) {
	m := testMedia(t)
	s := NewServer(m, "https://audio.test", Credentials{Username: "admin", Password: "test-only-password-2026"})
	for i := 0; i < 9; i++ {
		r := httptest.NewRequest("POST", "https://audio.test/api/login", bytes.NewBufferString(`{"username":"admin","password":"wrong"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", s.origin)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		want := 401
		if i == 8 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d got %d", i, w.Code)
		}
	}
	s.sessions["expired"] = session{CSRF: "x", Expires: time.Now().Add(-time.Hour)}
	r := httptest.NewRequest("GET", "https://audio.test/api/session", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "expired"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestStaticSecurityHeaders(t *testing.T) {
	s := NewServer(testMedia(t), "https://audio.test", Credentials{})
	r := httptest.NewRequest("GET", "https://audio.test/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	if w.Code != 200 || !strings.Contains(string(b), "运行 440Hz 测试音") {
		t.Fatal("UI missing")
	}
	if !strings.Contains(w.Header().Get("Permissions-Policy"), "microphone=(self)") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("security policy missing")
	}
}
