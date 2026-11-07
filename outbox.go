package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var errSMSRateLimit = errors.New("SMS rate limit")
var errSMSDuplicate = errors.New("same SMS recently submitted")

type OutgoingSMS struct {
	ID             string    `json:"id"`
	Number         string    `json:"number"`
	Text           string    `json:"text"`
	Info           SMSInfo   `json:"info"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	ConversationID string    `json:"conversation_id"`
	Ref            SMSRef    `json:"reference"`
}
type Outbox struct {
	mu      sync.Mutex
	path    string
	items   map[string]OutgoingSMS
	lastErr error
}

func openOutbox(dir string) (*Outbox, error) {
	s := &Outbox{path: filepath.Join(dir, "outbox.json"), items: map[string]OutgoingSMS{}}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []OutgoingSMS
	if err = json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&items); err != nil {
		return nil, err
	}
	changed := false
	for _, r := range items {
		switch r.Status {
		case "pending", "creating", "ready":
			r.Status = "failed"
			r.Error = "服务重启前未进入发送阶段，未自动重发"
			changed = true
		case "submitting":
			r.Status = "unknown"
			r.Error = "服务在发送阶段重启，结果待确认；不会自动重发"
			changed = true
		}
		s.items[r.ID] = r
	}
	if changed {
		if err = s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func (s *Outbox) listLocked() []OutgoingSMS {
	items := make([]OutgoingSMS, 0, len(s.items))
	for _, r := range s.items {
		items = append(items, r)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created.After(items[j].Created) })
	return items
}
func (s *Outbox) List() []OutgoingSMS { s.mu.Lock(); defer s.mu.Unlock(); return s.listLocked() }
func (s *Outbox) Get(id string) (OutgoingSMS, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[id]
	return r, ok
}
func (s *Outbox) LastError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.lastErr }
func (s *Outbox) saveLocked() (err error) {
	defer func() { s.lastErr = err }()
	f, err := os.CreateTemp(filepath.Dir(s.path), ".outbox-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		err = json.NewEncoder(f).Encode(s.listLocked())
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Outbox) Reserve(id, number, text string, info SMSInfo) (OutgoingSMS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.items[id]; ok {
		return r, errors.New("request already exists")
	}
	if len(s.items) >= 1000 {
		return OutgoingSMS{}, errors.New("outbox archive full")
	}
	recent := 0
	for _, r := range s.items {
		if time.Since(r.Created) < time.Minute {
			if r.ConversationID == conversationID(number) && r.Text == text && r.Status != "failed" {
				return OutgoingSMS{}, errSMSDuplicate
			}
			recent++
		}
	}
	if recent >= 5 {
		return OutgoingSMS{}, errSMSRateLimit
	}
	now := time.Now().UTC()
	r := OutgoingSMS{ID: id, Number: number, Text: text, Info: info, Status: "pending", Created: now, Updated: now, ConversationID: conversationID(number)}
	s.items[id] = r
	if err := s.saveLocked(); err != nil {
		delete(s.items, id)
		return OutgoingSMS{}, err
	}
	return r, nil
}
func (s *Outbox) Update(id, status, problem string, ref SMSRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok := s.items[id]
	if !ok {
		return os.ErrNotExist
	}
	r := prior
	r.Status = status
	r.Error = problem
	r.Updated = time.Now().UTC()
	r.Ref = ref
	s.items[id] = r
	if err := s.saveLocked(); err != nil {
		s.items[id] = prior
		return err
	}
	return nil
}
func (s *Outbox) Messages() []Message {
	items := s.List()
	messages := make([]Message, 0, len(items))
	for _, r := range items {
		messages = append(messages, Message{ID: "out-" + r.ID, Sender: r.Number, Text: r.Text, Timestamp: r.Created.Format(time.RFC3339), FirstSeen: r.Created, Read: true, Direction: "outgoing", SendStatus: r.Status, SendError: r.Error})
	}
	return messages
}
