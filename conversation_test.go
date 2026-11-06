package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestConversationNumberAliasesAndSearch(t *testing.T) {
	cases := map[string]string{"+86 138-0000-0000": "13800000000", "008613800000000": "13800000000", "8613800000000": "13800000000", "13800000000": "13800000000", "10010": "10010", "+8610010": "+8610010", "00442012345678": "+442012345678", "测试联系人": "测试联系人"}
	for input, want := range cases {
		if got := canonicalSender(input); got != want {
			t.Fatalf("canonicalSender(%q)=%q; want %q", input, got, want)
		}
	}
	s, _ := openInbox(t.TempDir())
	m := []Message{{Sender: "10010", Text: "运营商提示", Timestamp: "2026-11-05T10:00:00+08:00"}, {Sender: "+8613800000000", Text: "第一条", Timestamp: "2026-11-05T12:00:00+08:00"}, {Sender: "13800000000", Text: "周末继续调试", Timestamp: "2026-11-06T12:00:00+08:00"}}
	if _, e := s.Merge("device", m); e != nil {
		t.Fatal(e)
	}
	c := s.Conversations("", false)
	if len(c) != 2 || c[0].Count != 2 || c[0].Unread != 2 || c[0].Sender != "13800000000" {
		t.Fatalf("wrong conversation grouping: %+v", c)
	}
	result := s.Conversations("周末", false)
	if len(result) != 1 || result[0].Matches != 1 || result[0].Count != 2 {
		t.Fatal("search discarded conversation context")
	}
	detail, e := s.Conversation(c[0].ID)
	if e != nil || len(detail.Messages) != 2 || detail.Messages[0].Text != "第一条" || len(detail.Conversation.Numbers) != 2 {
		t.Fatal("chronology or original numbers lost")
	}
	if e = s.MarkConversationRead(c[0].ID); e != nil {
		t.Fatal(e)
	}
	if len(s.Conversations("", true)) != 1 {
		t.Fatal("read conversation stayed in unread filter")
	}
	reopened, e := openInbox(filepath.Dir(s.path))
	if e != nil {
		t.Fatal(e)
	}
	if reopened.Conversations("13800000000", false)[0].Unread != 0 {
		t.Fatal("conversation read state lost on restart")
	}
	if conversationID("10010") == conversationID("+8610010") {
		t.Fatal("short service code merged with a distinct number")
	}
}

func TestConversationAPIAuthenticationAndReadTransaction(t *testing.T) {
	a, _, _ := testApp(t)
	for _, p := range []string{"/api/conversations", "/api/conversations/missing"} {
		if w := request(a, "GET", p, "", nil, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatal("conversation API leaked without login")
		}
	}
	c, csrf := login(t, a)
	w := request(a, "GET", "/api/conversations?q=10010", "", c, "", "")
	var threads []Conversation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &threads) != nil || len(threads) != 1 || threads[0].Count != 2 {
		t.Fatal("conversation list failed")
	}
	id := threads[0].ID
	if w = request(a, "POST", "/api/conversations/"+id+"/read", `{}`, c, "", ""); w.Code != 403 {
		t.Fatal("conversation read accepted without CSRF")
	}
	if w = request(a, "POST", "/api/conversations/"+id+"/read", `{}`, c, csrf, ""); w.Code != 200 {
		t.Fatal("conversation read failed")
	}
	_, unread := a.inbox.Counts()
	if a.state().UnreadCount != unread {
		t.Fatal("global unread count did not update immediately")
	}
	w = request(a, "GET", "/api/conversations/"+id, "", c, "", "")
	var detail ConversationDetail
	if json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Conversation.Unread != 0 || len(detail.Messages) != 2 {
		t.Fatal("conversation read not reflected")
	}
}
