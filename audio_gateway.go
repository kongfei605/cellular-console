package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AudioProxy accepts no caller-supplied upstream or identity headers.
// Only a fixed loopback backend receives the private, systemd-loaded key.
type AudioProxy struct {
	base, key string
	client    *http.Client
}

func newAudioProxy(base, key string) (*AudioProxy, error) {
	u, e := url.Parse(base)
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || len(key) < 32 {
		return nil, errors.New("audio backend must be a fixed loopback HTTP address with a private key")
	}
	return &AudioProxy{base: strings.TrimSuffix(base, "/"), key: key, client: &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p *AudioProxy) request(ctx context.Context, method, path, owner string, expires time.Time, body io.Reader) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, p.base+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("X-EC20-Gateway-Key", p.key)
	req.Header.Set("X-EC20-Owner", owner)
	req.Header.Set("X-EC20-Expires", strconv.FormatInt(expires.Unix(), 10))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.client.Do(req)
}
func (a *App) audioRoutes(mux *http.ServeMux) {
	forward := func(method, upstream string) http.HandlerFunc {
		return a.protected(func(w http.ResponseWriter, r *http.Request) {
			if a.audio == nil {
				apiError(w, 503, "音频后端尚未配置，主控制台和自动备用网络仍独立运行")
				return
			}
			owner, s, ok := a.authenticate(r)
			if !ok {
				apiError(w, 401, "请重新登录")
				return
			}
			var body io.Reader
			if method == "POST" {
				if r.Header.Get("Content-Type") != "application/json" {
					apiError(w, 400, "请使用JSON请求")
					return
				}
				limit := int64(65536)
				if upstream == "/api/loopback/stop" {
					limit = 1024
				}
				data, e := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
				if e != nil {
					apiError(w, 413, "音频请求过大")
					return
				}
				body = bytes.NewReader(data)
			}
			response, e := a.audio.request(r.Context(), method, upstream, owner, s.Expires, body)
			if e != nil {
				apiError(w, 503, "音频后端暂时不可用，网络与其他页面不受影响")
				return
			}
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 500 || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
				apiError(w, 503, "音频后端暂时不可用")
				return
			}
			data, e := io.ReadAll(io.LimitReader(response.Body, 128<<10))
			if e != nil {
				apiError(w, 502, "音频返回结果未完成")
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(response.StatusCode)
			w.Write(data)
		})
	}
	mux.HandleFunc("GET /api/audio/status", forward("GET", "/api/loopback"))
	mux.HandleFunc("POST /api/audio/offer", forward("POST", "/api/loopback"))
	mux.HandleFunc("POST /api/audio/stop", forward("POST", "/api/loopback/stop"))
	// Keep old signalling clients/probes compatible, using the same console login.
	mux.HandleFunc("GET /api/loopback", forward("GET", "/api/loopback"))
	mux.HandleFunc("POST /api/loopback", forward("POST", "/api/loopback"))
	mux.HandleFunc("POST /api/loopback/stop", forward("POST", "/api/loopback/stop"))
	for _, path := range []string{"/audio", "/audio/", "/audio-lab"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/#audio", http.StatusTemporaryRedirect)
		})
	}
}
func (a *App) releaseAudio(owner string, expires time.Time) {
	if a.audio == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	response, e := a.audio.request(ctx, "POST", "/internal/revoke", owner, expires, strings.NewReader("{}"))
	if e == nil {
		response.Body.Close()
	}
}
func (a *App) sessionCookieName() string {
	if a.secure {
		return "__Host-ec20_session"
	}
	return "ec20_session"
}
func canonicalRedirect(origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != "GET" && r.Method != "HEAD" {
			apiError(w, http.StatusUpgradeRequired, "请刷新页面，使用统一HTTPS入口登录和操作")
			return
		}
		http.Redirect(w, r, origin+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}
