package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const locationIF = modemIF + ".Location"
const gpsManaged uint32 = 6

type Position struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Altitude  *float64  `json:"altitude,omitempty"`
	UTC       time.Time `json:"utc"`
}
type LocationStatus struct {
	Available         bool      `json:"available"`
	Capabilities      uint32    `json:"capabilities"`
	EnabledSources    uint32    `json:"enabled_sources"`
	Signals           bool      `json:"signals"`
	RefreshSeconds    uint32    `json:"refresh_seconds"`
	ManagerVersion    string    `json:"manager_version"`
	Reviewed          bool      `json:"reviewed_profile"`
	GNSSOn            bool      `json:"gnss_on"`
	Fix               bool      `json:"fix"`
	FixReason         string    `json:"fix_reason"`
	Position          *Position `json:"position,omitempty"`
	SatellitesUsed    *int      `json:"satellites_used,omitempty"`
	SatellitesVisible *int      `json:"satellites_visible,omitempty"`
	HDOP              *float64  `json:"hdop,omitempty"`
	NMEA              string    `json:"nmea"`
	Sentences         int       `json:"sentences"`
	InvalidSentences  int       `json:"invalid_sentences"`
	Observed          time.Time `json:"observed"`
	EnableAllowed     bool      `json:"enable_allowed"`
	ControlReason     string    `json:"control_reason"`
	ControlEnabled    bool      `json:"control_enabled"`
	Demo              bool      `json:"demo"`
}
type LocationBackend interface {
	ReadLocation(context.Context) (LocationStatus, error)
	SetGNSS(context.Context, bool) (LocationStatus, error)
}

func nmeaFields(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 10 || len(line) > 160 || line[0] != '$' {
		return nil, false
	}
	star := strings.LastIndexByte(line, '*')
	if star < 6 || len(line) != star+3 {
		return nil, false
	}
	checksum, e := strconv.ParseUint(line[star+1:], 16, 8)
	if e != nil {
		return nil, false
	}
	var sum byte
	for i := 1; i < star; i++ {
		sum ^= line[i]
	}
	if sum != byte(checksum) {
		return nil, false
	}
	fields := strings.Split(line[1:star], ",")
	if len(fields[0]) != 5 {
		return nil, false
	}
	return fields, true
}
func nmeaCoordinate(value, hemisphere string, latitude bool) (float64, bool) {
	if !nmeaDecimal(value) {
		return 0, false
	}
	n, e := strconv.ParseFloat(value, 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, false
	}
	deg := math.Floor(n / 100)
	minutes := n - deg*100
	limit := 180.
	if latitude {
		limit = 90
	}
	if minutes >= 60 || deg > limit || (deg == limit && minutes != 0) {
		return 0, false
	}
	n = deg + minutes/60
	if latitude {
		if hemisphere == "S" {
			n = -n
		} else if hemisphere != "N" {
			return 0, false
		}
	} else {
		if hemisphere == "W" {
			n = -n
		} else if hemisphere != "E" {
			return 0, false
		}
	}
	return n, true
}
func nmeaTime(date, clock string) (time.Time, bool) {
	if len(date) != 6 || len(clock) < 6 {
		return time.Time{}, false
	}
	if !nmeaDecimal(clock) || (len(clock) > 6 && clock[6] != '.') {
		return time.Time{}, false
	}
	for _, c := range date {
		if c < '0' || c > '9' {
			return time.Time{}, false
		}
	}
	if _, e := strconv.Atoi(date + clock[:6]); e != nil {
		return time.Time{}, false
	}
	d, e := time.Parse("020106150405", date+clock[:6])
	if e != nil || d.Year() < 2020 {
		return time.Time{}, false
	}
	return d.UTC(), true
}
func nmeaDecimal(value string) bool {
	if value == "" {
		return false
	}
	dot := false
	for i, c := range value {
		if c == '.' && !dot && i > 0 && i < len(value)-1 {
			dot = true
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func finiteNumber(value string, lo, hi float64) *float64 {
	n, e := strconv.ParseFloat(value, 64)
	if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < lo || n > hi {
		return nil
	}
	return &n
}
func nmeaInt(value string, max int) *int {
	n, e := strconv.Atoi(value)
	if e != nil || n < 0 || n > max {
		return nil
	}
	return &n
}
func parseNMEA(s *LocationStatus, now time.Time) {
	s.Observed = now
	s.Fix = false
	s.Position = nil
	s.Sentences = 0
	s.InvalidSentences = 0
	s.SatellitesUsed = nil
	s.SatellitesVisible = nil
	s.HDOP = nil
	s.FixReason = "尚未收到有效NMEA数据"
	var rmc, gga []string
	visible := map[string]int{}
	for _, line := range strings.Split(strings.ReplaceAll(s.NMEA, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields, ok := nmeaFields(line)
		if !ok {
			s.InvalidSentences++
			continue
		}
		s.Sentences++
		tag := fields[0][2:]
		switch tag {
		case "RMC":
			if len(fields) >= 10 {
				rmc = fields
			}
		case "GGA":
			if len(fields) >= 11 {
				gga = fields
				s.SatellitesUsed = nmeaInt(fields[7], 99)
				s.HDOP = finiteNumber(fields[8], 0, 100)
			}
		case "GSV":
			if len(fields) >= 4 {
				if n := nmeaInt(fields[3], 200); n != nil {
					visible[fields[0][:2]] = *n
				}
			}
		}
	}
	if len(visible) > 0 {
		n := 0
		if combined, ok := visible["GN"]; ok {
			n = combined
		} else {
			for _, count := range visible {
				n += count
			}
		}
		s.SatellitesVisible = &n
	}
	if !s.GNSSOn {
		s.FixReason = "GNSS未启用，缓存数据不作为当前位置"
		return
	}
	if len(rmc) == 0 {
		s.FixReason = "GNSS已启用，尚未收到带日期的RMC定位信息"
		return
	}
	stamp, ok := nmeaTime(rmc[9], rmc[1])
	if !ok {
		s.FixReason = "等待卫星时间与有效定位"
		return
	}
	if now.Sub(stamp) > 90*time.Second || stamp.Sub(now) > 5*time.Second {
		s.FixReason = "NMEA定位时间已过期或异常，未展示为当前位置"
		return
	}
	if rmc[2] != "A" {
		s.FixReason = "GNSS正在搜星，RMC尚无有效定位"
		return
	}
	if len(rmc) > 12 {
		switch rmc[12] {
		case "", "A", "D", "F", "R", "P":
		default:
			s.FixReason = "RMC报告估算、手动、模拟或未知定位模式，未作为卫星定位展示"
			return
		}
	}
	lat, a := nmeaCoordinate(rmc[3], rmc[4], true)
	lon, b := nmeaCoordinate(rmc[5], rmc[6], false)
	if !a || !b {
		s.FixReason = "定位坐标格式无效"
		return
	}
	p := &Position{Latitude: lat, Longitude: lon, UTC: stamp}
	if len(gga) > 0 {
		same := len(gga[1]) >= 6 && len(rmc[1]) >= 6 && gga[1][:6] == rmc[1][:6]
		if same {
			q, e := strconv.Atoi(gga[6])
			if e != nil || q < 1 || q > 5 {
				s.FixReason = "GGA报告未定位，暂不展示坐标"
				return
			}
			if gga[10] == "M" {
				p.Altitude = finiteNumber(gga[9], -1500, 100000)
			}
		}
	}
	s.Position = p
	s.Fix = true
	s.FixReason = "有效GNSS定位（WGS84），以卫星UTC时间核验"
}

func (b *ModemBackend) locationObject(ctx context.Context) (dbus.BusObject, LocationStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := LocationStatus{Observed: time.Now().UTC()}
	if e := b.connect(); e != nil {
		return nil, s, e
	}
	var owner string
	if e := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, destination).Store(&owner); e != nil {
		return nil, s, e
	}
	var all objects
	if e := b.conn.Object(owner, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", dbus.FlagNoAutoStart).Store(&all); e != nil {
		return nil, s, e
	}
	path, r, _, _, e := parseObjects(all, b.expected)
	if e != nil {
		return nil, s, e
	}
	if path == "" || !r.Present {
		return nil, s, nil
	}
	props := all[path][locationIF]
	if props == nil {
		return nil, s, nil
	}
	s.Available = true
	s.Capabilities = uint32(numProp(props, "Capabilities"))
	s.EnabledSources = uint32(numProp(props, "Enabled"))
	s.Signals = boolProp(props, "SignalsLocation")
	s.RefreshSeconds = uint32(numProp(props, "GpsRefreshRate"))
	s.GNSSOn = s.EnabledSources&gpsManaged != 0
	for _, ifaces := range all {
		if p := ifaces[destination]; p != nil {
			s.ManagerVersion = stringProp(p, "Version")
		}
	}
	if s.ManagerVersion == "" {
		var p map[string]dbus.Variant
		if e = b.conn.Object(owner, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, destination).Store(&p); e != nil {
			return nil, s, e
		}
		s.ManagerVersion = stringProp(p, "Version")
	}
	s.Reviewed = r.Model == "EC20F" && r.Firmware == "EC20CEFAGR08A03M4G" && s.ManagerVersion == "1.20.4" && s.Capabilities&gpsManaged == gpsManaged
	return b.conn.Object(owner, path), s, nil
}
func (b *ModemBackend) ReadLocation(ctx context.Context) (LocationStatus, error) {
	obj, s, e := b.locationObject(ctx)
	if e != nil || !s.Available {
		return s, e
	}
	var data map[uint32]dbus.Variant
	if e = obj.CallWithContext(ctx, locationIF+".GetLocation", dbus.FlagNoAutoStart).Store(&data); e != nil {
		return s, e
	}
	if text, ok := data[4].Value().(string); ok {
		if len(text) > 16384 {
			text = text[:16384]
		}
		s.NMEA = text
	}
	parseNMEA(&s, time.Now().UTC())
	return s, nil
}
func (b *ModemBackend) SetGNSS(ctx context.Context, on bool) (LocationStatus, error) {
	_, s, e := b.locationObject(ctx)
	if e != nil {
		return s, e
	}
	next, e := gnssSources(s, on)
	if e != nil {
		return s, e
	}
	if next != s.EnabledSources {
		if e = requestGNSSBroker(ctx, on, next); e != nil {
			return s, e
		}
	}
	return b.ReadLocation(ctx)
}
func requestGNSSBroker(ctx context.Context, on bool, expected uint32) error {
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/run/ec20-gnss-control.sock")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	body, _ := json.Marshal(map[string]bool{"enabled": on})
	r, e := http.NewRequestWithContext(ctx, "POST", "http://gnss.local/gnss", bytes.NewReader(body))
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/json")
	response, e := client.Do(r)
	if e != nil {
		return errors.New("GNSS本机代理不可用")
	}
	defer response.Body.Close()
	var result struct {
		Sources uint32 `json:"enabled_sources"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&result) != nil || result.Sources != expected {
		return errors.New("GNSS本机代理未确认设置")
	}
	return nil
}
func gnssSources(s LocationStatus, on bool) (uint32, error) {
	if !s.Available {
		return s.EnabledSources, errors.New("定位接口不可用")
	}
	partial := s.EnabledSources&gpsManaged != 0 && s.EnabledSources&gpsManaged != gpsManaged
	if on && (!s.Reviewed || s.Signals || s.EnabledSources&16 != 0 || partial) {
		return s.EnabledSources, errors.New("定位配置不在已验证范围内，未执行启用")
	}
	if on {
		return s.EnabledSources | gpsManaged, nil
	}
	return s.EnabledSources &^ gpsManaged, nil
}
func (b *DemoBackend) ReadLocation(ctx context.Context) (LocationStatus, error) {
	b.mu.Lock()
	on := b.gnssOn
	b.mu.Unlock()
	s := LocationStatus{Available: true, Reviewed: true, ManagerVersion: "1.20.4", Capabilities: 23, EnabledSources: 1, RefreshSeconds: 30, GNSSOn: on, Demo: true}
	if on {
		s.EnabledSources = 7
		now := time.Now().UTC()
		s.NMEA = nmeaLine("GPRMC,"+now.Format("150405")+",A,4807.038,N,01131.000,E,0.0,0.0,"+now.Format("020106")+",,,A") + "\r\n" + nmeaLine("GPGGA,"+now.Format("150405")+",4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,") + "\r\n" + nmeaLine("GPGSV,1,1,08,01,40,083,42")
	}
	parseNMEA(&s, time.Now().UTC())
	return s, nil
}
func nmeaLine(body string) string {
	var sum byte
	for i := range body {
		sum ^= body[i]
	}
	return "$" + body + "*" + strings.ToUpper(strconv.FormatUint(uint64(sum)+256, 16)[1:])
}
func (b *DemoBackend) SetGNSS(ctx context.Context, on bool) (LocationStatus, error) {
	b.mu.Lock()
	b.gnssOn = on
	b.mu.Unlock()
	return b.ReadLocation(ctx)
}

func locationPolicy(r Radio, f Failover, fe error, calls []Call, last time.Time, l LocationStatus) string {
	if !l.Available {
		return "EC20定位接口未就绪"
	}
	if !l.Reviewed {
		return "固件或ModemManager版本未经过本机无重启验证"
	}
	if l.Signals {
		return "外部已启用位置广播，请先由管理员核查"
	}
	if l.EnabledSources&16 != 0 {
		return "外部正在使用非托管GPS，未修改其配置"
	}
	if l.EnabledSources&gpsManaged != 0 && l.EnabledSources&gpsManaged != gpsManaged {
		return "外部正在使用部分GPS数据源，请先由管理员核查"
	}
	if fe != nil || f.Active != "wired" || !f.WiredHealthy {
		return "有线主用且状态新鲜时才能启用GNSS"
	}
	if !r.Present || !f.CellularPresent || !f.CellularHealthy || f.CellularIP == "" {
		return "EC20蜂窝备用尚未恢复健康，暂不启用GNSS"
	}
	if last.IsZero() || time.Since(last) > 15*time.Second || time.Until(last) > time.Second {
		return "设备状态过期，请稍候刷新"
	}
	for _, c := range calls {
		if c.StateCode != 7 {
			return "当前有呼叫，请结束后再启用GNSS"
		}
	}
	return ""
}
func (a *App) locationStatus(ctx context.Context) (LocationStatus, error) {
	backend, ok := a.backend.(LocationBackend)
	if !ok {
		return LocationStatus{}, errors.New("当前后端不支持定位")
	}
	s, e := backend.ReadLocation(ctx)
	if e != nil {
		return s, e
	}
	f, fe := a.failover()
	state := a.state()
	s.ControlEnabled = a.locationControl
	s.ControlReason = locationPolicy(state.Radio, f, fe, state.Calls, state.LastModemSuccess, s)
	if !a.locationControl {
		s.ControlReason = "本机尚未开放GNSS启用，先完成保护验证"
	}
	a.actionMu.Lock()
	busy := a.locationBusy
	a.actionMu.Unlock()
	if busy {
		s.ControlReason = "GNSS设置正在处理"
	}
	s.EnableAllowed = s.ControlReason == ""
	return s, nil
}
func (a *App) locationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/location", a.protected(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		s, e := a.locationStatus(ctx)
		if e != nil {
			apiError(w, 503, "无法读取定位，请检查设备与定位权限；其他功能继续运行")
			return
		}
		writeJSON(w, 200, s)
	}))
	mux.HandleFunc("POST /api/location/control", a.protected(a.controlLocation))
}
func (a *App) controlLocation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if readJSON(w, r, &input) != nil || input.Enabled == nil {
		apiError(w, 400, "定位操作格式无效")
		return
	}
	backend, ok := a.backend.(LocationBackend)
	if !ok || !a.locationControl {
		apiError(w, 409, "本机尚未开放GNSS设置")
		return
	}
	a.actionMu.Lock()
	a.smsMu.Lock()
	busy := a.smsActive || a.locationBusy
	a.smsMu.Unlock()
	if busy {
		a.actionMu.Unlock()
		apiError(w, 409, "短信或GNSS设置正在处理，请稍候")
		return
	}
	a.locationBusy = true
	a.actionMu.Unlock()
	defer func() { a.actionMu.Lock(); a.locationBusy = false; a.actionMu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	before, e := backend.ReadLocation(ctx)
	if e != nil {
		apiError(w, 503, "无法读取原定位配置，未执行设置")
		return
	}
	f, fe := a.failover()
	snapshot := a.state()
	if *input.Enabled {
		if reason := locationPolicy(snapshot.Radio, f, fe, snapshot.Calls, snapshot.LastModemSuccess, before); reason != "" {
			apiError(w, 409, reason)
			return
		}
	}
	after, e := backend.SetGNSS(ctx, *input.Enabled)
	if e != nil {
		apiError(w, 502, "GNSS设置未确认；未重启模块，请先刷新状态，不要连续重试")
		return
	}
	if *input.Enabled {
		latest, err := a.failover()
		if err != nil || !latest.WiredHealthy || latest.Active != "wired" || !latest.CellularHealthy || latest.CellularIP != f.CellularIP {
			if !before.GNSSOn {
				restore, done := context.WithTimeout(context.Background(), 10*time.Second)
				_, _ = backend.SetGNSS(restore, false)
				done()
			}
			apiError(w, 503, "网络保护状态发生变化，已尝试恢复原GNSS开关，请刷新核查")
			return
		}
	}
	after.ControlEnabled = a.locationControl
	after.EnableAllowed = false
	after.ControlReason = "请刷新状态后再操作"
	writeJSON(w, 200, after)
}
