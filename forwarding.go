package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func forwardRulePublic(r ForwardRule) map[string]any {
	return map[string]any{"id": r.ID, "name": r.Name, "enabled": r.Enabled, "match": r.Match, "senders": r.Senders, "keywords": r.Keywords, "webhook": r.Webhook, "url_display": maskedWebhook(r.URL), "url_configured": r.URL != "", "bearer_configured": r.Bearer != "", "sms": r.SMS, "number": r.Number, "version": r.Version, "since": r.Since}
}
func (a *App) forwardingStatus() map[string]any {
	if a.forwarding == nil {
		return map[string]any{"available": false, "enabled": false, "error": "转发存储不可用，未启用自动转发"}
	}
	s := a.forwarding
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := []map[string]any{}
	for _, r := range s.data.Rules {
		rules = append(rules, forwardRulePublic(r))
	}
	jobs := append([]ForwardJob{}, s.data.Jobs...)
	for i, j := 0, len(jobs)-1; i < j; i, j = i+1, j-1 {
		jobs[i], jobs[j] = jobs[j], jobs[i]
	}
	if len(jobs) > 100 {
		jobs = jobs[:100]
	}
	problem := s.data.Problem
	if s.lastErr != "" {
		problem = s.lastErr
	}
	return map[string]any{"available": true, "enabled": s.data.Enabled, "sms_per_hour": s.data.SMSPerHour, "rules": rules, "jobs": jobs, "error": problem, "demo": a.demo, "sms_block_reason": a.smsAllowed()}
}
func (a *App) forwardingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/forwarding", a.protected(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.forwardingStatus()) }))
	mux.HandleFunc("POST /api/forwarding/config", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Enabled    *bool `json:"enabled"`
			SMSPerHour int   `json:"sms_per_hour"`
		}
		if readJSONSize(w, r, &v, 32<<10) != nil || v.Enabled == nil {
			apiError(w, 400, "转发设置格式无效")
			return
		}
		if a.forwarding == nil {
			apiError(w, 503, "转发存储不可用")
			return
		}
		if e := a.forwarding.configure(*v.Enabled, v.SMSPerHour); e != nil {
			apiError(w, 409, e.Error())
			return
		}
		writeJSON(w, 200, a.forwardingStatus())
	}))
	mux.HandleFunc("POST /api/forwarding/rules", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var rule ForwardRule
		if readJSONSize(w, r, &rule, 32<<10) != nil {
			apiError(w, 400, "规则格式无效")
			return
		}
		if a.forwarding == nil {
			apiError(w, 503, "转发存储不可用")
			return
		}
		saved, e := a.forwarding.put(rule)
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, forwardRulePublic(saved))
	}))
	mux.HandleFunc("POST /api/forwarding/preview", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Rule   ForwardRule `json:"rule"`
			Sender string      `json:"sender"`
			Text   string      `json:"text"`
		}
		if readJSONSize(w, r, &v, 32<<10) != nil || len(v.Text) > 10000 || !validLabel(v.Sender, 40) {
			apiError(w, 400, "测试短信格式无效")
			return
		}
		if a.forwarding != nil && v.Rule.ID != "" {
			s := a.forwarding
			s.mu.Lock()
			for _, prior := range s.data.Rules {
				if prior.ID == v.Rule.ID && v.Rule.URL == "" {
					v.Rule.URL = prior.URL
				}
			}
			s.mu.Unlock()
		}
		checked, e := validateForwardRule(v.Rule)
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		m := Message{Sender: v.Sender, Text: v.Text}
		body := forwardSMSBody(m)
		var info SMSInfo
		problem := ""
		if checked.SMS {
			_, _, info, e = validateSMS(checked.Number, body)
			if e != nil {
				problem = e.Error()
			}
		}
		writeJSON(w, 200, map[string]any{"matched": matchesForward(checked, m), "sms_preview": body, "sms_info": info, "sms_error": problem, "network_requests": false, "modem_operations": false, "note": "只做条件匹配与分段预览，不保存消息、不发送请求或短信"})
	}))
}
func forwardSMSBody(m Message) string { return forwardMarker + "\nFrom: " + m.Sender + "\n" + m.Text }
func (a *App) forwardLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.forwardOnce(ctx)
		}
	}
}
func (a *App) forwardOnce(ctx context.Context) {
	s := a.forwarding
	if s == nil {
		return
	}
	// Recover/reconcile SMS through existing durable outbox handles; never blindly resend.
	s.mu.Lock()
	jobs := append([]ForwardJob{}, s.data.Jobs...)
	s.mu.Unlock()
	for _, j := range jobs {
		if j.Channel != "sms" || (j.Status != "sms_recover" && j.Status != "sms_pending" && j.Status != "submitted" && j.Status != "unknown") {
			continue
		}
		id := j.OutboxID
		if id == "" {
			id = j.ID
		}
		if a.outbox == nil {
			continue
		}
		o, found := a.outbox.Get(id)
		if !found {
			if j.Status == "sms_recover" {
				s.update(j.ID, "pending", "重启前尚未进入发送队列", false, time.Time{}, id)
			}
			continue
		}
		next := "sms_pending"
		if o.Status == "sent" || o.Status == "failed" || o.Status == "unknown" || o.Status == "submitted" {
			next = o.Status
		}
		if next != j.Status || j.OutboxID == "" {
			s.update(j.ID, next, o.Error, j.Attempts == 0, time.Time{}, id)
		}
	}
	j, rule, ok := s.claim(time.Now().UTC())
	if !ok {
		return
	}
	var message Message
	found := false
	for _, m := range a.inbox.List() {
		if m.ID == j.MessageID {
			message = m
			found = true
			break
		}
	}
	if !found {
		s.update(j.ID, "failed", "原短信归档不可用，未转发", false, time.Time{}, "")
		return
	}
	if !s.allowed(j) {
		s.update(j.ID, "cancelled", "转发已关闭或规则已改变", false, time.Time{}, "")
		return
	}
	f, e := a.failover()
	if e != nil || (f.Active == "wired" && !f.WiredHealthy) || (f.Active == "cellular" && !f.CellularHealthy) || (f.Active != "wired" && f.Active != "cellular") {
		s.update(j.ID, "waiting", "等待健康网络出口", false, time.Now().Add(15*time.Second), "")
		return
	}
	if j.Channel == "sms" {
		if a.outbox != nil {
			if existing, yes := a.outbox.Get(j.ID); yes {
				s.update(j.ID, existing.Status, existing.Error, j.Attempts == 0, time.Time{}, j.ID)
				return
			}
		}
		number, text, info, e := validateSMS(rule.Number, forwardSMSBody(message))
		if e != nil {
			s.update(j.ID, "failed", "短信内容无法按最多3段原样转发："+e.Error(), false, time.Time{}, "")
			return
		}
		if reason := a.smsAllowed(); reason != "" {
			s.update(j.ID, "waiting", reason, false, time.Now().Add(15*time.Second), "")
			return
		}
		if a.demo {
			_ = number
			_ = text
			_ = info
			s.update(j.ID, "demo", "演示模式：没有发送真实短信", true, time.Time{}, "")
			return
		}
		_, _, code, e := a.queueSMS(j.ID, number, text, info)
		if e != nil {
			if code == 409 || code == 429 {
				s.update(j.ID, "waiting", "短信保护或速率限制："+e.Error(), false, time.Now().Add(15*time.Second), "")
			} else {
				s.update(j.ID, "failed", "无法进入短信发送队列", false, time.Time{}, "")
			}
			return
		}
		s.update(j.ID, "sms_pending", "已进入短信发送队列", true, time.Time{}, j.ID)
		return
	}
	if j.Attempts >= 3 {
		s.update(j.ID, "failed", "Webhook 已尝试3次，停止自动重试，请核对接收端", false, time.Time{}, "")
		return
	}
	if a.demo {
		s.update(j.ID, "demo", "演示模式：没有向 Webhook 发起网络请求", true, time.Time{}, "")
		return
	}
	s.update(j.ID, "running", "", true, time.Time{}, "")
	sendCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	status, err := a.postForwardWebhook(sendCtx, j, rule, message)
	if err == nil && status >= 200 && status < 300 {
		s.update(j.ID, "delivered", "Webhook 返回2xx；不代表下游业务处理成功", false, time.Time{}, "")
		return
	}
	problem := fmt.Sprintf("Webhook HTTP %d", status)
	retry := status == 0 || status == 429 || status == 502 || status == 503 || status == 504
	if retry && j.Attempts+1 < 3 {
		s.update(j.ID, "retry", "投递未确认，将携带同一事件标识重试；接收端需去重", false, time.Now().Add(time.Duration(15*(j.Attempts+1))*time.Second), "")
	} else {
		if err != nil {
			problem = "Webhook 请求结果未确认，请核对接收端"
		}
		s.update(j.ID, "failed", problem, false, time.Time{}, "")
	}
}
