package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

var chinaMobile = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
var numericSender = regexp.MustCompile(`^\+?[0-9]+$`)

// Normalize only recognizable mobile numbers. Short service codes and sender
// names must not be merged with international numbers accidentally.
func canonicalSender(sender string) string {
	s := strings.TrimSpace(sender)
	compact := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(s)
	if !numericSender.MatchString(compact) {
		return s
	}
	for _, prefix := range []string{"+86", "0086", "86"} {
		if strings.HasPrefix(compact, prefix) && chinaMobile.MatchString(strings.TrimPrefix(compact, prefix)) {
			return strings.TrimPrefix(compact, prefix)
		}
	}
	if strings.HasPrefix(compact, "00") && len(compact) > 8 {
		return "+" + strings.TrimPrefix(compact, "00")
	}
	return compact
}

func conversationID(sender string) string {
	h := sha256.Sum256([]byte("conversation:" + canonicalSender(sender)))
	return hex.EncodeToString(h[:])
}

type Conversation struct {
	ID      string    `json:"id"`
	Sender  string    `json:"sender"`
	Numbers []string  `json:"numbers"`
	Count   int       `json:"count"`
	Unread  int       `json:"unread"`
	Preview string    `json:"preview"`
	LastAt  time.Time `json:"last_at"`
	Matches int       `json:"matches"`
}

type ConversationDetail struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
}

func summarizeConversation(messages []Message) Conversation {
	c := Conversation{ID: conversationID(messages[0].Sender), Sender: canonicalSender(messages[0].Sender), Numbers: []string{}, Count: len(messages)}
	seen := map[string]bool{}
	for _, m := range messages {
		if !m.Read && m.Direction != "outgoing" {
			c.Unread++
		}
		if !seen[m.Sender] {
			c.Numbers = append(c.Numbers, m.Sender)
			seen[m.Sender] = true
		}
		if c.LastAt.IsZero() || messageTime(m).After(c.LastAt) {
			c.LastAt = messageTime(m)
			c.Preview = m.Text
			if m.Direction == "outgoing" {
				c.Preview = "我：" + m.Text
			}
		}
	}
	sort.Strings(c.Numbers)
	return c
}

func (s *Inbox) Conversations(query string, unreadOnly bool) []Conversation {
	return conversationsFor(s.List(), query, unreadOnly)
}
func conversationsFor(items []Message, query string, unreadOnly bool) []Conversation {
	groups := map[string][]Message{}
	for _, m := range items {
		id := conversationID(m.Sender)
		groups[id] = append(groups[id], m)
	}
	result := []Conversation{}
	query = strings.ToLower(strings.TrimSpace(query))
	for _, messages := range groups {
		c := summarizeConversation(messages)
		if unreadOnly && c.Unread == 0 {
			continue
		}
		for _, m := range messages {
			if query == "" || strings.Contains(strings.ToLower(m.Sender+" "+canonicalSender(m.Sender)+" "+m.Text), query) {
				c.Matches++
			}
		}
		if query != "" && c.Matches == 0 {
			continue
		}
		result = append(result, c)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LastAt.Equal(result[j].LastAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].LastAt.After(result[j].LastAt)
	})
	return result
}

func (s *Inbox) Conversation(id string) (ConversationDetail, error) {
	return conversationFor(s.List(), id)
}
func conversationFor(messages []Message, id string) (ConversationDetail, error) {
	items := []Message{}
	for _, m := range messages {
		if conversationID(m.Sender) == id {
			items = append(items, m)
		}
	}
	if len(items) == 0 {
		return ConversationDetail{}, os.ErrNotExist
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := messageTime(items[i]), messageTime(items[j])
		if a.Equal(b) {
			return items[i].ID < items[j].ID
		}
		return a.Before(b)
	})
	return ConversationDetail{Conversation: summarizeConversation(items), Messages: items}, nil
}

func (s *Inbox) MarkConversationRead(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := []string{}
	found := false
	for key, m := range s.items {
		if conversationID(m.Sender) != id {
			continue
		}
		found = true
		if !m.Read {
			m.Read = true
			s.items[key] = m
			changed = append(changed, key)
		}
	}
	if !found {
		return os.ErrNotExist
	}
	if len(changed) == 0 {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		for _, key := range changed {
			m := s.items[key]
			m.Read = false
			s.items[key] = m
		}
		return err
	}
	return nil
}
