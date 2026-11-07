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
)

const forwardMarker = "[EC20-FWD]"

type ForwardRule struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	Match       string    `json:"match"`
	Senders     []string  `json:"senders"`
	Keywords    []string  `json:"keywords"`
	Webhook     bool      `json:"webhook"`
	URL         string    `json:"url,omitempty"`
	Bearer      string    `json:"bearer,omitempty"`
	ClearBearer bool      `json:"clear_bearer,omitempty"`
	SMS         bool      `json:"sms"`
	Number      string    `json:"number,omitempty"`
	Version     int       `json:"version"`
	Since       time.Time `json:"since"`
}
type ForwardJob struct {
	AttemptedAt time.Time `json:"attempted_at"`
	ID          string    `json:"id"`
	MessageID   string    `json:"message_id"`
	RuleID      string    `json:"rule_id"`
	RuleName    string    `json:"rule_name"`
	RuleVersion int       `json:"rule_version"`
	Channel     string    `json:"channel"`
	Target      string    `json:"target"`
	Status      string    `json:"status"`
	Problem     string    `json:"problem,omitempty"`
	Attempts    int       `json:"attempts"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	Next        time.Time `json:"next"`
	OutboxID    string    `json:"outbox_id,omitempty"`
}
type forwardingData struct {
	SMSAttempts []time.Time     `json:"sms_attempts"`
	Enabled     bool            `json:"enabled"`
	SMSPerHour  int             `json:"sms_per_hour"`
	Since       time.Time       `json:"since"`
	Rules       []ForwardRule   `json:"rules"`
	Jobs        []ForwardJob    `json:"jobs"`
	Seen        map[string]bool `json:"seen"`
	Problem     string          `json:"problem,omitempty"`
}
type ForwardStore struct {
	mu      sync.Mutex
	path    string
	data    forwardingData
	lastErr string
}

func openForwarding(dir string) (*ForwardStore, error) {
	s := &ForwardStore{path: filepath.Join(dir, "forwarding.json"), data: forwardingData{SMSPerHour: 20, Rules: []ForwardRule{}, Jobs: []ForwardJob{}, Seen: map[string]bool{}}}
	f, e := os.Open(s.path)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if e = json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&s.data); e != nil {
		return nil, e
	}
	if s.data.Seen == nil {
		s.data.Seen = map[string]bool{}
	}
	if s.data.SMSPerHour < 1 || s.data.SMSPerHour > 60 || len(s.data.Rules) > 20 || len(s.data.Jobs) > 1000 || len(s.data.Seen) > 20000 {
		return nil, errors.New("invalid forwarding store bounds")
	}
	for _, r := range s.data.Rules {
		if _, e = validateForwardRule(r); e != nil {
			return nil, errors.New("invalid persisted forwarding rule")
		}
	}
	// Webhook retries use the same event ID; SMS recovery only reconciles the durable outbox.
	for i := range s.data.Jobs {
		j := &s.data.Jobs[i]
		if j.Status == "running" {
			if j.Channel == "webhook" {
				j.Status = "retry"
			} else {
				j.Status = "sms_recover"
			}
		}
	}
	return s, nil
}
func (s *ForwardStore) copy() forwardingData {
	d := s.data
	d.SMSAttempts = append([]time.Time{}, s.data.SMSAttempts...)
	d.Rules = append([]ForwardRule{}, d.Rules...)
	d.Jobs = append([]ForwardJob{}, d.Jobs...)
	d.Seen = make(map[string]bool, len(s.data.Seen))
	for k, v := range s.data.Seen {
		d.Seen[k] = v
	}
	return d
}
func (s *ForwardStore) save(d forwardingData) error {
	if e := privateJSON(s.path, d); e != nil {
		s.lastErr = "转发数据保存失败，已停止启动新任务"
		return e
	}
	s.data = d
	s.lastErr = ""
	return nil
}
func validateForwardRule(r ForwardRule) (ForwardRule, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" || !validLabel(r.Name, 60) {
		return r, errors.New("规则名称为1–60字")
	}
	if r.Match != "any" && r.Match != "all" {
		return r, errors.New("选择任一满足或全部满足")
	}
	if len(r.Senders) > 30 || len(r.Keywords) > 30 {
		return r, errors.New("号码或关键词每组最多30项")
	}
	senders := []string{}
	for _, v := range r.Senders {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !validLabel(v, 40) {
			return r, errors.New("发件标识无效")
		}
		senders = append(senders, canonicalSender(v))
	}
	r.Senders = senders
	keywords := []string{}
	for _, v := range r.Keywords {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !validLabel(v, 80) {
			return r, errors.New("关键词为1–80字")
		}
		keywords = append(keywords, v)
	}
	r.Keywords = keywords
	if len(r.Senders) == 0 && len(r.Keywords) == 0 {
		return r, errors.New("至少配置一个发件号码或关键词")
	}
	if !r.Webhook && !r.SMS {
		return r, errors.New("至少选择一种转发方式")
	}
	if len(r.Bearer) > 2048 || !validLabel(r.Bearer, 2048) {
		return r, errors.New("Bearer token 格式无效")
	}
	if r.Webhook {
		if e := validateWebhookURL(r.URL); e != nil {
			return r, e
		}
	}
	if r.SMS {
		n, _, _, e := validateSMS(r.Number, "x")
		if e != nil {
			return r, e
		}
		r.Number = n
	}
	return r, nil
}
func matchesForward(r ForwardRule, m Message) bool {
	if m.Direction == "outgoing" || strings.Contains(m.Text, forwardMarker) {
		return false
	}
	sender, keyword := false, false
	for _, n := range r.Senders {
		if canonicalSender(m.Sender) == n {
			sender = true
		}
	}
	text := strings.ToLower(m.Text)
	for _, k := range r.Keywords {
		if strings.Contains(text, strings.ToLower(k)) {
			keyword = true
		}
	}
	if r.Match == "all" {
		return (len(r.Senders) == 0 || sender) && (len(r.Keywords) == 0 || keyword) && (len(r.Senders) > 0 || len(r.Keywords) > 0)
	}
	return sender || keyword
}
func forwardID(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func forwardMessageKey(m Message) string {
	return forwardID(canonicalSender(m.Sender), m.Timestamp, m.Text)
}

func pendingForward(status string) bool {
	return status == "pending" || status == "waiting" || status == "retry"
}
func activeForward(status string) bool {
	return pendingForward(status) || status == "running" || status == "sms_recover" || status == "sms_pending"
}
func cancelPending(d *forwardingData, rule string) {
	for i := range d.Jobs {
		j := &d.Jobs[i]
		if (rule == "" || j.RuleID == rule) && pendingForward(j.Status) {
			j.Status = "cancelled"
			j.Problem = "配置关闭或规则已修改，未执行任务已取消"
			j.Updated = time.Now().UTC()
		}
	}
}
func (s *ForwardStore) configure(enabled bool, limit int) error {
	if limit < 1 || limit > 60 {
		return errors.New("短信转发每小时上限为1–60条")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.copy()
	if enabled && !d.Enabled {
		has := false
		for _, r := range d.Rules {
			has = has || r.Enabled
		}
		if !has {
			return errors.New("先保存并启用至少一条规则")
		}
		d.Since = time.Now().UTC()
		d.Seen = map[string]bool{}
		d.Problem = ""
	}
	if !enabled {
		cancelPending(&d, "")
	}
	d.Enabled = enabled
	d.SMSPerHour = limit
	return s.save(d)
}
func (s *ForwardStore) put(r ForwardRule) (ForwardRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.copy()
	at := -1
	for i, p := range d.Rules {
		if p.ID == r.ID && r.ID != "" {
			at = i
			if r.Version != p.Version {
				return r, errors.New("规则已变化，请刷新后再编辑")
			}
			if r.URL == "" {
				r.URL = p.URL
			}
			if r.Bearer == "" && !r.ClearBearer {
				r.Bearer = p.Bearer
			}
			r.Version = p.Version + 1
		}
	}
	if r.ID != "" && at < 0 {
		return r, errors.New("规则不存在")
	}
	if r.ClearBearer {
		r.Bearer = ""
	}
	r.ClearBearer = false
	checked, e := validateForwardRule(r)
	if e != nil {
		return r, e
	}
	r = checked
	if at < 0 {
		if len(d.Rules) >= 20 {
			return r, errors.New("最多20条规则，请编辑已有规则")
		}
		r.ID = randomToken()[:24]
		r.Version = 1
	}
	r.Since = time.Now().UTC()
	if at < 0 {
		d.Rules = append(d.Rules, r)
	} else {
		d.Rules[at] = r
		cancelPending(&d, r.ID)
	}
	return r, s.save(d)
}
func (s *ForwardStore) observe(messages []Message, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.data.Enabled || s.lastErr != "" {
		return nil
	}
	d := s.copy()
	changed := false
	// Seed/compare against persisted FirstSeen; enabling or editing never forwards history.
	sort.Slice(messages, func(i, j int) bool { return messages[i].FirstSeen.Before(messages[j].FirstSeen) })
	processed := 0
	for _, m := range messages {
		if !m.FirstSeen.After(d.Since) || m.Direction == "outgoing" {
			continue
		}
		key := forwardMessageKey(m)
		if d.Seen[key] {
			continue
		}
		if len(d.Seen) >= 20000 {
			d.Enabled = false
			cancelPending(&d, "")
			d.Problem = "去重记录达到20000条，已关闭转发；重新启用只处理之后的新短信"
			changed = true
			break
		}
		if processed >= 100 {
			break
		}
		processed++
		d.Seen[key] = true
		changed = true
		for _, r := range d.Rules {
			if !r.Enabled || m.FirstSeen.Before(r.Since) || !matchesForward(r, m) {
				continue
			}
			for _, channel := range []string{"webhook", "sms"} {
				if (channel == "webhook" && !r.Webhook) || (channel == "sms" && !r.SMS) {
					continue
				}
				targetKey := r.URL
				if channel == "sms" {
					targetKey = canonicalSender(r.Number)
				}
				eventID := forwardID(key, channel, targetKey)
				duplicate := false
				for _, prior := range d.Jobs {
					if prior.ID == eventID {
						duplicate = true
						break
					}
				}
				if duplicate {
					continue
				}
				j := ForwardJob{ID: eventID, MessageID: m.ID, RuleID: r.ID, RuleName: r.Name, RuleVersion: r.Version, Channel: channel, Status: "pending", Created: now, Updated: now}
				if channel == "webhook" {
					j.Target = maskedWebhook(r.URL)
				} else {
					j.Target = r.Number
				}
				pending := 0
				for _, old := range d.Jobs {
					if activeForward(old.Status) {
						pending++
					}
				}
				if pending >= 500 {
					j.Status = "failed"
					j.Problem = "待处理队列达到500条，未转发"
				}
				if channel == "sms" && canonicalSender(r.Number) == canonicalSender(m.Sender) {
					j.Status = "failed"
					j.Problem = "目标与原发件号码相同，已阻止回环"
				}
				d.Jobs = append(d.Jobs, j)
				if len(d.Jobs) > 1000 {
					for index, old := range d.Jobs {
						if !activeForward(old.Status) {
							d.Jobs = append(d.Jobs[:index], d.Jobs[index+1:]...)
							break
						}
					}
				}
			}
		}
	}
	if !changed {
		return nil
	}
	// Keep at most 1000 recent records, never evict live tasks.
	if len(d.Jobs) > 1000 {
		keep := make([]ForwardJob, 0, 1000)
		extra := len(d.Jobs) - 1000
		for _, j := range d.Jobs {
			if extra > 0 && !activeForward(j.Status) {
				extra--
				continue
			}
			keep = append(keep, j)
		}
		d.Jobs = keep
	}
	return s.save(d)
}
func (s *ForwardStore) claim(now time.Time) (ForwardJob, ForwardRule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.data.Enabled || s.lastErr != "" {
		return ForwardJob{}, ForwardRule{}, false
	}
	d := s.copy()
	dirty := false
	for i := range d.Jobs {
		j := &d.Jobs[i]
		if !pendingForward(j.Status) || now.Before(j.Next) {
			continue
		}
		var rule ForwardRule
		for _, r := range d.Rules {
			if r.ID == j.RuleID {
				rule = r
				break
			}
		}
		if !rule.Enabled || rule.Version != j.RuleVersion || now.Sub(j.Created) > 24*time.Hour {
			j.Status = "cancelled"
			j.Problem = "规则失效或任务超过24小时"
			j.Updated = now
			dirty = true
			continue
		}
		if j.Channel == "sms" {
			used := 0
			for _, attempt := range d.SMSAttempts {
				if now.Sub(attempt) < time.Hour {
					used++
				}
			}
			if used >= d.SMSPerHour {
				j.Status = "waiting"
				j.Problem = "达到短信转发每小时上限"
				j.Next = now.Add(time.Minute)
				dirty = true
				continue
			}
		}
		j.Status = "running"
		j.Updated = now
		if e := s.save(d); e != nil {
			return ForwardJob{}, ForwardRule{}, false
		}
		return *j, rule, true
	}
	if dirty {
		_ = s.save(d)
	}
	return ForwardJob{}, ForwardRule{}, false
}
func (s *ForwardStore) update(id, status, problem string, attempt bool, next time.Time, outbox string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.copy()
	for i := range d.Jobs {
		j := &d.Jobs[i]
		if j.ID == id {
			j.Status = status
			j.Problem = problem
			j.Updated = time.Now().UTC()
			j.Next = next
			if attempt {
				if j.Channel == "sms" && j.Attempts == 0 {
					recent := []time.Time{}
					for _, at := range d.SMSAttempts {
						if time.Since(at) < time.Hour {
							recent = append(recent, at)
						}
					}
					d.SMSAttempts = append(recent, time.Now().UTC())
				}
				j.Attempts++
				j.AttemptedAt = time.Now().UTC()
			}
			if outbox != "" {
				j.OutboxID = outbox
			}
			_ = s.save(d)
			return
		}
	}
}
func (s *ForwardStore) allowed(j ForwardJob) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.data.Enabled || s.lastErr != "" {
		return false
	}
	for _, r := range s.data.Rules {
		if r.ID == j.RuleID {
			return r.Enabled && r.Version == j.RuleVersion
		}
	}
	return false
}
