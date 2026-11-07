package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSMSValidationEncodingAndMultipartLimits(t *testing.T) {
	for _, tc := range []struct {
		text, encoding string
		segments       int
	}{
		{strings.Repeat("a", 160), "GSM-7", 1}, {strings.Repeat("a", 161), "GSM-7", 2}, {strings.Repeat("a", 459), "GSM-7", 3},
		{strings.Repeat("^", 80), "GSM-7", 1}, {strings.Repeat("^", 81), "GSM-7", 2}, {strings.Repeat("^", 228), "GSM-7", 3}, {strings.Repeat("中", 70), "UCS-2", 1}, {strings.Repeat("中", 71), "UCS-2", 2}, {strings.Repeat("中", 201), "UCS-2", 3},
	} {
		_, _, info, err := validateSMS("10010", tc.text)
		if err != nil || info.Encoding != tc.encoding || info.Segments != tc.segments {
			t.Fatalf("encoding count: %+v %v", info, err)
		}
	}
	for _, text := range []string{"  ", strings.Repeat("a", 460), strings.Repeat("^", 229), strings.Repeat("中", 202), "hello😀", "a\x00b", "a\u0085b"} {
		if _, _, _, err := validateSMS("13800000000", text); err == nil {
			t.Fatalf("unsupported text accepted: %q", text)
		}
	}
	for _, number := range []string{"12", "a10010", "10010,10086", "*100#", "+123456789012345678901"} {
		if _, _, _, err := validateSMS(number, "test"); err == nil {
			t.Fatal("invalid recipient accepted")
		}
	}
	n, text, _, err := validateSMS("0086 138-0000-0000", "a\r\nb")
	if err != nil || n != "+8613800000000" || text != "a\nb" {
		t.Fatal("recipient/newline normalization")
	}
}

type fakeSMSBackend struct {
	DemoBackend
	prepares            atomic.Int32
	sends               atomic.Int32
	prepareErr, sendErr error
	state               uint32
	block               chan struct{}
}

func (b *fakeSMSBackend) PrepareSMS(ctx context.Context, n, text string) (SMSRef, error) {
	b.prepares.Add(1)
	if b.prepareErr != nil {
		return SMSRef{}, b.prepareErr
	}
	return SMSRef{Owner: ":1.123", Path: "/org/freedesktop/ModemManager1/SMS/7", DeviceID: "demo-ec20"}, nil
}
func (b *fakeSMSBackend) SubmitSMS(ctx context.Context, ref SMSRef) error {
	b.sends.Add(1)
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.sendErr
}
func (b *fakeSMSBackend) ReadSMSState(context.Context, SMSRef) (uint32, error) { return b.state, nil }
func smsApp(t *testing.T, b *fakeSMSBackend) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	inbox, err := openInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "failover.json")
	writeFailover(t, f, demoFailover())
	a := newApp(b, inbox, credentials{"admin", "test-password-12345"}, f, false)
	a.poll(context.Background())
	return a, f
}
func smsBody(id, text string) string {
	b, _ := json.Marshal(map[string]string{"request_id": id, "number": "13800000000", "text": text})
	return string(b)
}
func waitSMS(t *testing.T, a *App, id, status string) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		r, _ := a.outbox.Get(id)
		if r.Status == status {
			a.smsMu.Lock()
			active := a.smsActive
			a.smsMu.Unlock()
			if !active {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, _ := a.outbox.Get(id)
	t.Fatalf("SMS state %q, expected %q", r.Status, status)
}

func TestSMSSendAuthenticationIdempotencyAndConversation(t *testing.T) {
	b := &fakeSMSBackend{state: smsStateSent}
	a, _ := smsApp(t, b)
	id := "request_1234567890123456"
	body := smsBody(id, "中文短信测试，仅演示")
	if r := request(a, "POST", "/api/sms", body, nil, "", ""); r.Code != 401 {
		t.Fatal("unauthenticated SMS")
	}
	c, csrf := login(t, a)
	if r := request(a, "POST", "/api/sms", body, c, "", ""); r.Code != 403 {
		t.Fatal("SMS without CSRF")
	}
	if r := request(a, "POST", "/api/sms", body, c, csrf, "http://evil.test"); r.Code != 403 {
		t.Fatal("cross-origin SMS")
	}
	if r := request(a, "POST", "/api/sms", body, c, csrf, "http://console.test"); r.Code != 202 {
		t.Fatalf("send rejected %d %s", r.Code, r.Body)
	}
	waitSMS(t, a, id, "sent")
	if r := request(a, "POST", "/api/sms", body, c, csrf, ""); r.Code != 200 {
		t.Fatal("idempotent retry failed")
	}
	if b.prepares.Load() != 1 || b.sends.Load() != 1 {
		t.Fatal("duplicate SMS reached backend")
	}
	if r := request(a, "POST", "/api/sms", smsBody(id, "changed"), c, csrf, ""); r.Code != 409 {
		t.Fatal("id reused for new text")
	}
	if r := request(a, "POST", "/api/sms", smsBody("request_different_123456", "中文短信测试，仅演示"), c, csrf, ""); r.Code != 409 {
		t.Fatal("duplicate content not protected")
	}
	d, err := conversationFor(a.allMessages(), conversationID("+8613800000000"))
	if err != nil || len(d.Messages) != 3 || d.Conversation.Unread != 2 {
		t.Fatalf("outgoing grouping/read count: %+v %v", d, err)
	}
	last := d.Messages[len(d.Messages)-1]
	if last.Direction != "outgoing" || last.SendStatus != "sent" || !last.Read {
		t.Fatal("outgoing bubble metadata missing")
	}
	info, err := os.Stat(a.outbox.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("outbox permissions")
	}
}

func TestSMSFreshNetworkAndCallGuards(t *testing.T) {
	for _, kind := range []string{"cellular", "stale", "missing", "call"} {
		t.Run(kind, func(t *testing.T) {
			b := &fakeSMSBackend{state: smsStateSent}
			a, path := smsApp(t, b)
			f := demoFailover()
			switch kind {
			case "cellular":
				f.Active = "cellular"
			case "stale":
				f.Updated = time.Now().Add(-time.Minute).Format("2006-01-02T15:04:05-0700")
			case "missing":
				f.CellularPresent = false
			case "call":
				a.mu.Lock()
				a.snapshot.Calls = []Call{{StateCode: 4}}
				a.mu.Unlock()
			}
			writeFailover(t, path, f)
			c, csrf := login(t, a)
			if r := request(a, "POST", "/api/sms", smsBody("request_guard_123456789", "test"), c, csrf, ""); r.Code != 409 {
				t.Fatalf("guard bypass %d", r.Code)
			}
			if b.prepares.Load() != 0 || b.sends.Load() != 0 {
				t.Fatal("unsafe SMS touched backend")
			}
		})
	}
}

func TestSMSSendingStateIsNotSentAndUnknownNeverRetries(t *testing.T) {
	b := &fakeSMSBackend{state: 4}
	a, _ := smsApp(t, b)
	c, csrf := login(t, a)
	id := "request_sending_12345678"
	request(a, "POST", "/api/sms", smsBody(id, "pending"), c, csrf, "")
	waitSMS(t, a, id, "submitted")
	if b.sends.Load() != 1 {
		t.Fatal("send not called once")
	}
	b2 := &fakeSMSBackend{state: 1, sendErr: context.DeadlineExceeded}
	a2, _ := smsApp(t, b2)
	c2, csrf2 := login(t, a2)
	id2 := "request_unknown_12345678"
	request(a2, "POST", "/api/sms", smsBody(id2, "unknown"), c2, csrf2, "")
	waitSMS(t, a2, id2, "unknown")
	request(a2, "POST", "/api/sms", smsBody(id2, "unknown"), c2, csrf2, "")
	a2.reconcileSMS(context.Background())
	if b2.sends.Load() != 1 {
		t.Fatal("unknown SMS automatically resent")
	}
	ref, _ := a2.outbox.Get(id2)
	ref.Status = "submitting"
	a2.outbox.Update(id2, "submitting", "", ref.Ref)
	reopened, err := openOutbox(filepath.Dir(a2.outbox.path))
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := reopened.Get(id2)
	if saved.Status != "unknown" {
		t.Fatal("restart lost ambiguity protection")
	}
}

func TestSMSStorageFailureAndCreateFailureNeverSend(t *testing.T) {
	b := &fakeSMSBackend{prepareErr: errors.New("not authorised")}
	a, _ := smsApp(t, b)
	c, csrf := login(t, a)
	id := "request_create_fail_12345"
	request(a, "POST", "/api/sms", smsBody(id, "test"), c, csrf, "")
	waitSMS(t, a, id, "failed")
	if b.sends.Load() != 0 {
		t.Fatal("failed create still sent")
	}
	a2, _ := smsApp(t, &fakeSMSBackend{state: smsStateSent})
	a2.outbox.path = filepath.Join(t.TempDir(), "missing", "outbox.json")
	c2, csrf2 := login(t, a2)
	if r := request(a2, "POST", "/api/sms", smsBody("request_disk_fail_123456", "test"), c2, csrf2, ""); r.Code != 503 {
		t.Fatal("storage failure not rejected")
	}
}

func TestSMSRateLimitAndBusySlot(t *testing.T) {
	out, err := openOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = out.Reserve(fmt.Sprintf("request_rate_%020d", i), "10010", fmt.Sprint(i), SMSInfo{Segments: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = out.Reserve("request_rate_final_1234", "10010", "six", SMSInfo{Segments: 1}); !errors.Is(err, errSMSRateLimit) {
		t.Fatal("rate limit missing")
	}
	b := &fakeSMSBackend{state: 5, block: make(chan struct{})}
	a, _ := smsApp(t, b)
	c, csrf := login(t, a)
	request(a, "POST", "/api/sms", smsBody("request_first_123456789", "first"), c, csrf, "")
	if r := request(a, "POST", "/api/sms", smsBody("request_second_12345678", "second"), c, csrf, ""); r.Code != 409 {
		t.Fatal("concurrent SMS accepted")
	}
	close(b.block)
	waitSMS(t, a, "request_first_123456789", "sent")
}
