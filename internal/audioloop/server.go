package audioloop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kongfei605/cellular-console/internal/voicepcm"
	"github.com/pion/webrtc/v4"
)

//go:embed web/*
var assets embed.FS

const Version = "0.3.0"
const cookieName = "__Host-ec20_audio_session"

type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type session struct {
	CSRF    string
	Expires time.Time
}
type attempt struct {
	Count   int
	Expires time.Time
}
type Server struct {
	Voice      VoiceProvider
	delegated  bool
	gatewayKey [32]byte
	revoked    map[string]time.Time
	media      *Media
	origin     string
	username   string
	password   [32]byte
	mu         sync.Mutex
	sessions   map[string]session
	attempts   map[string]attempt
}

func NewServer(media *Media, origin string, creds Credentials) *Server {
	return &Server{media: media, origin: origin, username: creds.Username, password: sha256.Sum256([]byte(creds.Password)), sessions: map[string]session{}, attempts: map[string]attempt{}}
}
func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func jsonResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, code int, text string) {
	jsonResponse(w, code, map[string]string{"error": text})
}
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	if r.Header.Get("Content-Type") != "application/json" {
		return errors.New("JSON required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}
func (s *Server) auth(r *http.Request) (string, session, bool) {
	if s.delegated {
		id, v, ok := s.gatewayIdentity(r)
		return id, v, ok && !s.isRevoked(id)
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", session{}, false
	}
	s.mu.Lock()
	v, ok := s.sessions[c.Value]
	s.mu.Unlock()
	return c.Value, v, ok && time.Now().Before(v.Expires)
}
func (s *Server) protect(fn func(http.ResponseWriter, *http.Request, string, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, v, ok := s.auth(r)
		if !ok {
			failure(w, 401, "请先登录")
			return
		}
		if !s.delegated && r.Method != "GET" && (r.Header.Get("Origin") != s.origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(v.CSRF)) != 1) {
			failure(w, 403, "请求来源或安全令牌无效")
			return
		}
		fn(w, r, id, v)
	}
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.origin {
		failure(w, 403, "请求来源不允许")
		return
	}
	var input Credentials
	if decode(w, r, &input, 4096) != nil {
		failure(w, 400, "登录信息格式错误")
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, a := range s.attempts {
		if time.Now().After(a.Expires) {
			delete(s.attempts, k)
		}
	}
	for k, v := range s.sessions {
		if time.Now().After(v.Expires) {
			delete(s.sessions, k)
		}
	}
	a := s.attempts[host]
	if a.Count >= 8 {
		failure(w, 429, "尝试次数过多，请十分钟后再试")
		return
	}
	h := sha256.Sum256([]byte(input.Password))
	if subtle.ConstantTimeCompare(h[:], s.password[:]) != 1 || input.Username != s.username {
		if len(s.attempts) >= 128 && a.Count == 0 {
			failure(w, 429, "请稍后重试")
			return
		}
		s.attempts[host] = attempt{Count: a.Count + 1, Expires: time.Now().Add(10 * time.Minute)}
		failure(w, 401, "用户名或密码不正确")
		return
	}
	if len(s.sessions) >= 32 {
		failure(w, 429, "登录会话过多，请稍后重试")
		return
	}
	delete(s.attempts, host)
	id := token()
	v := session{CSRF: token(), Expires: time.Now().Add(8 * time.Hour)}
	s.sessions[id] = v
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
	jsonResponse(w, 200, map[string]any{"username": s.username, "csrf": v.CSRF})
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]any{"ok": true, "version": Version, "mode": "software-loopback", "hardware_access": s.Voice != nil})
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/session", s.protect(func(w http.ResponseWriter, r *http.Request, id string, v session) {
		jsonResponse(w, 200, map[string]any{"username": s.username, "csrf": v.CSRF})
	}))
	mux.HandleFunc("POST /api/logout", s.protect(func(w http.ResponseWriter, r *http.Request, id string, v session) {
		s.media.StopOwner(id)
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		jsonResponse(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /api/loopback", s.protect(func(w http.ResponseWriter, r *http.Request, id string, v session) {
		stats := s.media.Stats(id)
		if s.Voice != nil {
			e := s.Voice.Ready()
			stats.VoiceReady = e == nil
			if e != nil {
				stats.VoiceReason = "EC20 USB 声卡未就绪"
			}
		} else {
			stats.VoiceReason = "真实通话音频尚未启用"
		}
		jsonResponse(w, 200, stats)
	}))
	mux.HandleFunc("POST /api/loopback", s.protect(func(w http.ResponseWriter, r *http.Request, id string, v session) {
		var input struct {
			Type webrtc.SDPType `json:"type"`
			SDP  string         `json:"sdp"`
			Mode string         `json:"mode"`
			Call string         `json:"call"`
		}
		if decode(w, r, &input, 65536) != nil {
			failure(w, 400, "音频协商数据无效")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		offer := webrtc.SessionDescription{Type: input.Type, SDP: input.SDP}
		if validateOffer(offer) != nil {
			failure(w, 400, "音频协商数据无效")
			return
		}
		mode := input.Mode
		if mode == "" {
			mode = "software-loopback"
		}
		if mode != "software-loopback" && mode != "ec20-call" {
			failure(w, 400, "音频模式无效")
			return
		}
		if s.media.Stats(id).Active {
			failure(w, 409, ErrBusy.Error())
			return
		}
		var factory voicepcm.Factory
		var cleanup func()
		if mode == "ec20-call" {
			if s.Voice == nil {
				failure(w, 503, "真实通话音频未启用")
				return
			}
			var e error
			factory, cleanup, e = s.Voice.Prepare(ctx, input.Call)
			if e != nil {
				failure(w, 409, "真实通话或网络保护未通过，无法接入音频")
				return
			}
			if factory == nil || cleanup == nil {
				failure(w, 503, "真实音频设备未准备完成")
				if cleanup != nil {
					cleanup()
				}
				return
			}
			original := cleanup
			var once sync.Once
			cleanup = func() { once.Do(original) }
		} else if input.Call != "" {
			failure(w, 400, "软件回环不选择呼叫")
			return
		}
		accepted := false
		defer func() {
			if !accepted && cleanup != nil {
				cleanup()
			}
		}()
		peer, answer, err := s.media.offer(ctx, id, v.Expires, offer, mode, input.Call, factory, cleanup)
		if errors.Is(err, ErrBusy) {
			failure(w, 409, err.Error())
			return
		}
		if err != nil {
			failure(w, 400, "音频协商未完成，请检查浏览器是否支持 PCMU/8000")
			return
		}
		if s.delegated && s.isRevoked(id) {
			s.media.StopOwner(id)
			failure(w, 401, "session revoked")
			return
		}
		accepted = true
		jsonResponse(w, 200, map[string]any{"id": peer, "answer": answer, "max_seconds": 180, "mode": mode, "call": input.Call})
	}))
	mux.HandleFunc("POST /api/loopback/stop", s.protect(func(w http.ResponseWriter, r *http.Request, id string, v session) {
		var input struct {
			ID string `json:"id"`
		}
		if decode(w, r, &input, 1024) != nil {
			failure(w, 400, "会话标识无效")
			return
		}
		if !s.media.Stop(id, input.ID) {
			failure(w, 403, "此音频会话属于其他浏览器")
			return
		}
		jsonResponse(w, 200, map[string]bool{"ok": true})
	}))
	web, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; media-src 'self' blob:; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'")
		if !s.delegated && r.TLS == nil {
			failure(w, 400, "此服务仅支持 HTTPS")
			return
		}
		if !s.delegated && "https://"+r.Host != s.origin {
			failure(w, 421, "访问地址不匹配")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
