package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForwardCustomHeadersValidationAndDelivery(t *testing.T) {
	for _, headers := range [][]ForwardHeader{
		{{Name: "X-Key", Value: "a\r\nInjected: x"}}, {{Name: "Bad Name", Value: "x"}},
		{{Name: "X-Key", Value: "a"}, {Name: "x-key", Value: "b"}},
		{{Name: "Content-Length", Value: "1"}}, {{Name: "Host", Value: "example.com"}},
		{{Name: "Idempotency-Key", Value: "override"}}, {{Name: "Content-Type", Value: "text/plain"}},
		{{Name: "Connection", Value: "close"}}, {{Name: "X-EC20-Event-ID", Value: "override"}},
	} {
		if _, e := validateForwardHeaders(headers); e == nil {
			t.Fatal("invalid headers accepted")
		}
	}
	a, _, _ := forwardingApp(t)
	r := forwardRuleFixture()
	r.Bearer = ""
	r.SMS = false
	r.Headers = []ForwardHeader{{Name: "Authorization", Value: "Basic SYNTHETIC_SECRET"}, {Name: "X-Api-Key", Value: "KEY_SECRET"}, {Name: "X-Tenant", Value: "test"}}
	enqueueForward(t, a, r)
	calls := 0
	a.webhookClient = &http.Client{Transport: forwardingRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "Basic SYNTHETIC_SECRET" || req.Header.Get("X-API-Key") != "KEY_SECRET" || req.Header.Get("X-Tenant") != "test" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Idempotency-Key") == "" {
			t.Fatal("headers missing or overwritten")
		}
		return &http.Response{StatusCode: 204, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	a.forwardOnce(context.Background())
	if calls != 1 {
		t.Fatal(calls)
	}
	c, _ := login(t, a)
	public := request(a, "GET", "/api/forwarding", "", c, "", "")
	if strings.Contains(public.Body.String(), "SYNTHETIC_SECRET") || strings.Contains(public.Body.String(), "KEY_SECRET") {
		t.Fatal("public list exposes values")
	}
	id := a.forwarding.data.Rules[0].ID
	if request(a, "GET", "/api/forwarding/rules/"+id, "", nil, "", "").Code != 401 {
		t.Fatal("editor lacks auth")
	}
	editor := request(a, "GET", "/api/forwarding/rules/"+id, "", c, "", "")
	if editor.Code != 200 || editor.Header().Get("Cache-Control") != "no-store" || !strings.Contains(editor.Body.String(), "SYNTHETIC_SECRET") || !strings.Contains(editor.Body.String(), "KEY_SECRET") {
		t.Fatal("editor does not expose configured headers")
	}
}

func TestForwardLegacyBearerMigrationAndHeaderEdits(t *testing.T) {
	dir := t.TempDir()
	legacy := forwardRuleFixture()
	legacy.ID = "legacy"
	legacy.Version = 1
	legacy.Since = time.Now()
	data := forwardingData{SMSPerHour: 20, Rules: []ForwardRule{legacy}, Seen: map[string]bool{}}
	b, _ := json.Marshal(data)
	if e := os.WriteFile(filepath.Join(dir, "forwarding.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	s, e := openForwarding(dir)
	if e != nil {
		t.Fatal(e)
	}
	migrated := s.data.Rules[0]
	if migrated.Bearer != "" || len(migrated.Headers) != 1 || migrated.Headers[0].Name != "Authorization" || migrated.Headers[0].Value != "Bearer TOKEN_SECRET" {
		t.Fatal("legacy token lost")
	}
	before, _ := os.ReadFile(s.path)
	if string(before) != string(b) {
		t.Fatal("opening rewrote user config")
	}
	migrated.Headers = append(migrated.Headers, ForwardHeader{Name: "X-Key", Value: "another"})
	saved, e := s.put(migrated)
	if e != nil {
		t.Fatal(e)
	}
	omitted := saved
	omitted.Headers = nil
	saved, e = s.put(omitted)
	if e != nil || len(saved.Headers) != 2 {
		t.Fatal("omitted headers not preserved", e)
	}
	saved.Headers = []ForwardHeader{}
	saved, e = s.put(saved)
	if e != nil || len(saved.Headers) != 0 {
		t.Fatal("explicit removal not saved", e)
	}
	s, e = openForwarding(dir)
	if e != nil || len(s.data.Rules[0].Headers) != 0 {
		t.Fatal("removed headers resurrected", e)
	}
}
