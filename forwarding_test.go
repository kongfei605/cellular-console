package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func forwardRuleFixture() ForwardRule {
	return ForwardRule{Name: "测试规则", Enabled: true, Match: "any", Senders: []string{"13800000000"}, Keywords: []string{"告警"}, Webhook: true, URL: "https://receiver.invalid/hook?secret=URL_SECRET", Bearer: "TOKEN_SECRET", SMS: true, Number: "13900000000"}
}
func TestForwardMatchingAndLoopProtection(t *testing.T) {
	r, e := validateForwardRule(forwardRuleFixture())
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		sender, text string
		any, all     bool
	}{{"+8613800000000", "正常", true, false}, {"10010", "系统告警", true, false}, {"13800000000", "系统告警", true, true}, {"10086", "正常", false, false}, {"13800000000", forwardMarker + " 告警", false, false}} {
		m := Message{Sender: tc.sender, Text: tc.text}
		r.Match = "any"
		if matchesForward(r, m) != tc.any {
			t.Fatal(tc)
		}
		r.Match = "all"
		if matchesForward(r, m) != tc.all {
			t.Fatal(tc)
		}
	}
	r.Keywords = []string{"ALERT"}
	r.Senders = nil
	r.Match = "all"
	if !matchesForward(r, Message{Text: "alert online"}) {
		t.Fatal("case-insensitive keyword")
	}
	if matchesForward(r, Message{Text: "alert", Direction: "outgoing"}) {
		t.Fatal("outbox forwarded")
	}
}
func TestForwardRuleValidation(t *testing.T) {
	for _, change := range []func(*ForwardRule){func(r *ForwardRule) { r.Senders = nil; r.Keywords = nil }, func(r *ForwardRule) { r.Match = "regex" }, func(r *ForwardRule) { r.Webhook = false; r.SMS = false }, func(r *ForwardRule) { r.Number = "12" }, func(r *ForwardRule) { r.Bearer = "a\r\nb" }} {
		r := forwardRuleFixture()
		change(&r)
		if _, e := validateForwardRule(r); e == nil {
			t.Fatal(r)
		}
	}
	for _, url := range []string{"http://localhost/", "https://127.0.0.1/", "http://169.254.169.254/", "https://[::1]/", "https://100.100.100.200/", "file:///etc/passwd", "https://user:pass@example.com/", "http://example.com/", "https://0.0.0.0/", "https://224.0.0.1/"} {
		if validateWebhookURL(url) == nil {
			t.Fatal("unsafe webhook", url)
		}
	}
	for _, url := range []string{"https://receiver.example/hook?token=abc", "http://10.23.4.5:8080/sms", "https://192.168.1.9:8443/receive"} {
		if e := validateWebhookURL(url); e != nil {
			t.Fatal(url, e)
		}
	}
}
func TestForwardDurableDedupeActivationAndCancellation(t *testing.T) {
	dir := t.TempDir()
	s, e := openForwarding(dir)
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.put(forwardRuleFixture())
	if e != nil {
		t.Fatal(e)
	}
	old := Message{ID: "old", Sender: "13800000000", Text: "告警旧短信", FirstSeen: time.Now().Add(-time.Hour)}
	if e = s.configure(true, 20); e != nil {
		t.Fatal(e)
	}
	m := Message{ID: "new", Sender: "+8613800000000", Text: "告警新短信", Timestamp: "2026-11-07T10:00:00Z", FirstSeen: time.Now()}
	if e = s.observe([]Message{old, m}, time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(s.data.Jobs) != 2 {
		t.Fatal(s.data.Jobs)
	}
	// Overlapping rules and repeated reads cannot duplicate a destination.
	other := r
	other.ID = ""
	other.Version = 0
	other.Name = "相同目标"
	if _, e = s.put(other); e != nil {
		t.Fatal(e)
	}
	_ = s.observe([]Message{m}, time.Now())
	if len(s.data.Jobs) != 2 {
		t.Fatal("duplicate destination")
	}
	s, e = openForwarding(dir)
	if e != nil {
		t.Fatal(e)
	}
	_ = s.observe([]Message{m}, time.Now())
	if len(s.data.Jobs) != 2 {
		t.Fatal("restart duplicate")
	}
	r.Version = 1
	r.Name = "更新规则"
	if _, e = s.put(r); e != nil {
		t.Fatal(e)
	}
	for _, j := range s.data.Jobs {
		if j.Status != "cancelled" {
			t.Fatal(j)
		}
	}
	if e = s.configure(false, 20); e != nil {
		t.Fatal(e)
	}
	st, _ := os.Stat(filepath.Join(dir, "forwarding.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("private forwarding file permissions")
	}
}

type forwardingRoundTripper func(*http.Request) (*http.Response, error)

func (f forwardingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func forwardingApp(t *testing.T) (*App, *fakeSMSBackend, string) {
	b := &fakeSMSBackend{state: smsStateSent}
	a, f := smsApp(t, b)
	return a, b, f
}
func enqueueForward(t *testing.T, a *App, r ForwardRule) ForwardRule {
	t.Helper()
	saved, e := a.forwarding.put(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.forwarding.configure(true, 20); e != nil {
		t.Fatal(e)
	}
	_, e = a.inbox.Merge("demo", []Message{{Sender: "13800000000", Text: "告警：安全模拟短信", Timestamp: time.Now().Format(time.RFC3339Nano)}})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.forwarding.observe(a.inbox.List(), time.Now()); e != nil {
		t.Fatal(e)
	}
	return saved
}
func TestForwardWebhookPayloadSecretAndBoundedRetry(t *testing.T) {
	a, b, _ := forwardingApp(t)
	rule := forwardRuleFixture()
	rule.SMS = false
	enqueueForward(t, a, rule)
	calls := 0
	event := ""
	a.webhookClient = &http.Client{Transport: forwardingRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		if p["event"] != "sms.received" || p["sender"] != "13800000000" || p["text"] != "告警：安全模拟短信" || r.Header.Get("Authorization") != "Bearer TOKEN_SECRET" {
			t.Fatal(p)
		}
		if event == "" {
			event = p["event_id"].(string)
		}
		if event != r.Header.Get("Idempotency-Key") || event != p["event_id"] {
			t.Fatal("event ID changed")
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("PRIVATE_RESPONSE")), Header: http.Header{}}, nil
	})}
	for i := 0; i < 3; i++ {
		a.forwardOnce(context.Background())
		a.forwarding.mu.Lock()
		a.forwarding.data.Jobs[0].Next = time.Now().Add(-time.Second)
		a.forwarding.mu.Unlock()
	}
	if calls != 3 || a.forwarding.data.Jobs[0].Status != "failed" || b.sends.Load() != 0 {
		t.Fatal(calls, a.forwarding.data.Jobs)
	}
	c, csrf := login(t, a)
	resp := request(a, "GET", "/api/forwarding", "", c, "", "")
	if resp.Code != 200 {
		t.Fatal(resp.Code)
	}
	for _, secret := range []string{"URL_SECRET", "TOKEN_SECRET", "PRIVATE_RESPONSE"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatal("secret exposed", secret)
		}
	}
	for _, path := range []string{"/api/status", "/api/diagnostics"} {
		resp = request(a, "GET", path, "", c, "", "")
		if strings.Contains(resp.Body.String(), "测试规则") || strings.Contains(resp.Body.String(), "receiver.invalid") {
			t.Fatal("forward config leaked")
		}
	}
	_ = csrf
}
func TestForwardSMSUsesExistingGuardsAndNeverResendsUnknown(t *testing.T) {
	a, b, f := forwardingApp(t)
	rule := forwardRuleFixture()
	rule.Webhook = false
	enqueueForward(t, a, rule)
	state := demoFailover()
	state.Active = "cellular"
	writeFailover(t, f, state)
	a.forwardOnce(context.Background())
	if b.prepares.Load() != 0 || b.sends.Load() != 0 || a.forwarding.data.Jobs[0].Status != "waiting" {
		t.Fatal("cellular guard bypassed")
	}
	writeFailover(t, f, demoFailover())
	a.forwarding.mu.Lock()
	a.forwarding.data.Jobs[0].Next = time.Time{}
	a.forwarding.mu.Unlock()
	a.forwardOnce(context.Background())
	id := a.forwarding.data.Jobs[0].ID
	waitSMS(t, a, id, "sent")
	a.forwardOnce(context.Background())
	if b.sends.Load() != 1 || a.forwarding.data.Jobs[0].Status != "sent" {
		t.Fatal(b.sends.Load(), a.forwarding.data.Jobs)
	}
	// An ambiguous send is reconciled, never submitted again.
	a.outbox.Update(id, "unknown", "not confirmed", SMSRef{})
	a.forwarding.update(id, "unknown", "not confirmed", false, time.Time{}, id)
	for i := 0; i < 3; i++ {
		a.forwardOnce(context.Background())
	}
	if b.sends.Load() != 1 {
		t.Fatal("indeterminate send retried")
	}
}
func TestForwardPreviewAndAuthNeverSend(t *testing.T) {
	a, b, _ := forwardingApp(t)
	r := forwardRuleFixture()
	body, _ := json.Marshal(map[string]any{"rule": r, "sender": "13800000000", "text": "告警 模拟验证码123456"})
	if got := request(a, "GET", "/api/forwarding", "", nil, "", ""); got.Code != 401 {
		t.Fatal(got.Code)
	}
	c, csrf := login(t, a)
	if got := request(a, "POST", "/api/forwarding/preview", string(body), c, "", ""); got.Code != 403 {
		t.Fatal(got.Code)
	}
	if got := request(a, "POST", "/api/forwarding/rules", string(body), c, csrf, "http://evil.test"); got.Code != 403 {
		t.Fatal(got.Code)
	}
	got := request(a, "POST", "/api/forwarding/preview", string(body), c, csrf, "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"matched":true`) || !strings.Contains(got.Body.String(), `"network_requests":false`) {
		t.Fatal(got.Code, got.Body)
	}
	if b.sends.Load() != 0 || len(a.forwarding.data.Jobs) != 0 || a.forwarding.data.Enabled {
		t.Fatal("preview caused forwarding")
	}
	if got := request(a, "POST", "/api/forwarding/config", `{"enabled":true,"sms_per_hour":20}`, c, csrf, ""); got.Code != 409 {
		t.Fatal("enabled without rules")
	}
}
func TestForwardDemoNeverUsesNetworkOrModem(t *testing.T) {
	a, b, _ := forwardingApp(t)
	a.demo = true
	enqueueForward(t, a, forwardRuleFixture())
	a.webhookClient = &http.Client{Transport: forwardingRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("demo made external request")
		return nil, fmt.Errorf("unreachable")
	})}
	a.forwardOnce(context.Background())
	a.forwardOnce(context.Background())
	if b.sends.Load() != 0 {
		t.Fatal("demo submitted SMS")
	}
	for _, j := range a.forwarding.data.Jobs {
		if j.Status != "demo" {
			t.Fatal(j)
		}
	}
}

func TestForwardHourlyCapAndExpiry(t *testing.T) {
	s, _ := openForwarding(t.TempDir())
	r, _ := s.put(forwardRuleFixture())
	_ = s.configure(true, 1)
	now := time.Now()
	s.mu.Lock()
	s.data.SMSAttempts = []time.Time{now}
	s.data.Jobs = []ForwardJob{{ID: "used", Channel: "sms", Attempts: 1, AttemptedAt: now, Updated: now, Status: "sent"}, {ID: "next", Channel: "sms", RuleID: r.ID, RuleVersion: r.Version, Created: now, Status: "pending"}}
	s.mu.Unlock()
	if _, _, ok := s.claim(now); ok {
		t.Fatal("hourly cap bypassed")
	}
	if s.data.Jobs[1].Status != "waiting" {
		t.Fatal(s.data.Jobs)
	}
	s.mu.Lock()
	s.data.Jobs[0].AttemptedAt = now.Add(-2 * time.Hour)
	s.data.SMSAttempts = []time.Time{now.Add(-2 * time.Hour)}
	s.data.Jobs[1].Next = time.Time{}
	s.mu.Unlock()
	if _, _, ok := s.claim(now); !ok {
		t.Fatal("hourly cap never expires")
	}
	s.update("next", "pending", "", false, time.Time{}, "")
	if _, _, ok := s.claim(now.Add(25 * time.Hour)); ok {
		t.Fatal("expired task sent")
	}
}
func TestForwardPersistedRunningSMSDoesNotDuplicate(t *testing.T) {
	a, b, _ := forwardingApp(t)
	r := forwardRuleFixture()
	r.Webhook = false
	enqueueForward(t, a, r)
	a.forwardOnce(context.Background())
	id := a.forwarding.data.Jobs[0].ID
	waitSMS(t, a, id, "sent")
	a.forwarding.update(id, "running", "", false, time.Time{}, "")
	reopened, e := openForwarding(filepath.Dir(a.forwarding.path))
	if e != nil {
		t.Fatal(e)
	}
	a.forwarding = reopened
	a.forwardOnce(context.Background())
	if b.sends.Load() != 1 || a.forwarding.data.Jobs[0].Status != "sent" {
		t.Fatal("restart resubmitted existing outbox", a.forwarding.data.Jobs)
	}
}
func TestWebhookDialBlocksLocalAndMetadata(t *testing.T) {
	a, _, _ := forwardingApp(t)
	transport := a.newWebhookClient().Transport.(*http.Transport)
	for _, target := range []string{"127.0.0.1:80", "[::1]:443", "169.254.169.254:80", "192.0.2.10:8788", "100.100.100.200:80"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, e := transport.DialContext(ctx, "tcp", target)
		cancel()
		if conn != nil {
			conn.Close()
		}
		if e == nil {
			t.Fatal("unsafe dial allowed", target)
		}
	}
	if transport.Proxy != nil {
		t.Fatal("proxy environment can bypass address validation")
	}
}
