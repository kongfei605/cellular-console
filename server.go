package main

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
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

const version = "1.3.0"

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type session struct {
	CSRF    string
	Expires time.Time
}
type loginLimit struct {
	Count int
	Until time.Time
}

type App struct {
	workspace    *Workspace
	backend      Backend
	inbox        *Inbox
	password     [32]byte
	username     string
	failoverPath string
	demo         bool
	mu           sync.RWMutex
	pollMu       sync.Mutex
	actionMu     sync.Mutex
	snapshot     Snapshot
	sessions     map[string]session
	limits       map[string]loginLimit
	network      *NetworkMonitor
	outbox       *Outbox
	smsMu        sync.Mutex
	smsActive    bool
	smsCursor    int
}

func newApp(backend Backend, inbox *Inbox, creds credentials, failoverPath string, demo bool) *App {
	a := &App{backend: backend, inbox: inbox, password: sha256.Sum256([]byte(creds.Password)), username: creds.Username,
		failoverPath: failoverPath, demo: demo, sessions: map[string]session{}, limits: map[string]loginLimit{},
		snapshot: Snapshot{Calls: []Call{}, Events: []Event{}, Version: version, Demo: demo, Network: NetworkStatus{Config: defaultNetworkConfig(), Interfaces: []InterfaceStats{}, Samples: []TrafficSample{}, Events: []NetworkEvent{}}}}
	var workspaceErr error
	a.workspace, workspaceErr = openWorkspace(filepath.Dir(inbox.path))
	if workspaceErr != nil {
		log.Printf("workspace unavailable: %v", workspaceErr)
	}
	a.network = newNetworkMonitor(filepath.Dir(inbox.path), "/etc/ec20-failover.json")
	var outboxErr error
	a.outbox, outboxErr = openOutbox(filepath.Dir(inbox.path))
	if outboxErr != nil {
		log.Printf("SMS outbox unavailable: %v", outboxErr)
	}
	if demo {
		start := time.Now()
		a.network.read = func(name string) (counterReading, error) {
			elapsed := time.Since(start).Seconds()
			base := uint64(1)
			scale := float64(1)
			if name == "ppp0" {
				base = 2
				scale = .02
			}
			return counterReading{Index: base, RX: uint64(2e7*scale + elapsed*76000*scale), TX: uint64(5e6*scale + elapsed*18000*scale), Up: true}, nil
		}
	}
	return a
}

func (a *App) failover() (Failover, error) {
	if a.demo {
		return demoFailover(), nil
	}
	return readFailover(a.failoverPath)
}

func callPolicy(r Radio, f Failover, failErr error) string {
	if failErr != nil {
		return "自动备用网络状态不可用，通话控制暂时锁定"
	}
	if f.Active != "wired" || !f.WiredHealthy {
		return "EC20 正在承载备用网络；有线恢复后可拨号、接听"
	}
	if !f.CellularPresent || !r.Present {
		return "EC20 未连接，插入后会自动恢复"
	}
	if !r.SIMReady {
		return "SIM 或移动网络尚未就绪"
	}
	if !r.Voice || r.EmergencyOnly {
		return "当前模块或 SIM 尚不支持普通语音通话"
	}
	return ""
}

func (a *App) poll(ctx context.Context) {
	a.pollMu.Lock()
	defer a.pollMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r, m, c, err := a.backend.Snapshot(ctx)
	f, fe := a.failover()
	netStatus := a.network.Collect(f, fe == nil, time.Now().UTC())
	if a.demo {
		netStatus.ConfigReadable = true
	}
	problem := ""
	if err != nil {
		problem = "暂时无法读取调制解调器"
		log.Printf("modem snapshot: %v", err)
	}
	if err == nil && a.workspace != nil {
		if e := a.workspace.Observe(r, c, time.Now().UTC()); e != nil {
			log.Printf("call history persistence failed")
		}
	}
	added := 0
	if err == nil && r.Present {
		var se error
		added, se = a.inbox.Merge(r.ID, m)
		if se != nil {
			problem = "短信本地归档失败，请检查服务日志"
			log.Printf("inbox persistence: %v", se)
		}
	}
	if fe != nil {
		problem = "自动备用网络状态不可用，通话控制已锁定"
	}
	messageCount, unread := a.inbox.Counts()
	receivedCount := messageCount
	if a.outbox != nil {
		messageCount += len(a.outbox.List())
	}
	if c == nil {
		c = []Call{}
	}
	smsReason := smsPolicy(r, f, fe, c, time.Now().UTC())
	if err != nil {
		smsReason = "调制解调器状态暂时不可用"
	}
	if a.outbox == nil {
		smsReason = "本地发送记录不可用"
	} else if a.outbox.LastError() != nil {
		smsReason = "发送记录写入异常，请检查存储"
	}
	if _, ok := a.backend.(SMSBackend); !ok {
		smsReason = "当前后端未提供短信发送"
	}
	a.smsMu.Lock()
	if a.smsActive {
		smsReason = "上一条短信仍在处理，请稍候"
	}
	a.smsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	previous := a.snapshot
	lastSuccess := previous.LastModemSuccess
	if err == nil && r.Present {
		lastSuccess = time.Now().UTC()
	}
	events := append([]Event{}, previous.Events...)
	add := func(kind, text string) {
		events = append([]Event{{Time: time.Now().UTC(), Kind: kind, Text: text}}, events...)
	}
	if previous.Updated.IsZero() {
		add("system", "控制台已连接，自动备用网络独立运行")
	} else {
		if previous.Radio.Present != r.Present {
			if r.Present {
				add("device", "EC20 已连接")
			} else {
				add("device", "EC20 离线，保留本地短信归档")
			}
		}
		if previous.Failover.Active != f.Active {
			if f.Active == "cellular" {
				add("network", "已切换到蜂窝出口，通话控制暂时锁定")
			} else {
				add("network", "已恢复有线出口")
			}
		}
	}
	if added > 0 {
		add("message", "收到新短信，已保存到本地收件箱")
	}
	if len(events) > 40 {
		events = events[:40]
	}
	reason := callPolicy(r, f, fe)
	if err != nil {
		reason = "调制解调器状态暂时不可用"
	}
	a.snapshot = Snapshot{Radio: r, Failover: f, Calls: c, Events: events, MessageCount: messageCount, UnreadCount: unread,
		ReceivedCount: receivedCount, CallAllowed: reason == "", CallBlockReason: reason, Error: problem, Updated: time.Now().UTC(), Version: version, Demo: a.demo,
		LastModemSuccess: lastSuccess, Network: netStatus}
	a.snapshot.SMSSendAllowed = smsReason == ""
	a.snapshot.SMSBlockReason = smsReason
}

func (a *App) refreshInboxCounts() {
	count, unread := a.inbox.Counts()
	receivedCount := count
	if a.outbox != nil {
		count += len(a.outbox.List())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.snapshot.ReceivedCount = receivedCount
	a.snapshot.MessageCount = count
	a.snapshot.UnreadCount = unread
}

func (a *App) background(ctx context.Context) {
	a.poll(ctx)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.poll(ctx)
			a.reconcileSMS(ctx)
		}
	}
}

func (a *App) state() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s := a.snapshot
	s.Calls = append([]Call{}, s.Calls...)
	s.Events = append([]Event{}, s.Events...)
	return s
}
func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, text string) {
	writeJSON(w, status, map[string]string{"error": text})
}
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("请使用 JSON 请求")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return errors.New("请求格式错误")
	}
	return nil
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
}
func (a *App) authenticate(r *http.Request) (string, session, bool) {
	c, err := r.Cookie("ec20_session")
	if err != nil {
		return "", session{}, false
	}
	a.mu.RLock()
	s, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return c.Value, s, ok && time.Now().Before(s.Expires)
}
func (a *App) protected(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, s, ok := a.authenticate(r)
		if !ok {
			apiError(w, 401, "请先登录")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(s.CSRF), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
				apiError(w, 403, "安全校验失败，请刷新后重试")
				return
			}
		}
		fn(w, r)
	}
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		apiError(w, 403, "请求来源不允许")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if readJSON(w, r, &input) != nil {
		apiError(w, 400, "登录信息格式错误")
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.mu.Lock()
	for k, v := range a.limits {
		if time.Now().After(v.Until) {
			delete(a.limits, k)
		}
	}
	limit := a.limits[host]
	if limit.Count >= 8 && time.Now().Before(limit.Until) {
		a.mu.Unlock()
		apiError(w, 429, "尝试次数过多，请 10 分钟后再试")
		return
	}
	h := sha256.Sum256([]byte(input.Password))
	if subtle.ConstantTimeCompare(h[:], a.password[:]) != 1 || input.Username != a.username {
		limit.Count++
		limit.Until = time.Now().Add(10 * time.Minute)
		a.limits[host] = limit
		a.mu.Unlock()
		apiError(w, 401, "用户名或密码不正确")
		return
	}
	delete(a.limits, host)
	for k, v := range a.sessions {
		if time.Now().After(v.Expires) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= 64 {
		a.mu.Unlock()
		apiError(w, 429, "活动会话过多，请稍后重试")
		return
	}
	token := randomToken()
	s := session{CSRF: randomToken(), Expires: time.Now().Add(8 * time.Hour)}
	a.sessions[token] = s
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "ec20_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 28800})
	writeJSON(w, 200, map[string]any{"username": a.username, "csrf": s.CSRF, "demo": a.demo})
}

var phonePattern = regexp.MustCompile(`^\+?[0-9]{7,20}$`)
var callIDPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

func normalizePhone(phone string) (string, error) {
	p := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(phone))
	if !phonePattern.MatchString(p) {
		return "", errors.New("请输入 7–20 位电话号码，可使用 + 国际区号")
	}
	return p, nil
}

func (a *App) voiceAction(w http.ResponseWriter, r *http.Request) {
	a.actionMu.Lock()
	defer a.actionMu.Unlock()
	action := r.PathValue("action")
	id := r.PathValue("id")
	if action == "" {
		action = "dial"
	}
	if action != "dial" && action != "accept" && action != "hangup" {
		apiError(w, 404, "操作不存在")
		return
	}
	if action != "dial" && !callIDPattern.MatchString(id) {
		apiError(w, 400, "呼叫标识无效")
		return
	}
	if action != "hangup" {
		a.smsMu.Lock()
		smsBusy := a.smsActive
		a.smsMu.Unlock()
		if smsBusy {
			apiError(w, 409, "短信请求正在处理，请稍候再拨号或接听")
			return
		}
		f, err := a.failover()
		s := a.state()
		reason := callPolicy(s.Radio, f, err)
		if reason != "" {
			apiError(w, 409, reason)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var err error
	if action == "dial" {
		var input struct {
			Number string `json:"number"`
		}
		if readJSON(w, r, &input) != nil {
			apiError(w, 400, "号码格式错误")
			return
		}
		n, ne := normalizePhone(input.Number)
		if ne != nil {
			apiError(w, 400, ne.Error())
			return
		}
		id, err = a.backend.Dial(ctx, n)
	} else {
		err = a.backend.Act(ctx, id, action)
	}
	if err != nil {
		log.Printf("voice action %s failed: %v", action, err)
		apiError(w, 502, "通话操作未完成："+err.Error())
		return
	}
	log.Printf("voice action %s completed", action)
	a.poll(r.Context())
	writeJSON(w, 200, map[string]string{"id": id, "status": "ok"})
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	a.workspaceRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version})
	})
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("GET /api/session", a.protected(func(w http.ResponseWriter, r *http.Request) {
		_, s, _ := a.authenticate(r)
		writeJSON(w, 200, map[string]any{"username": a.username, "csrf": s.CSRF, "demo": a.demo})
	}))
	mux.HandleFunc("POST /api/logout", a.protected(func(w http.ResponseWriter, r *http.Request) {
		token, _, _ := a.authenticate(r)
		a.mu.Lock()
		delete(a.sessions, token)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "ec20_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /api/status", a.protected(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.state()) }))
	mux.HandleFunc("GET /api/messages", a.protected(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.allMessages()) }))
	mux.HandleFunc("POST /api/sms", a.protected(a.sendSMS))
	mux.HandleFunc("GET /api/outbox", a.protected(func(w http.ResponseWriter, r *http.Request) {
		if a.outbox == nil {
			apiError(w, 503, "发送记录不可用")
			return
		}
		writeJSON(w, 200, a.outbox.List())
	}))
	mux.HandleFunc("GET /api/conversations", a.protected(func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(query) > 600 {
			apiError(w, 400, "搜索内容过长")
			return
		}
		items := a.allMessages()
		threads := conversationsFor(items, query, r.URL.Query().Get("unread") == "1")
		result := []Conversation{}
		trash := r.URL.Query().Get("trash") == "1"
		for _, c := range threads {
			archived := a.workspace != nil && a.workspace.IsArchived(c.ID, items)
			if archived == trash {
				result = append(result, c)
			}
		}
		writeJSON(w, 200, result)
	}))
	mux.HandleFunc("GET /api/conversations/{id}", a.protected(func(w http.ResponseWriter, r *http.Request) {
		d, err := conversationFor(a.allMessages(), r.PathValue("id"))
		if err != nil {
			apiError(w, 404, "会话不存在")
			return
		}
		writeJSON(w, 200, d)
	}))
	mux.HandleFunc("POST /api/conversations/{id}/read", a.protected(func(w http.ResponseWriter, r *http.Request) {
		err := a.inbox.MarkConversationRead(r.PathValue("id"))
		if errors.Is(err, os.ErrNotExist) {
			_, present := conversationFor(a.allMessages(), r.PathValue("id"))
			if present == nil {
				err = nil
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			apiError(w, 404, "会话不存在")
			return
		}
		if err != nil {
			apiError(w, 500, "保存已读状态失败")
			return
		}
		a.refreshInboxCounts()
		count, unread := a.inbox.Counts()
		writeJSON(w, 200, map[string]any{"ok": true, "message_count": count, "unread_count": unread})
	}))
	mux.HandleFunc("POST /api/messages/{id}/read", a.protected(func(w http.ResponseWriter, r *http.Request) {
		err := a.inbox.MarkRead(r.PathValue("id"))
		if errors.Is(err, os.ErrNotExist) {
			apiError(w, 404, "短信不存在")
			return
		}
		if err != nil {
			apiError(w, 500, "保存已读状态失败")
			return
		}
		a.refreshInboxCounts()
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("POST /api/calls", a.protected(a.voiceAction))
	mux.HandleFunc("POST /api/calls/{id}/{action}", a.protected(a.voiceAction))
	static, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
