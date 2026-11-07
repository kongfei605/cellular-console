package audioloop

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrivateGatewayAuthenticationExpiryAndRevocation(t *testing.T) {
	key := strings.Repeat("a", 64)
	s, e := NewGatewayServer(testMedia(t), key)
	if e != nil {
		t.Fatal(e)
	}
	h := s.GatewayHandler()
	owner := strings.Repeat("b", 64)
	run := func(method, path, secret, id string, expires time.Time, remote string) int {
		r := httptest.NewRequest(method, "http://127.0.0.1:8790"+path, strings.NewReader(`{}`))
		r.RemoteAddr = remote
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-EC20-Gateway-Key", secret)
		r.Header.Set("X-EC20-Owner", id)
		r.Header.Set("X-EC20-Expires", strconv.FormatInt(expires.Unix(), 10))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	future := time.Now().Add(time.Hour)
	if run("GET", "/api/loopback", key, owner, future, "127.0.0.1:1234") != 200 {
		t.Fatal("valid gateway request denied")
	}
	for _, tc := range []struct {
		key, owner, remote string
		expiry             time.Time
	}{{"bad", owner, "127.0.0.1:1234", future}, {key, "bad", "127.0.0.1:1234", future}, {key, owner, "192.0.2.10:1234", future}, {key, owner, "127.0.0.1:1234", time.Now().Add(-time.Hour)}, {key, owner, "127.0.0.1:1234", time.Now().Add(9 * time.Hour)}} {
		if run("GET", "/api/loopback", tc.key, tc.owner, tc.expiry, tc.remote) != 401 {
			t.Fatal("invalid gateway identity accepted")
		}
	}
	for _, path := range []string{"/api/login", "/", "/api/session"} {
		if run("POST", path, key, owner, future, "127.0.0.1:1234") != 404 {
			t.Fatal("internal server exposes standalone login/UI")
		}
	}
	if run("POST", "/internal/revoke", key, owner, future, "127.0.0.1:1234") != 200 {
		t.Fatal("revoke failed")
	}
	if run("GET", "/api/loopback", key, owner, future, "127.0.0.1:1234") != 401 {
		t.Fatal("revoked identity accepted")
	}
	other := strings.Repeat("c", 64)
	if run("GET", "/api/loopback", key, other, future, "127.0.0.1:1234") != 200 {
		t.Fatal("revocation leaked to another session")
	}
}
