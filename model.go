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
	"sync"
	"time"
)

type Failover struct {
	Active               string          `json:"active"`
	WiredHealthy         bool            `json:"wired_healthy"`
	WiredIP              string          `json:"wired_ip"`
	WiredGateway         string          `json:"wired_gateway"`
	CellularIP           string          `json:"cellular_ip"`
	CellularPresent      bool            `json:"cellular_present"`
	CellularHealthy      bool            `json:"cellular_healthy"`
	ConnectionState      string          `json:"cellular_connection_state"`
	VPNs                 map[string]bool `json:"vpns"`
	Updated              string          `json:"updated"`
	LastSwitch           string          `json:"last_switch"`
	WiredLink            bool            `json:"wired_link"`
	WiredFailures        int             `json:"wired_failures"`
	WiredRecoverySeconds float64         `json:"wired_recovery_seconds"`
	DialAttempts         int             `json:"cellular_dial_attempts"`
}

func readFailover(path string) (Failover, error) {
	var f Failover
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err = json.Unmarshal(b, &f); err != nil {
		return f, err
	}
	t, err := time.Parse("2006-01-02T15:04:05-0700", f.Updated)
	if err != nil || time.Since(t) > 30*time.Second || time.Until(t) > 30*time.Second {
		return f, errors.New("自动备用网络状态已过期，通话控制暂时锁定")
	}
	return f, nil
}

type Radio struct {
	SessionID     string `json:"-"`
	Location      bool   `json:"location"`
	Present       bool   `json:"present"`
	ID            string `json:"id,omitempty"`
	Model         string `json:"model"`
	Manufacturer  string `json:"manufacturer"`
	Firmware      string `json:"firmware"`
	Operator      string `json:"operator"`
	Technology    string `json:"technology"`
	Signal        int    `json:"signal"`
	State         string `json:"state"`
	SIMReady      bool   `json:"sim_ready"`
	Messaging     bool   `json:"messaging"`
	Voice         bool   `json:"voice"`
	EmergencyOnly bool   `json:"emergency_only"`
	PrimaryPort   string `json:"primary_port"`
}

type Message struct {
	ID         string    `json:"id"`
	Sender     string    `json:"sender"`
	Text       string    `json:"text"`
	Timestamp  string    `json:"timestamp"`
	FirstSeen  time.Time `json:"first_seen"`
	Read       bool      `json:"read"`
	Source     string    `json:"source,omitempty"`
	Direction  string    `json:"direction,omitempty"`
	SendStatus string    `json:"send_status,omitempty"`
	SendError  string    `json:"send_error,omitempty"`
}

type Call struct {
	ID        string `json:"id"`
	Number    string `json:"number"`
	Direction string `json:"direction"`
	State     string `json:"state"`
	StateCode int    `json:"state_code"`
	AudioPort string `json:"audio_port,omitempty"`
}

type Event struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
	Kind string    `json:"kind"`
}

type Snapshot struct {
	Radio            Radio         `json:"radio"`
	Failover         Failover      `json:"failover"`
	Calls            []Call        `json:"calls"`
	Events           []Event       `json:"events"`
	ReceivedCount    int           `json:"received_count"`
	MessageCount     int           `json:"message_count"`
	UnreadCount      int           `json:"unread_count"`
	CallAllowed      bool          `json:"call_allowed"`
	CallBlockReason  string        `json:"call_block_reason"`
	Error            string        `json:"error,omitempty"`
	Updated          time.Time     `json:"updated"`
	Demo             bool          `json:"demo"`
	Version          string        `json:"version"`
	LastModemSuccess time.Time     `json:"last_modem_success"`
	Network          NetworkStatus `json:"network"`
	SMSSendAllowed   bool          `json:"sms_send_allowed"`
	SMSBlockReason   string        `json:"sms_block_reason"`
}

type Inbox struct {
	mu    sync.Mutex
	path  string
	items map[string]Message
}

func openInbox(dir string) (*Inbox, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Inbox{path: filepath.Join(dir, "inbox.json"), items: map[string]Message{}}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []Message
	if err = json.NewDecoder(io.LimitReader(f, 32<<20)).Decode(&items); err != nil {
		return nil, err
	}
	for _, m := range items {
		s.items[m.ID] = m
	}
	return s, nil
}

func messageID(device string, m Message) string {
	source := ""
	if m.Timestamp == "" {
		source = m.Source
	}
	b, _ := json.Marshal([]string{device, m.Sender, m.Timestamp, m.Text, source})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func messageTime(m Message) time.Time {
	if t, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
		return t
	}
	return m.FirstSeen
}

func (s *Inbox) listLocked() []Message {
	items := make([]Message, 0, len(s.items))
	for _, m := range s.items {
		items = append(items, m)
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := messageTime(items[i]), messageTime(items[j])
		if a.Equal(b) {
			return items[i].ID < items[j].ID
		}
		return a.After(b)
	})
	return items
}

func (s *Inbox) List() []Message { s.mu.Lock(); defer s.mu.Unlock(); return s.listLocked() }

func (s *Inbox) Counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unread := 0
	for _, m := range s.items {
		if !m.Read {
			unread++
		}
	}
	return len(s.items), unread
}

func (s *Inbox) saveLocked() error {
	f, err := os.CreateTemp(filepath.Dir(s.path), ".inbox-*")
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
	return os.Rename(name, s.path)
}

func (s *Inbox) Merge(device string, incoming []Message) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := []string{}
	for _, m := range incoming {
		m.ID = messageID(device, m)
		if _, exists := s.items[m.ID]; exists {
			continue
		}
		m.FirstSeen = time.Now().UTC()
		m.Read = false
		s.items[m.ID] = m
		added = append(added, m.ID)
	}
	if len(added) == 0 {
		return 0, nil
	}
	// Do not silently evict old messages. A storage limit returns a visible error.
	if len(s.items) > 10000 {
		for _, id := range added {
			delete(s.items, id)
		}
		return 0, errors.New("短信归档已达到 10000 条，请先备份收件箱")
	}
	if err := s.saveLocked(); err != nil {
		for _, id := range added {
			delete(s.items, id)
		}
		return 0, err
	}
	return len(added), nil
}

func (s *Inbox) MarkRead(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.items[id]
	if !ok {
		return os.ErrNotExist
	}
	if m.Read {
		return nil
	}
	original := m
	m.Read = true
	s.items[id] = m
	if err := s.saveLocked(); err != nil {
		s.items[id] = original
		return err
	}
	return nil
}
