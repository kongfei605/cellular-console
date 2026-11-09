package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kongfei605/cellular-console/internal/atmodem"
	"github.com/kongfei605/cellular-console/internal/hostconfig"
)

type atSession interface {
	Command(context.Context, string, []byte) ([]string, error)
	Alive() bool
	Close() error
}
type preparedATMessage struct {
	parts           []atmodem.Submit
	attempted, sent bool
}
type ATBackend struct {
	mu                         sync.Mutex
	helper, location, expected string
	device                     atmodem.Device
	client                     atSession
	owner                      string
	radio                      Radio
	calls                      map[int]Call
	serial                     uint64
	prepared                   map[string]*preparedATMessage
	discover                   func(context.Context) ([]atmodem.Device, error)
	open                       func(string) (atSession, error)
}

func newATBackend(helper, location, expected string) *ATBackend {
	b := &ATBackend{helper: helper, location: location, expected: expected, calls: map[int]Call{}, prepared: map[string]*preparedATMessage{}}
	b.discover = func(ctx context.Context) ([]atmodem.Device, error) { return atmodem.Devices(ctx, helper) }
	b.open = func(location string) (atSession, error) { return atmodem.Open(helper, location) }
	return b
}
func (b *ATBackend) Close() { b.mu.Lock(); defer b.mu.Unlock(); b.closeLocked() }
func (b *ATBackend) closeLocked() {
	if b.client != nil {
		_ = b.client.Close()
	}
	b.client = nil
	b.owner = ""
	b.calls = map[int]Call{}
	b.prepared = map[string]*preparedATMessage{}
}
func (b *ATBackend) command(ctx context.Context, command string) ([]string, error) {
	q, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return b.client.Command(q, command, nil)
}
func atValue(lines []string, prefix string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
func atFields(text string) []string {
	r := csv.NewReader(strings.NewReader(text))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	f, err := r.Read()
	if err != nil {
		return nil
	}
	return f
}
func atInt(text string) int {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return -1
	}
	return n
}
func (b *ATBackend) connect(ctx context.Context) error {
	devices, err := b.discover(ctx)
	if err != nil {
		return err
	}
	var selected *atmodem.Device
	for _, d := range devices {
		if b.location != "" && d.Location != b.location {
			continue
		}
		if selected != nil {
			return errors.New("发现多个 EC20，请使用 -mac-usb-location 指定设备")
		}
		v := d
		selected = &v
	}
	if selected == nil {
		b.closeLocked()
		return errors.New("EC20 USB 未连接")
	}
	if b.client != nil && b.client.Alive() && selected.Session == b.device.Session && selected.Location == b.device.Location {
		return nil
	}
	b.closeLocked()
	b.location = selected.Location
	b.device = *selected
	c, err := b.open(selected.Location)
	if err != nil {
		return err
	}
	b.client = c
	fail := func(err error) error { b.closeLocked(); return err }
	if _, err = b.command(ctx, "AT"); err != nil {
		return fail(errors.New("EC20 AT 接口不可用，可能被其他应用占用；请断开其他 AT 客户端"))
	}
	values := map[string]string{}
	for _, command := range []string{"AT+CGMI", "AT+CGMM", "AT+CGMR", "AT+CGSN"} {
		lines, e := b.command(ctx, command)
		if e != nil {
			return fail(e)
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "+") && line != command {
				values[command] = line
				break
			}
		}
	}
	identity := values["AT+CGSN"]
	if len(identity) < 8 || len(identity) > 32 {
		return fail(errors.New("无法验证模块身份"))
	}
	hash := sha256.Sum256([]byte(identity))
	id := "ec20-" + hex.EncodeToString(hash[:16])
	if b.expected != "" && id != b.expected {
		return fail(errors.New("模块身份与所选设备不一致"))
	}
	if !strings.Contains(strings.ToUpper(values["AT+CGMM"]), "EC20") {
		return fail(errors.New("USB 设备未返回 EC20 型号"))
	}
	// Pin identity after the first successful observation, including on hotplug.
	b.expected = id
	b.owner = randomToken()
	b.radio = Radio{ID: id, SessionID: b.owner, Present: true, Model: values["AT+CGMM"], Manufacturer: values["AT+CGMI"], Firmware: values["AT+CGMR"], PrimaryPort: "USB AT @ " + selected.Location, Location: true, Technology: "—"}
	return nil
}
func (b *ATBackend) reviewed() bool {
	return b.radio.Model == "EC20F" && b.radio.Firmware == hostconfig.ReviewedFirmware
}
func (b *ATBackend) Snapshot(ctx context.Context) (Radio, []Message, []Call, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(ctx); err != nil {
		return Radio{Model: "EC20", State: "控制通道不可用"}, nil, nil, err
	}
	r := b.radio
	r.State = "搜索网络"
	r.SIMReady = false
	r.Messaging = false
	r.Voice = false
	lines, err := b.command(ctx, "AT+CPIN?")
	if err != nil {
		return r, nil, nil, err
	}
	ready := atValue(lines, "+CPIN:") == "READY"
	if !ready {
		r.State = "SIM 未就绪"
	}
	lines, err = b.command(ctx, "AT+CEREG?")
	if err != nil {
		return r, nil, nil, err
	}
	f := atFields(atValue(lines, "+CEREG:"))
	registered := len(f) > 1 && (atInt(f[1]) == 1 || atInt(f[1]) == 5)
	if registered {
		r.State = "已注册"
		r.Technology = "LTE / 4G"
	}
	r.SIMReady = ready && registered
	lines, err = b.command(ctx, "AT+CSQ")
	if err != nil {
		return r, nil, nil, err
	}
	f = atFields(atValue(lines, "+CSQ:"))
	r.Signal = 0
	if len(f) > 0 {
		n := atInt(f[0])
		if n >= 0 && n <= 31 {
			r.Signal = n * 100 / 31
		}
	}
	lines, err = b.command(ctx, "AT+COPS?")
	if err != nil {
		return r, nil, nil, err
	}
	f = atFields(atValue(lines, "+COPS:"))
	if len(f) > 2 {
		r.Operator = f[2]
	}
	calls, err := b.readCalls(ctx)
	if err != nil {
		return r, nil, nil, err
	}
	r.Voice = b.reviewed()
	r.Messaging = b.reviewed()
	var messages []Message
	if ready && b.reviewed() {
		messages, err = b.readMessages(ctx)
		if err != nil {
			return r, nil, calls, err
		}
	}
	b.radio = r
	return r, messages, calls, nil
}
func (b *ATBackend) readCalls(ctx context.Context) ([]Call, error) {
	lines, err := b.command(ctx, "AT+CLCC")
	if err != nil {
		return nil, err
	}
	next := map[int]Call{}
	result := []Call{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		f := atFields(strings.TrimSpace(strings.TrimPrefix(line, "+CLCC:")))
		if len(f) < 5 {
			return nil, errors.New("invalid CLCC response")
		}
		index, status := atInt(f[0]), atInt(f[2])
		if index < 1 || index > 99 || status < 0 || status > 5 || atInt(f[3]) != 0 {
			continue
		}
		direction := "outgoing"
		if atInt(f[1]) == 1 {
			direction = "incoming"
		}
		number := ""
		if len(f) > 5 {
			number = f[5]
			if len(f) > 6 && atInt(f[6]) == 145 && !strings.HasPrefix(number, "+") {
				number = "+" + number
			}
		}
		call, exists := b.calls[index]
		if !exists || call.Number != number || call.Direction != direction {
			b.serial++
			call = Call{ID: strconv.FormatUint(b.serial, 10)}
		}
		call.Number = number
		call.Direction = direction
		call.StateCode = map[int]int{0: 4, 1: 5, 2: 1, 3: 2, 4: 3, 5: 6}[status]
		call.State = map[int]string{0: "通话中", 1: "保持", 2: "拨号中", 3: "等待接听", 4: "来电", 5: "呼叫等待"}[status]
		next[index] = call
		result = append(result, call)
	}
	for index, call := range b.calls {
		if _, ok := next[index]; !ok {
			call.StateCode = 7
			call.State = "已结束"
			result = append(result, call)
		}
	}
	b.calls = next
	return result, nil
}
func (b *ATBackend) actionReady(ctx context.Context) error {
	if err := b.connect(ctx); err != nil {
		return err
	}
	if !b.reviewed() {
		return errors.New("当前固件尚未开放硬件控制")
	}
	lines, err := b.command(ctx, "AT+CPIN?")
	if err != nil {
		return err
	}
	if atValue(lines, "+CPIN:") != "READY" {
		return errors.New("SIM 未就绪")
	}
	lines, err = b.command(ctx, "AT+CEREG?")
	if err != nil {
		return err
	}
	fields := atFields(atValue(lines, "+CEREG:"))
	if len(fields) < 2 || (fields[1] != "1" && fields[1] != "5") {
		return errors.New("移动网络未注册")
	}
	return nil
}
func (b *ATBackend) Dial(ctx context.Context, number string) (string, error) {
	if !phonePattern.MatchString(number) {
		return "", errors.New("invalid telephone number")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.actionReady(ctx); err != nil {
		return "", err
	}
	if _, err := b.readCalls(ctx); err != nil {
		return "", err
	}
	if len(b.calls) > 0 {
		return "", errors.New("已有呼叫")
	}
	if _, err := b.command(ctx, "ATD"+number+";"); err != nil {
		return "", err
	}
	calls, err := b.readCalls(ctx)
	if err != nil {
		return "", err
	}
	for _, call := range calls {
		if call.Direction == "outgoing" && call.StateCode != 7 {
			return call.ID, nil
		}
	}
	return "", errors.New("拨号已提交，但尚未确认呼叫；请刷新状态，不要重复拨号")
}
func (b *ATBackend) Act(ctx context.Context, id, action string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(ctx); err != nil {
		return err
	}
	if !b.reviewed() {
		return errors.New("当前固件尚未开放硬件控制")
	}
	if _, err := b.readCalls(ctx); err != nil {
		return err
	}
	for index, call := range b.calls {
		if call.ID != id {
			continue
		}
		command := ""
		switch action {
		case "accept":
			if call.StateCode != 3 || call.Direction != "incoming" || len(b.calls) != 1 {
				return errors.New("该呼叫已不处于可接听状态")
			}
			command = "ATA"
		case "hangup":
			if len(b.calls) == 1 {
				command = "AT+CHUP"
			} else {
				command = fmt.Sprintf("AT+CHLD=1%d", index)
			}
		default:
			return errors.New("不支持的通话操作")
		}
		_, err := b.command(ctx, command)
		return err
	}
	return errors.New("该呼叫不存在或已失效")
}
func (b *ATBackend) readMessages(ctx context.Context) ([]Message, error) {
	// This firmware accepts CMGL=<stat> without the optional mode argument.
	// Reading may mark module messages as read; local unread state is independent.
	// Messages are never deleted from the SIM/module.
	if _, err := b.command(ctx, "AT+CMGF=0"); err != nil {
		return nil, err
	}
	lines, err := b.command(ctx, "AT+CMGL=4")
	if err != nil {
		return nil, err
	}
	type piece struct {
		delivery atmodem.Delivery
		source   string
	}
	var singles []Message
	groups := map[string]map[int]piece{}
	ambiguous := map[string]bool{}
	for i, line := range lines {
		if !strings.HasPrefix(line, "+CMGL:") || i+1 >= len(lines) {
			continue
		}
		f := atFields(strings.TrimSpace(strings.TrimPrefix(line, "+CMGL:")))
		if len(f) < 2 || atInt(f[1]) > 1 {
			continue
		}
		d, e := atmodem.DecodeDelivery(lines[i+1])
		if e != nil {
			continue
		}
		source := "mac-at:" + f[0]
		if d.Total == 0 {
			singles = append(singles, Message{Sender: d.Sender, Text: d.Text, Timestamp: d.Timestamp, Source: source})
			continue
		}
		key := fmt.Sprintf("%s:%d:%d:%s", d.Sender, d.Reference, d.Total, dayOfSMS(d.Timestamp))
		if groups[key] == nil {
			groups[key] = map[int]piece{}
		}
		if _, exists := groups[key][d.Part]; exists {
			ambiguous[key] = true
		}
		groups[key][d.Part] = piece{d, source}
	}
	for key, parts := range groups {
		if ambiguous[key] {
			continue
		}
		first, ok := parts[1]
		if !ok || len(parts) != first.delivery.Total {
			continue
		}
		text := ""
		complete := true
		for i := 1; i <= first.delivery.Total; i++ {
			p, ok := parts[i]
			if !ok {
				complete = false
				break
			}
			text += p.delivery.Text
		}
		if complete {
			singles = append(singles, Message{Sender: first.delivery.Sender, Text: text, Timestamp: first.delivery.Timestamp, Source: first.source})
		}
	}
	return singles, nil
}
func dayOfSMS(stamp string) string {
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return stamp
}
func (b *ATBackend) PrepareSMS(ctx context.Context, number, text string) (SMSRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.actionReady(ctx); err != nil {
		return SMSRef{}, err
	}
	if len(b.prepared) >= 1024 {
		return SMSRef{}, errors.New("SMS session limit reached; restart the console")
	}
	token := randomToken()
	parts, err := atmodem.EncodeSubmit(number, text, token[0])
	if err != nil {
		return SMSRef{}, err
	}
	b.prepared[token] = &preparedATMessage{parts: parts}
	return SMSRef{Owner: b.owner, Path: token, DeviceID: b.radio.ID}, nil
}
func (b *ATBackend) message(ref SMSRef) (*preparedATMessage, error) {
	if ref.Owner == "" || ref.Owner != b.owner || ref.DeviceID != b.radio.ID || b.client == nil || !b.client.Alive() {
		return nil, errors.New("SMS session changed; submission is not retried")
	}
	m := b.prepared[ref.Path]
	if m == nil {
		return nil, errors.New("unknown SMS handle")
	}
	return m, nil
}
func (b *ATBackend) SubmitSMS(ctx context.Context, ref SMSRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.actionReady(ctx); err != nil {
		return err
	}
	m, err := b.message(ref)
	if err != nil {
		return err
	}
	if m.attempted {
		return errors.New("SMS submission already attempted")
	}
	m.attempted = true
	if _, err := b.readCalls(ctx); err != nil {
		return err
	}
	if len(b.calls) > 0 {
		return errors.New("当前有呼叫，未发送短信")
	}
	if _, err := b.command(ctx, "AT+CMGF=0"); err != nil {
		return err
	}
	for _, part := range m.parts {
		lines, e := b.client.Command(ctx, fmt.Sprintf("AT+CMGS=%d", part.Length), []byte(part.Hex))
		if e != nil {
			return e
		}
		if atValue(lines, "+CMGS:") == "" {
			return errors.New("SMS network reference missing")
		}
	}
	m.sent = true
	return nil
}
func (b *ATBackend) ReadSMSState(ctx context.Context, ref SMSRef) (uint32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m, err := b.message(ref)
	if err != nil {
		return 0, err
	}
	if m.sent {
		return smsStateSent, nil
	}
	return 0, errors.New("SMS result not confirmed")
}
func (b *ATBackend) ReadLocation(ctx context.Context) (LocationStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(ctx); err != nil {
		return LocationStatus{}, err
	}
	return b.locationLocked(ctx)
}
func (b *ATBackend) locationLocked(ctx context.Context) (LocationStatus, error) {
	s := LocationStatus{Observed: time.Now().UTC(), Reviewed: b.reviewed(), ManagerVersion: "macOS USB AT", Capabilities: gpsManaged, cacheIdentity: b.owner + ":" + b.radio.ID}
	lines, err := b.command(ctx, "AT+QGPS?")
	if err != nil {
		return s, err
	}
	s.Available = true
	s.GNSSOn = atValue(lines, "+QGPS:") == "1"
	if s.GNSSOn {
		s.EnabledSources = gpsManaged
		for _, kind := range []string{"RMC", "GGA", "GSA", "GSV"} {
			lines, e := b.command(ctx, "AT+QGPSGNMEA=\""+kind+"\"")
			if e != nil {
				var atErr *atmodem.Error
				if errors.As(e, &atErr) {
					continue
				}
				return s, e
			}
			for _, line := range lines {
				if strings.HasPrefix(line, "+QGPSGNMEA:") {
					s.NMEA += strings.TrimSpace(strings.TrimPrefix(line, "+QGPSGNMEA:")) + "\r\n"
				}
			}
		}
	}
	parseNMEA(&s, time.Now().UTC())
	return s, nil
}
func (b *ATBackend) SetGNSS(ctx context.Context, on bool) (LocationStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.actionReady(ctx); err != nil {
		return LocationStatus{}, err
	}
	s, err := b.locationLocked(ctx)
	if err != nil {
		return s, err
	}
	if on == s.GNSSOn {
		return s, nil
	}
	if _, err = b.readCalls(ctx); err != nil {
		return s, err
	}
	if len(b.calls) > 0 {
		return s, errors.New("当前有呼叫")
	}
	if on {
		lines, e := b.command(ctx, "AT+QGPSCFG=\"nmeasrc\"")
		if e != nil {
			return s, e
		}
		f := atFields(atValue(lines, "+QGPSCFG:"))
		if len(f) != 2 || f[1] != "1" {
			return s, errors.New("NMEA 查询源未开放，请先核查模块配置")
		}
	}
	command := "AT+QGPSEND"
	if on {
		command = "AT+QGPS=1"
	}
	if _, err = b.command(ctx, command); err != nil {
		return s, err
	}
	after, err := b.locationLocked(ctx)
	if err == nil && after.GNSSOn != on {
		err = errors.New("GNSS setting not confirmed")
	}
	return after, err
}

var _ Backend = (*ATBackend)(nil)
var _ SMSBackend = (*ATBackend)(nil)
var _ LocationBackend = (*ATBackend)(nil)
