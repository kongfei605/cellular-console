package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceContactPersistenceAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, e := openWorkspace(dir)
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.PutContact(Contact{Name: "测试联系人", Number: "+86 13800000000", Note: "测试"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.PutContact(Contact{Name: "重复", Number: "13800000000"}); e == nil {
		t.Fatal("canonical duplicate accepted")
	}
	c.Deleted = true
	if _, e = s.PutContact(c); e != nil {
		t.Fatal(e)
	}
	s, e = openWorkspace(dir)
	if e != nil || len(s.Contacts()) != 1 || !s.Contacts()[0].Deleted {
		t.Fatal("soft delete not persisted")
	}
	c.Deleted = false
	if _, e = s.PutContact(c); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"", "a\x00b", strings.Repeat("中", 81)} {
		if _, e = s.PutContact(Contact{Name: name, Number: "10010"}); e == nil {
			t.Fatal("invalid label")
		}
	}
	info, _ := os.Stat(filepath.Join(dir, "workspace.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("private file permissions")
	}
	if e = os.WriteFile(filepath.Join(dir, "workspace.json"), []byte("invalid"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = openWorkspace(dir); e == nil {
		t.Fatal("corrupt workspace accepted")
	}
}
func TestCallHistoryObservationAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := openWorkspace(dir)
	now := time.Now().UTC()
	r := Radio{ID: "device", SessionID: ":1.4", Present: true}
	c := Call{ID: "1", Number: "13800000000", Direction: "outgoing", StateCode: 1}
	s.Observe(r, []Call{c}, now)
	c.StateCode = 4
	s.Observe(r, []Call{c}, now.Add(5*time.Second))
	s, _ = openWorkspace(dir)
	c.StateCode = 7
	s.Observe(r, []Call{c}, now.Add(20*time.Second))
	items := s.Calls()
	if len(items) != 1 || items[0].Status != "completed" || items[0].ObservedSeconds == nil || *items[0].ObservedSeconds != 15 {
		t.Fatalf("bad duration: %+v", items)
	}
	s.Observe(r, []Call{c}, now.Add(time.Minute))
	if !s.Calls()[0].EndedAt.Equal(now.Add(20 * time.Second)) {
		t.Fatal("end time drifted")
	}
	r.SessionID = ":1.5"
	c.StateCode = 3
	c.Direction = "incoming"
	s.Observe(r, []Call{c}, now.Add(time.Minute))
	if len(s.Calls()) != 2 {
		t.Fatal("MM instance reused ID")
	}
	s.Observe(r, []Call{}, now.Add(2*time.Minute))
	if s.Calls()[0].Status != "lost" || s.Calls()[0].ObservedSeconds != nil {
		t.Fatal("disappearance invented duration")
	}
	c.ID = "9"
	c.StateCode = 7
	s.Observe(r, []Call{c}, now.Add(3*time.Minute))
	if s.Calls()[0].Status != "ended_unknown" || !s.Calls()[0].Partial {
		t.Fatal("invented historical connection")
	}
	// A failed write must not advance the in-memory archive.
	s.path = filepath.Join(dir, "missing", "file.json")
	c.ID = "10"
	if e := s.Observe(r, []Call{c}, now.Add(4*time.Minute)); e == nil {
		t.Fatal("storage error hidden")
	}
	if len(s.Calls()) != 3 {
		t.Fatal("unpersisted record committed")
	}
}
func TestArchiveRevivesOnNewSMS(t *testing.T) {
	s, _ := openWorkspace(t.TempDir())
	id := conversationID("10010")
	old := Message{Sender: "10010", FirstSeen: time.Now().Add(-time.Minute)}
	if e := s.Archive(id, true); e != nil {
		t.Fatal(e)
	}
	if !s.IsArchived(id, []Message{old}) {
		t.Fatal("not archived")
	}
	fresh := old
	fresh.FirstSeen = time.Now().Add(time.Second)
	if s.IsArchived(id, []Message{old, fresh}) {
		t.Fatal("new SMS remained hidden")
	}
	s.Archive(id, false)
	if s.IsArchived(id, []Message{old}) {
		t.Fatal("not restored")
	}
}
func TestWorkspaceAPIsAndDiagnosticsPrivacy(t *testing.T) {
	b := &fakeSMSBackend{state: smsStateSent}
	a, _ := smsApp(t, b)
	a.poll(context.Background())
	cookie, csrf := login(t, a)
	for _, path := range []string{"/api/contacts", "/api/call-history", "/api/diagnostics", "/api/diagnostics/export"} {
		if result := request(a, "GET", path, "", nil, "", ""); result.Code != 401 {
			t.Fatalf("unauth %s: %d", path, result.Code)
		}
	}
	bad := request(a, "POST", "/api/contacts", `{"name":"测试","number":"13800000000"}`, cookie, "", "")
	if bad.Code != 403 {
		t.Fatal("contact CSRF")
	}
	saved := request(a, "POST", "/api/contacts", `{"name":"隐私名字","number":"13800000000","note":"隐私备注"}`, cookie, csrf, "")
	if saved.Code != 200 {
		t.Fatal(saved.Body.String())
	}
	var c Contact
	json.Unmarshal(saved.Body.Bytes(), &c)
	got := request(a, "GET", "/api/contacts", "", cookie, "", "")
	if !strings.Contains(got.Body.String(), "隐私名字") {
		t.Fatal("missing contact")
	}
	data := request(a, "GET", "/api/diagnostics", "", cookie, "", "")
	for _, secret := range []string{"13800000000", "隐私名字", "隐私备注", "demo-ec20", "test-password-12345"} {
		if strings.Contains(data.Body.String(), secret) {
			t.Fatalf("diagnostics leaked %s", secret)
		}
	}
	z := request(a, "GET", "/api/diagnostics/export", "", cookie, "", "")
	if z.Code != 200 {
		t.Fatal(z.Code)
	}
	archive, e := zip.NewReader(bytes.NewReader(z.Body.Bytes()), int64(z.Body.Len()))
	if e != nil || len(archive.File) != 2 {
		t.Fatal("invalid zip")
	}
	items := a.allMessages()
	id := conversationID(items[0].Sender)
	result := request(a, "POST", "/api/conversations/"+id+"/archive", `{"archived":true}`, cookie, csrf, "")
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	normal := request(a, "GET", "/api/conversations", "", cookie, "", "")
	trash := request(a, "GET", "/api/conversations?trash=1", "", cookie, "", "")
	if strings.Contains(normal.Body.String(), id) || !strings.Contains(trash.Body.String(), id) {
		t.Fatal("trash filtering")
	}
}
