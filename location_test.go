package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNMEAFixValidationAndFreshness(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 20, 30, 0, time.UTC)
	rmc := func(clock, date, status, lat, lon string) string {
		return nmeaLine("GPRMC," + clock + "," + status + "," + lat + ",N," + lon + ",E,0,0," + date + ",,,A")
	}
	good := rmc("102030.00", "071126", "A", "4807.038", "01131.000")
	gga := nmeaLine("GPGGA,102030,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,")
	for _, tc := range []struct {
		name, nmea string
		on, want   bool
	}{
		{"valid", good + "\r\n" + gga, true, true},
		{"zero coordinates valid", rmc("102030", "071126", "A", "0000.000", "00000.000"), true, true},
		{"no fix", rmc("102030", "071126", "V", "4807.038", "01131.000"), true, false},
		{"expired", rmc("101800", "071126", "A", "4807.038", "01131.000"), true, false},
		{"future", rmc("102040", "071126", "A", "4807.038", "01131.000"), true, false},
		{"missing date", rmc("102030", "", "A", "4807.038", "01131.000"), true, false},
		{"invalid date", rmc("102030", "321126", "A", "4807.038", "01131.000"), true, false},
		{"bad checksum", good[:len(good)-2] + "XX", true, false},
		{"invalid coordinate", rmc("102030", "071126", "A", "4867.038", "01131.000"), true, false},
		{"nonfinite coordinate", rmc("102030", "071126", "A", "NaN", "01131.000"), true, false},
		{"disabled cache", good, false, false},
		{"simulated mode", nmeaLine("GPRMC,102030,A,4807.038,N,01131.000,E,0,0,071126,,,S"), true, false},
		{"manual quality", good + "\n" + nmeaLine("GPGGA,102030,4807.038,N,01131.000,E,7,08,0.9,545.4,M,46.9,M,,"), true, false},
		{"exponent coordinate", rmc("102030", "071126", "A", "1e3", "01131.000"), true, false},
		{"garbage fractional time", rmc("102030oops", "071126", "A", "4807.038", "01131.000"), true, false},
		{"GGA contradicts fix", good + "\n" + nmeaLine("GPGGA,102030,4807.038,N,01131.000,E,0,00,99.9,,M,,M,,"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := LocationStatus{GNSSOn: tc.on, NMEA: tc.nmea, Fix: true, Position: &Position{Latitude: 999}}
			parseNMEA(&s, now)
			if s.Fix != tc.want || (s.Position != nil) != tc.want {
				t.Fatalf("%+v", s)
			}
			if tc.name == "valid" {
				if math.Abs(s.Position.Latitude-48.1173) > 1e-6 || s.Position.Altitude == nil || *s.Position.Altitude != 545.4 || s.SatellitesUsed == nil || *s.SatellitesUsed != 8 {
					t.Fatal(s)
				}
			}
		})
	}
	for _, tc := range []struct {
		v, h    string
		lat, ok bool
	}{{"9000", "N", true, true}, {"9000.1", "N", true, false}, {"18000", "W", false, true}, {"18000.1", "W", false, false}, {"1200", "E", true, false}, {"-100", "N", true, false}, {"Inf", "E", false, false}} {
		if _, ok := nmeaCoordinate(tc.v, tc.h, tc.lat); ok != tc.ok {
			t.Fatal(tc)
		}
	}
}
func TestNMEASatellitesDoNotCreateFix(t *testing.T) {
	s := LocationStatus{GNSSOn: true, NMEA: nmeaLine("GPGSV,2,1,08,01,40,083,42") + "\n" + nmeaLine("GPGSV,2,2,08,02,40,083,42") + "\n" + nmeaLine("GLGSV,1,1,03,02,40,083,42")}
	parseNMEA(&s, time.Now())
	if s.Fix || s.Position != nil || s.SatellitesVisible == nil || *s.SatellitesVisible != 11 {
		t.Fatal(s)
	}
	s.NMEA += "\n" + nmeaLine("GNGSV,1,1,10,01,40,083,42")
	parseNMEA(&s, time.Now())
	if *s.SatellitesVisible != 10 {
		t.Fatal("combined talker double counted")
	}
}
func TestGNSSSourcePreservation(t *testing.T) {
	for _, mask := range []uint32{0, 1, 8, 9, 7, 15} {
		s := LocationStatus{Available: true, Reviewed: true, EnabledSources: mask}
		next, e := gnssSources(s, true)
		if e != nil || next != mask|6 {
			t.Fatal(mask, next, e)
		}
		s.EnabledSources = next
		off, e := gnssSources(s, false)
		if e != nil || off != mask&^6 {
			t.Fatal(off, e)
		}
	}
	for _, s := range []LocationStatus{{}, {Available: true}, {Available: true, Reviewed: true, Signals: true}, {Available: true, Reviewed: true, EnabledSources: 17}, {Available: true, Reviewed: true, EnabledSources: 3}} {
		if _, e := gnssSources(s, true); e == nil {
			t.Fatal("unreviewed configuration accepted", s)
		}
	}
	// Stopping managed sources neither takes over unmanaged GPS nor changes broadcasts.
	s := LocationStatus{Available: true, Signals: true, EnabledSources: 31}
	n, e := gnssSources(s, false)
	if e != nil || n != 25 || !s.Signals {
		t.Fatal(n, e)
	}
}

type fakeLocationBackend struct {
	DemoBackend
	sets      atomic.Int32
	err       error
	after     func()
	sensitive bool
}

func (b *fakeLocationBackend) ReadLocation(ctx context.Context) (LocationStatus, error) {
	s, e := b.DemoBackend.ReadLocation(ctx)
	if b.sensitive {
		s.NMEA = "PRIVATE_NMEA_SENTINEL"
		s.Position = &Position{Latitude: 12.345678901, Longitude: 98.765432101}
		s.Fix = true
	}
	return s, e
}
func (b *fakeLocationBackend) SetGNSS(ctx context.Context, on bool) (LocationStatus, error) {
	b.sets.Add(1)
	if b.err != nil {
		return LocationStatus{}, b.err
	}
	s, e := b.DemoBackend.SetGNSS(ctx, on)
	if b.after != nil {
		b.after()
	}
	return s, e
}
func TestLocationHTTPGuardsAndPrivacy(t *testing.T) {
	b := &fakeLocationBackend{}
	sb := &fakeSMSBackend{}
	a, f := smsApp(t, sb)
	a.backend = b
	a.poll(context.Background())
	if r := request(a, "GET", "/api/location", "", nil, "", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
	c, csrf := login(t, a)
	if r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("feature flag", r.Code)
	}
	a.locationControl = true
	for _, tc := range []struct {
		body, token, origin string
		code                int
	}{{`{"enabled":true}`, "", "", 403}, {`{"enabled":true}`, csrf, "http://evil.test", 403}, {`{}`, csrf, "", 400}, {`{"enabled":"yes"}`, csrf, "", 400}} {
		if r := request(a, "POST", "/api/location/control", tc.body, c, tc.token, tc.origin); r.Code != tc.code {
			t.Fatal(tc, r.Code)
		}
	}
	a.locationBusy = true
	if r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("concurrent setup")
	}
	a.locationBusy = false
	a.smsActive = true
	if r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("SMS interlock")
	}
	a.smsActive = false
	fstate := demoFailover()
	fstate.Active = "cellular"
	writeFailover(t, f, fstate)
	if r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, ""); r.Code != 409 {
		t.Fatal("cellular active", r.Code)
	}
	if b.sets.Load() != 0 {
		t.Fatal("guard reached hardware")
	}
	// A stop remains available while cellular is carrying data.
	if r := request(a, "POST", "/api/location/control", `{"enabled":false}`, c, csrf, ""); r.Code != 200 {
		t.Fatal("stop blocked", r.Code, r.Body)
	}
	writeFailover(t, f, demoFailover())
	if r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, ""); r.Code != 200 {
		t.Fatal("enable", r.Code, r.Body)
	}
	b.sensitive = true
	r := request(a, "GET", "/api/location", "", c, "", "")
	var loc LocationStatus
	if json.Unmarshal(r.Body.Bytes(), &loc) != nil || !loc.Fix || !strings.Contains(r.Body.String(), "PRIVATE_NMEA_SENTINEL") {
		t.Fatal(r.Code, r.Body)
	}
	for _, path := range []string{"/api/status", "/api/diagnostics", "/api/diagnostics/export"} {
		r := request(a, "GET", path, "", c, "", "")
		if r.Code != 200 || strings.Contains(r.Body.String(), "PRIVATE_NMEA_SENTINEL") || strings.Contains(r.Body.String(), "12.345678901") {
			t.Fatal("location leak", path)
		}
	}
	b.sensitive = false
	b.err = errors.New("timeout")
	count := b.sets.Load()
	if r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, ""); r.Code != 502 {
		t.Fatal("error handling", r.Code)
	}
	if b.sets.Load() != count+1 {
		t.Fatal("unsafe retry")
	}
}
func TestLocationGuardRestoresNewEnableWhenNetworkChanges(t *testing.T) {
	b := &fakeLocationBackend{}
	a, f := smsApp(t, &fakeSMSBackend{})
	a.backend = b
	a.poll(context.Background())
	a.locationControl = true
	b.after = func() { v := demoFailover(); v.CellularHealthy = false; writeFailover(t, f, v) }
	c, csrf := login(t, a)
	r := request(a, "POST", "/api/location/control", `{"enabled":true}`, c, csrf, "")
	if r.Code != 503 || b.sets.Load() != 2 {
		t.Fatal(r.Code, b.sets.Load())
	}
	s, _ := b.ReadLocation(context.Background())
	if s.GNSSOn {
		t.Fatal("enable not restored")
	}
}
func TestLocationEnablePolicy(t *testing.T) {
	f := demoFailover()
	r := Radio{Present: true}
	l := LocationStatus{Available: true, Reviewed: true}
	now := time.Now()
	if reason := locationPolicy(r, f, nil, nil, now, l); reason != "" {
		t.Fatal(reason)
	}
	cases := []struct {
		f     Failover
		l     LocationStatus
		calls []Call
		last  time.Time
	}{{f, l, nil, now.Add(-time.Minute)}, {f, l, []Call{{StateCode: 3}}, now}, {f, LocationStatus{Available: true}, nil, now}, {f, LocationStatus{Available: true, Reviewed: true, Signals: true}, nil, now}, {f, LocationStatus{Available: true, Reviewed: true, EnabledSources: 17}, nil, now}}
	for _, tc := range cases {
		if locationPolicy(r, tc.f, nil, tc.calls, tc.last, tc.l) == "" {
			t.Fatal(tc)
		}
	}
}
