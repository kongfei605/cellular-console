package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type Contact struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Number  string    `json:"number"`
	Note    string    `json:"note"`
	Deleted bool      `json:"deleted"`
	Updated time.Time `json:"updated"`
}
type CallRecord struct {
	ID              string     `json:"id"`
	Number          string     `json:"number"`
	Direction       string     `json:"direction"`
	Status          string     `json:"status"`
	FirstSeen       time.Time  `json:"first_seen"`
	LastSeen        time.Time  `json:"last_seen"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	ObservedSeconds *int64     `json:"observed_seconds,omitempty"`
	Partial         bool       `json:"partial"`
}
type workspaceData struct {
	Contacts []Contact            `json:"contacts"`
	Calls    []CallRecord         `json:"calls"`
	Archived map[string]time.Time `json:"archived_threads"`
}
type Workspace struct {
	mu      sync.Mutex
	path    string
	data    workspaceData
	lastErr string
}

func openWorkspace(dir string) (*Workspace, error) {
	s := &Workspace{path: filepath.Join(dir, "workspace.json"), data: workspaceData{Contacts: []Contact{}, Calls: []CallRecord{}, Archived: map[string]time.Time{}}}
	f, e := os.Open(s.path)
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if e = json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&s.data); e != nil {
		return nil, e
	}
	if s.data.Archived == nil {
		s.data.Archived = map[string]time.Time{}
	}
	return s, nil
}
func privateJSON(path string, value any) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".workspace-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		e = json.NewEncoder(f).Encode(value)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *Workspace) save(next workspaceData) error {
	if e := privateJSON(s.path, next); e != nil {
		s.lastErr = "本地数据写入失败"
		return e
	}
	s.data = next
	s.lastErr = ""
	return nil
}
func (s *Workspace) Contacts() []Contact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Contact{}, s.data.Contacts...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func validLabel(v string, max int) bool {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s *Workspace) PutContact(c Contact) (Contact, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.Note = strings.TrimSpace(c.Note)
	if c.Name == "" || !validLabel(c.Name, 80) || !validLabel(c.Note, 300) {
		return c, errors.New("姓名须为1–80字，备注最多300字，不支持控制字符")
	}
	n, _, _, e := validateSMS(c.Number, "x")
	if e != nil {
		return c, e
	}
	c.Number = n
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Contacts = append([]Contact{}, next.Contacts...)
	at := -1
	for i, prior := range next.Contacts {
		if prior.ID == c.ID {
			at = i
		}
		if prior.ID != c.ID && !prior.Deleted && !c.Deleted && canonicalSender(prior.Number) == canonicalSender(c.Number) {
			return c, errors.New("这个号码已有联系人，请编辑原记录")
		}
	}
	if c.ID != "" && at < 0 {
		return c, os.ErrNotExist
	}
	if c.ID == "" {
		if len(next.Contacts) >= 1000 {
			return c, errors.New("联系人最多1000条（含回收站）")
		}
		c.ID = randomToken()[:24]
	}
	c.Updated = time.Now().UTC()
	if at < 0 {
		next.Contacts = append(next.Contacts, c)
	} else {
		next.Contacts[at] = c
	}
	return c, s.save(next)
}
func (s *Workspace) Calls() []CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]CallRecord{}, s.data.Calls...)
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.After(out[j].FirstSeen) })
	return out
}
func callRecordID(device, session, id string) string {
	sum := sha256.Sum256([]byte(device + "/" + session + "/" + id))
	return hex.EncodeToString(sum[:])
}

// All times describe observations, not carrier billing. Never invent old history.
func (s *Workspace) Observe(r Radio, calls []Call, now time.Time) error {
	if r.SessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Calls = append([]CallRecord{}, next.Calls...)
	positions := map[string]int{}
	seen := map[string]bool{}
	changed := false
	for i, c := range next.Calls {
		positions[c.ID] = i
	}
	for _, c := range calls {
		id := callRecordID(r.ID, r.SessionID, c.ID)
		seen[id] = true
		at, ok := positions[id]
		if !ok {
			if len(next.Calls) >= 5000 {
				s.lastErr = "通话记录已达5000条，请导出后联系管理员整理"
				continue
			}
			at = len(next.Calls)
			next.Calls = append(next.Calls, CallRecord{ID: id, Number: c.Number, Direction: c.Direction, FirstSeen: now, Partial: c.StateCode != 1 && c.StateCode != 2 && c.StateCode != 3})
			positions[id] = at
			changed = true
		}
		rec := next.Calls[at]
		if rec.EndedAt != nil {
			continue
		}
		rec.LastSeen = now
		rec.Status = "active"
		if c.Number != "" {
			rec.Number = c.Number
		}
		if c.StateCode == 4 || c.StateCode == 5 || c.StateCode == 6 {
			if rec.ConnectedAt == nil {
				t := now
				rec.ConnectedAt = &t
			}
			rec.Status = "connected"
		}
		if c.StateCode == 7 {
			t := now
			rec.EndedAt = &t
			rec.Status = "unanswered"
			if rec.ConnectedAt != nil {
				rec.Status = "completed"
				secs := int64(now.Sub(*rec.ConnectedAt).Seconds())
				rec.ObservedSeconds = &secs
			} else if rec.Partial {
				rec.Status = "ended_unknown"
			}
		}
		next.Calls[at] = rec
		changed = true
	}
	for i, rec := range next.Calls {
		if rec.EndedAt == nil && !seen[rec.ID] {
			t := rec.LastSeen
			rec.EndedAt = &t
			rec.Status = "lost"
			rec.Partial = true
			rec.ObservedSeconds = nil
			next.Calls[i] = rec
			changed = true
		}
	}
	if changed {
		return s.save(next)
	}
	return nil
}
func (s *Workspace) Archive(id string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Archived = map[string]time.Time{}
	for k, v := range s.data.Archived {
		next.Archived[k] = v
	}
	if on {
		next.Archived[id] = time.Now().UTC()
	} else {
		delete(next.Archived, id)
	}
	return s.save(next)
}
func (s *Workspace) IsArchived(id string, items []Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.data.Archived[id]
	if !ok {
		return false
	}
	for _, m := range items {
		if conversationID(m.Sender) == id && m.FirstSeen.After(at) {
			return false
		}
	}
	return true
}
func (s *Workspace) Error() string { s.mu.Lock(); defer s.mu.Unlock(); return s.lastErr }
