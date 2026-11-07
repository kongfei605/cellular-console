package audioloop

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var ownerPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// NewGatewayServer has no login/password/TLS-key access. It serves only the
// loopback control channel; the console authenticates and supplies identity.
func NewGatewayServer(media *Media, key string) (*Server, error) {
	if len(key) < 32 {
		return nil, errors.New("gateway key too short")
	}
	s := NewServer(media, "", Credentials{})
	s.delegated = true
	s.gatewayKey = sha256.Sum256([]byte(key))
	s.revoked = map[string]time.Time{}
	return s, nil
}
func (s *Server) gatewayIdentity(r *http.Request) (string, session, bool) {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if e != nil || ip == nil || !ip.IsLoopback() {
		return "", session{}, false
	}
	h := sha256.Sum256([]byte(r.Header.Get("X-EC20-Gateway-Key")))
	if subtle.ConstantTimeCompare(h[:], s.gatewayKey[:]) != 1 {
		return "", session{}, false
	}
	id := r.Header.Get("X-EC20-Owner")
	unix, e := strconv.ParseInt(r.Header.Get("X-EC20-Expires"), 10, 64)
	expires := time.Unix(unix, 0)
	now := time.Now()
	if e != nil || !ownerPattern.MatchString(id) || !expires.After(now) || expires.After(now.Add(8*time.Hour+time.Minute)) {
		return "", session{}, false
	}
	return id, session{Expires: expires}, true
}
func (s *Server) isRevoked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.revoked[id]
	return ok
}
func (s *Server) GatewayHandler() http.Handler {
	api := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		id, v, ok := s.gatewayIdentity(r)
		if !ok {
			failure(w, 401, "private gateway authentication required")
			return
		}
		if r.Method == "POST" && r.URL.Path == "/internal/revoke" {
			s.mu.Lock()
			for owner, expiry := range s.revoked {
				if time.Now().After(expiry) {
					delete(s.revoked, owner)
				}
			}
			if len(s.revoked) >= 4096 {
				s.mu.Unlock()
				failure(w, 503, "revocation storage full")
				return
			}
			s.revoked[id] = v.Expires
			s.mu.Unlock()
			s.media.StopOwner(id)
			jsonResponse(w, 200, map[string]bool{"ok": true})
			return
		}
		if s.isRevoked(id) {
			failure(w, 401, "session revoked")
			return
		}
		if (r.URL.Path != "/api/loopback" || (r.Method != "GET" && r.Method != "POST")) && (r.URL.Path != "/api/loopback/stop" || r.Method != "POST") {
			failure(w, 404, "private audio API only")
			return
		}
		api.ServeHTTP(w, r)
	})
}
