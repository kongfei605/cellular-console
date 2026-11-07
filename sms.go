package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
const gsmExtended = "^{}\\[~]|€"
const smsStateSent uint32 = 5

var smsNumberPattern = regexp.MustCompile(`^\+?[0-9]{3,20}$`)
var smsRequestPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

type SMSInfo struct {
	Encoding string `json:"encoding"`
	Units    int    `json:"units"`
	Segments int    `json:"segments"`
}
type SMSRef struct {
	Owner    string `json:"owner"`
	Path     string `json:"path"`
	DeviceID string `json:"device_id"`
}
type SMSBackend interface {
	PrepareSMS(context.Context, string, string) (SMSRef, error)
	SubmitSMS(context.Context, SMSRef) error
	ReadSMSState(context.Context, SMSRef) (uint32, error)
}

func validateSMS(number, text string) (string, string, SMSInfo, error) {
	number = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(number))
	if !smsNumberPattern.MatchString(number) {
		return "", "", SMSInfo{}, errors.New("请输入3–20位单个收件号码，可使用+国际区号")
	}
	if strings.HasPrefix(number, "00") && len(number) > 8 {
		number = "+" + strings.TrimPrefix(number, "00")
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return "", "", SMSInfo{}, errors.New("请输入短信内容")
	}
	info := SMSInfo{Encoding: "GSM-7"}
	for _, r := range text {
		if r > 0xffff || (unicode.IsControl(r) && r != '\n') {
			return "", "", SMSInfo{}, errors.New("暂不支持表情、BMP以外字符或控制字符，请修改内容")
		}
		if strings.ContainsRune(gsmBasic, r) {
			info.Units++
		} else if strings.ContainsRune(gsmExtended, r) {
			info.Units += 2
		} else {
			info.Encoding = "UCS-2"
		}
	}
	single, multi := 160, 153
	if info.Encoding == "UCS-2" {
		info.Units = utf8.RuneCountInString(text)
		single, multi = 70, 67
	}
	info.Segments = 1
	if info.Units > single {
		if info.Encoding == "GSM-7" {
			// An escape and its extension character cannot straddle two parts.
			used := 0
			for _, r := range text {
				n := 1
				if strings.ContainsRune(gsmExtended, r) {
					n = 2
				}
				if used+n > multi {
					info.Segments++
					used = 0
				}
				used += n
			}
		} else {
			info.Segments = (info.Units + multi - 1) / multi
		}
	}
	if info.Segments > 3 {
		return "", "", info, errors.New("每次最多发送3段短信，请缩短内容")
	}
	return number, text, info, nil
}

func smsPolicy(r Radio, f Failover, failErr error, calls []Call, last time.Time) string {
	if failErr != nil {
		return "自动备用网络状态不可用，暂时不能发送"
	}
	if f.Active != "wired" || !f.WiredHealthy {
		return "蜂窝正在承载备用网络，有线恢复后可发送短信"
	}
	if !f.CellularPresent || !r.Present {
		return "EC20未连接，仍可查看本地短信"
	}
	if !f.CellularHealthy || f.CellularIP == "" {
		return "蜂窝备用尚未恢复健康，暂时不能发送"
	}
	if last.IsZero() || time.Since(last) > 15*time.Second || time.Until(last) > time.Second {
		return "设备状态过期，请稍候刷新"
	}
	if !r.SIMReady || !r.Messaging || r.EmergencyOnly {
		return "SIM或短信服务尚未就绪"
	}
	for _, c := range calls {
		if c.StateCode != 7 {
			return "当前有呼叫，请结束后再发送短信"
		}
	}
	return ""
}

func (a *App) smsAllowed() string {
	if a.outbox == nil {
		return "本地发送记录不可用，暂时不能发送"
	}
	if a.outbox.LastError() != nil {
		return "发送记录写入异常，请检查存储后再操作"
	}
	if _, ok := a.backend.(SMSBackend); !ok {
		return "当前设备后端未提供短信发送"
	}
	f, err := a.failover()
	s := a.state()
	return smsPolicy(s.Radio, f, err, s.Calls, s.LastModemSuccess)
}

func (a *App) sendSMS(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RequestID string `json:"request_id"`
		Number    string `json:"number"`
		Text      string `json:"text"`
	}
	if readJSON(w, r, &input) != nil || !smsRequestPattern.MatchString(input.RequestID) {
		apiError(w, 400, "短信请求格式无效")
		return
	}
	number, text, info, err := validateSMS(input.Number, input.Text)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	record, duplicate, code, err := a.queueSMS(input.RequestID, number, text, info)
	if err != nil {
		apiError(w, code, err.Error())
		return
	}
	writeJSON(w, code, map[string]any{"message": record, "duplicate": duplicate})
}

// Both manual sends and forwarding share this durable queue and all modem guards.
func (a *App) queueSMS(id, number, text string, info SMSInfo) (OutgoingSMS, bool, int, error) {
	a.actionMu.Lock()
	defer a.actionMu.Unlock()
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	if a.outbox == nil {
		return OutgoingSMS{}, false, 503, errors.New("本地发送记录不可用")
	}
	if prior, ok := a.outbox.Get(id); ok {
		if prior.Number != number || prior.Text != text {
			return OutgoingSMS{}, false, 409, errors.New("请求标识已用于其他短信，请重新编辑")
		}
		return prior, true, 200, nil
	}
	if a.smsActive || a.locationBusy {
		return OutgoingSMS{}, false, 409, errors.New("短信或定位设置仍在处理，请稍候")
	}
	if reason := a.smsAllowed(); reason != "" {
		return OutgoingSMS{}, false, 409, errors.New(reason)
	}
	record, err := a.outbox.Reserve(id, number, text, info)
	if errors.Is(err, errSMSDuplicate) {
		return OutgoingSMS{}, false, 409, errors.New("相同短信刚刚提交，请先查看会话中的发送状态")
	}
	if errors.Is(err, errSMSRateLimit) {
		return OutgoingSMS{}, false, 429, errors.New("一分钟内最多提交5条短信，请稍后重试")
	}
	if err != nil {
		return OutgoingSMS{}, false, 503, errors.New("无法保存发送记录，未提交到设备")
	}
	a.smsActive = true
	a.refreshInboxCounts()
	a.mu.Lock()
	a.snapshot.SMSSendAllowed = false
	a.snapshot.SMSBlockReason = "上一条短信仍在处理，请稍候"
	a.mu.Unlock()
	go a.sendSMSJob(record)
	return record, false, 202, nil
}

func (a *App) sendSMSJob(record OutgoingSMS) {
	defer func() {
		a.smsMu.Lock()
		a.smsActive = false
		reason := a.smsAllowed()
		a.mu.Lock()
		a.snapshot.SMSSendAllowed = reason == ""
		a.snapshot.SMSBlockReason = reason
		a.mu.Unlock()
		a.smsMu.Unlock()
		a.refreshInboxCounts()
	}()
	backend := a.backend.(SMSBackend)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	update := func(status, problem string, ref SMSRef) error {
		return a.outbox.Update(record.ID, status, problem, ref)
	}
	if reason := a.smsAllowed(); reason != "" {
		update("failed", reason, SMSRef{})
		return
	}
	if update("creating", "", SMSRef{}) != nil {
		return
	}
	createCtx, createCancel := context.WithTimeout(ctx, 8*time.Second)
	ref, err := backend.PrepareSMS(createCtx, record.Number, record.Text)
	createCancel()
	if err != nil {
		update("failed", "无法创建短信，请检查SIM、运营商或短信权限；未执行发送", SMSRef{})
		return
	}
	if update("ready", "", ref) != nil {
		return
	} // Never send unless the handle is durable.
	if reason := a.smsAllowed(); reason != "" {
		update("failed", reason, ref)
		return
	}
	if update("submitting", "", ref) != nil {
		return
	} // Crash from here is indeterminate.
	sendCtx, sendCancel := context.WithTimeout(ctx, 25*time.Second)
	err = backend.SubmitSMS(sendCtx, ref)
	sendCancel()
	if err == nil {
		update("submitted", "设备已接受请求，正在确认网络状态", ref)
	} else {
		update("unknown", "设备请求结果未确认，系统不会自动重发，请勿立即重复发送", ref)
	}
	queryCtx, queryCancel := context.WithTimeout(ctx, 3*time.Second)
	defer queryCancel()
	state, e := backend.ReadSMSState(queryCtx, ref)
	if e == nil && state == smsStateSent {
		update("sent", "", ref)
	}
}

func (a *App) reconcileSMS(ctx context.Context) {
	if a.outbox == nil {
		return
	}
	backend, ok := a.backend.(SMSBackend)
	if !ok {
		return
	}
	items := a.outbox.List()
	if len(items) == 0 {
		return
	}
	checked := 0
	start := a.smsCursor
	for i := 0; i < len(items) && checked < 3; i++ {
		index := (start + i) % len(items)
		r := items[index]
		if (r.Status != "submitted" && r.Status != "unknown") || r.Ref.Path == "" || time.Since(r.Created) > 24*time.Hour {
			continue
		}
		checked++
		a.smsCursor = (index + 1) % len(items)
		q, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		state, err := backend.ReadSMSState(q, r.Ref)
		cancel()
		if err == nil && state == smsStateSent {
			a.outbox.Update(r.ID, "sent", "", r.Ref)
		}
	}
}

func (a *App) allMessages() []Message {
	items := a.inbox.List()
	if a.outbox != nil {
		items = append(items, a.outbox.Messages()...)
	}
	return items
}
