package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const destination = "org.freedesktop.ModemManager1"
const modemIF = destination + ".Modem"
const voiceIF = modemIF + ".Voice"
const smsIF = destination + ".Sms"
const callIF = destination + ".Call"

type Backend interface {
	Snapshot(context.Context) (Radio, []Message, []Call, error)
	Dial(context.Context, string) (string, error)
	Act(context.Context, string, string) error
	Close()
}

type objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

func stringProp(p map[string]dbus.Variant, key string) string {
	if v, ok := p[key]; ok {
		return fmt.Sprint(v.Value())
	}
	return ""
}
func number(v any) int {
	switch n := v.(type) {
	case uint32:
		return int(n)
	case int32:
		return int(n)
	case uint64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
func numProp(p map[string]dbus.Variant, key string) int   { return number(p[key].Value()) }
func boolProp(p map[string]dbus.Variant, key string) bool { b, _ := p[key].Value().(bool); return b }
func pathList(p map[string]dbus.Variant, key string) []dbus.ObjectPath {
	a, _ := p[key].Value().([]dbus.ObjectPath)
	return a
}
func shortID(p dbus.ObjectPath) string { v := strings.Split(string(p), "/"); return v[len(v)-1] }

func selectModem(all objects, expected string) (dbus.ObjectPath, error) {
	var matches []dbus.ObjectPath
	for path, ifaces := range all {
		p, ok := ifaces[modemIF]
		if !ok {
			continue
		}
		id := stringProp(p, "DeviceIdentifier")
		if expected != "" && id == expected {
			return path, nil
		}
		if expected == "" && strings.Contains(strings.ToUpper(stringProp(p, "Model")), "EC20") {
			matches = append(matches, path)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", errors.New("发现多个 EC20，请配置设备标识")
	}
	return "", nil
}

func parseObjects(all objects, expected string) (dbus.ObjectPath, Radio, []Message, []Call, error) {
	path, err := selectModem(all, expected)
	r := Radio{Model: "EC20", State: "未连接", Technology: "—"}
	msgs := []Message{}
	calls := []Call{}
	if err != nil || path == "" {
		return path, r, msgs, calls, err
	}
	ifaces := all[path]
	p := ifaces[modemIF]
	r.Location = ifaces[modemIF+".Location"] != nil
	r.Present = true
	r.ID = stringProp(p, "DeviceIdentifier")
	r.Model = stringProp(p, "Model")
	r.Manufacturer = stringProp(p, "Manufacturer")
	r.Firmware = stringProp(p, "Revision")
	r.PrimaryPort = stringProp(p, "PrimaryPort")
	states := map[int]string{-1: "故障", 0: "未知", 1: "初始化", 2: "待解锁", 3: "已禁用", 4: "关闭中", 5: "启用中", 6: "已启用", 7: "搜索网络", 8: "已注册", 9: "断开中", 10: "连接中", 11: "数据已连接"}
	r.State = states[numProp(p, "State")]
	r.SIMReady = numProp(p, "UnlockRequired") == 1 && numProp(p, "State") >= 8
	if v, ok := p["SignalQuality"].Value().([]interface{}); ok && len(v) > 0 {
		r.Signal = number(v[0])
	}
	if numProp(p, "AccessTechnologies")&16384 != 0 {
		r.Technology = "LTE / 4G"
	} else if numProp(p, "AccessTechnologies") != 0 {
		r.Technology = "蜂窝网络"
	}
	if p3, ok := ifaces[modemIF+".Modem3gpp"]; ok {
		r.Operator = stringProp(p3, "OperatorName")
	}
	if pm, ok := ifaces[modemIF+".Messaging"]; ok {
		r.Messaging = true
		for _, sp := range pathList(pm, "Messages") {
			s := all[sp][smsIF]
			if numProp(s, "State") != 3 || (numProp(s, "PduType") != 1 && numProp(s, "PduType") != 32) {
				continue
			}
			text := stringProp(s, "Text")
			if text == "" {
				continue
			}
			msgs = append(msgs, Message{Sender: stringProp(s, "Number"), Text: text, Timestamp: stringProp(s, "Timestamp"), Source: string(sp)})
		}
	}
	if pv, ok := ifaces[voiceIF]; ok {
		r.Voice = true
		r.EmergencyOnly = boolProp(pv, "EmergencyOnly")
		for _, cp := range pathList(pv, "Calls") {
			p := all[cp][callIF]
			if p == nil {
				continue
			}
			code := numProp(p, "State")
			state := map[int]string{0: "准备中", 1: "拨号中", 2: "等待接听", 3: "来电", 4: "通话中", 5: "保持", 6: "呼叫等待", 7: "已结束"}[code]
			direction := "outgoing"
			if numProp(p, "Direction") == 1 {
				direction = "incoming"
			}
			calls = append(calls, Call{ID: shortID(cp), Number: stringProp(p, "Number"), Direction: direction, State: state, StateCode: code, AudioPort: stringProp(p, "AudioPort")})
		}
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].ID > calls[j].ID })
	return path, r, msgs, calls, nil
}

type ModemBackend struct {
	mu       sync.Mutex
	conn     *dbus.Conn
	expected string
}

func hydrateChildren(all objects, path dbus.ObjectPath, fetch func(dbus.ObjectPath, string) (map[string]dbus.Variant, error)) error {
	if path == "" {
		return nil
	}
	for _, pair := range []struct{ parent, list, child string }{
		{modemIF + ".Messaging", "Messages", smsIF},
		{voiceIF, "Calls", callIF},
	} {
		for _, child := range pathList(all[path][pair.parent], pair.list) {
			if all[child][pair.child] != nil {
				continue
			}
			properties, err := fetch(child, pair.child)
			if err != nil {
				var busError dbus.Error
				if errors.As(err, &busError) && (busError.Name == "org.freedesktop.DBus.Error.UnknownObject" || busError.Name == "org.freedesktop.DBus.Error.UnknownInterface") {
					continue
				}
				return fmt.Errorf("read modem child properties: %w", err)
			}
			if all[child] == nil {
				all[child] = map[string]map[string]dbus.Variant{}
			}
			all[child][pair.child] = properties
		}
	}
	return nil
}

func (b *ModemBackend) connect() error {
	if b.conn != nil && b.conn.Connected() {
		return nil
	}
	if b.conn != nil {
		b.conn.Close()
	}
	c, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	b.conn = c
	return nil
}
func (b *ModemBackend) get(ctx context.Context) (objects, error) {
	if err := b.connect(); err != nil {
		return nil, err
	}
	var all objects
	err := b.conn.Object(destination, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&all)
	if err != nil {
		return nil, err
	}
	path, err := selectModem(all, b.expected)
	if err != nil {
		return nil, err
	}
	err = hydrateChildren(all, path, func(child dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
		var p map[string]dbus.Variant
		e := b.conn.Object(destination, child).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, iface).Store(&p)
		return p, e
	})
	return all, err
}
func (b *ModemBackend) Snapshot(ctx context.Context) (Radio, []Message, []Call, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(); err != nil {
		return Radio{}, nil, nil, err
	}
	var owner string
	if err := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, destination).Store(&owner); err != nil {
		return Radio{}, nil, nil, err
	}
	all, err := b.get(ctx)
	if err != nil {
		return Radio{}, nil, nil, err
	}
	_, r, m, c, err := parseObjects(all, b.expected)
	if err != nil {
		return r, m, c, err
	}
	var after string
	if err = b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, destination).Store(&after); err != nil {
		return Radio{}, nil, nil, err
	}
	if owner != after {
		return Radio{}, nil, nil, errors.New("modem service changed during snapshot")
	}
	r.SessionID = owner
	return r, m, c, nil
}
func (b *ModemBackend) Dial(ctx context.Context, phone string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	all, err := b.get(ctx)
	if err != nil {
		return "", err
	}
	path, r, _, calls, err := parseObjects(all, b.expected)
	if err != nil {
		return "", err
	}
	if !r.Present || !r.Voice || !r.SIMReady || r.EmergencyOnly {
		return "", errors.New("当前设备无法拨打普通电话")
	}
	for _, c := range calls {
		if c.StateCode != 7 {
			return "", errors.New("已有呼叫，请先结束当前呼叫")
		}
	}
	var call dbus.ObjectPath
	err = b.conn.Object(destination, path).CallWithContext(ctx, voiceIF+".CreateCall", 0, map[string]dbus.Variant{"number": dbus.MakeVariant(phone)}).Store(&call)
	if err != nil {
		return "", err
	}
	err = b.conn.Object(destination, call).CallWithContext(ctx, callIF+".Start", 0).Err
	return shortID(call), err
}
func (b *ModemBackend) Act(ctx context.Context, id, action string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	all, err := b.get(ctx)
	if err != nil {
		return err
	}
	_, r, _, calls, err := parseObjects(all, b.expected)
	if err != nil {
		return err
	}
	if !r.Present {
		return errors.New("EC20 未连接")
	}
	for _, c := range calls {
		if c.ID != id {
			continue
		}
		method := "Hangup"
		if action == "accept" {
			if c.StateCode != 3 || c.Direction != "incoming" {
				return errors.New("该呼叫已不处于可接听状态")
			}
			method = "Accept"
		} else if action != "hangup" {
			return errors.New("不支持的通话操作")
		}
		if c.StateCode == 7 {
			return errors.New("该呼叫已经结束")
		}
		return b.conn.Object(destination, dbus.ObjectPath("/org/freedesktop/ModemManager1/Call/"+id)).CallWithContext(ctx, callIF+"."+method, 0).Err
	}
	return errors.New("该呼叫不存在或已失效，请刷新")
}
func (b *ModemBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		b.conn.Close()
	}
}

type DemoBackend struct {
	gnssOn    bool
	sessionID string
	mu        sync.Mutex
	calls     []Call
	serial    int
}

func (b *DemoBackend) Snapshot(ctx context.Context) (Radio, []Message, []Call, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionID == "" {
		b.sessionID = randomToken()
	}
	r := Radio{SessionID: b.sessionID, Present: true, ID: "demo-ec20", Model: "EC20F", Manufacturer: "Quectel", Firmware: "EC20CEFAGR08A03M4G", Operator: "CHN-UNICOM", Technology: "LTE / 4G", Signal: 80, State: "数据已连接", SIMReady: true, Messaging: true, Voice: true, PrimaryPort: "ttyUSB2"}
	m := []Message{{Sender: "10010", Text: "【演示短信】您的蜂窝网络已连接。本页面使用模拟设备，不会实际拨号或发送短信。", Timestamp: "2026-11-06T20:40:00+08:00"}, {Sender: "测试联系人", Text: "【演示短信】周末调试时，备用网络保持在线。", Timestamp: "2026-11-06T19:12:00+08:00"}}
	m = append(m, Message{Sender: "10010", Text: "【演示短信】前一日网络服务提示。短信会按发送号码聚合，并按日期显示。", Timestamp: "2026-11-05T09:30:00+08:00"}, Message{Sender: "+8613800000000", Text: "【演示短信】这是带 +86 的号码。", Timestamp: "2026-11-05T12:00:00+08:00"}, Message{Sender: "13800000000", Text: "【演示短信】同一手机号码的本地写法会显示在同一个会话。", Timestamp: "2026-11-06T18:20:00+08:00"})
	return r, m, append([]Call{}, b.calls...), nil
}
func (b *DemoBackend) Dial(ctx context.Context, phone string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.serial++
	id := fmt.Sprint(b.serial)
	b.calls = append([]Call{{ID: id, Number: phone, Direction: "outgoing", State: "通话中", StateCode: 4}}, b.calls...)
	return id, nil
}
func (b *DemoBackend) Act(ctx context.Context, id, action string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.calls {
		if b.calls[i].ID == id {
			if action == "accept" {
				b.calls[i].State = "通话中"
				b.calls[i].StateCode = 4
			} else {
				b.calls[i].State = "已结束"
				b.calls[i].StateCode = 7
			}
			return nil
		}
	}
	return errors.New("呼叫不存在")
}
func (b *DemoBackend) Close() {}
func demoFailover() Failover {
	return Failover{Active: "wired", WiredHealthy: true, WiredLink: true, WiredIP: "192.0.2.20", WiredGateway: "192.0.2.1", CellularIP: "198.51.100.20", CellularPresent: true, CellularHealthy: true, ConnectionState: "connected", VPNs: map[string]bool{"tun0": true, "tun1": true, "tun2": true}, Updated: time.Now().Format("2006-01-02T15:04:05-0700"), LastSwitch: "2026-11-06T21:51:18+0800"}
}
