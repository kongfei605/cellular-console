package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNMEADiagnosticsMissingReportsRemainUnknown(t *testing.T) {
	raw := strings.Join([]string{nmeaLine("GPRMC,,V,,,,,,,,,,N"), nmeaLine("GPGGA,,,,,,0,,,,,,,,"), nmeaLine("GPGSA,A,1,,,,,,,,,,,,,,,")}, "\r\n")
	s := LocationStatus{Available: true, GNSSOn: true, NMEA: raw}
	parseNMEA(&s, time.Now())
	d := s.Diagnostics
	if s.Fix || s.Position != nil || !d.HasRMC || !d.HasGGA || !d.HasGSA || d.HasGSV || d.RMCStatus != "V" || d.GGAQuality == nil || *d.GGAQuality != 0 || len(d.GSAFixTypes) != 1 || d.GSAFixTypes[0] != 1 {
		t.Fatalf("unexpected no-fix diagnostics: %+v", d)
	}
	if s.SatellitesVisible != nil || s.HDOP != nil || d.MaxSNR != nil || d.SNRReports != 0 || s.SatellitesUsed == nil || *s.SatellitesUsed != 0 {
		t.Fatalf("absent report treated as zero: %+v", s)
	}
	if d.AntennaStatus != "unknown" || d.SatelliteTime != "missing" {
		t.Fatal(d)
	}
	// A visible count of zero must come from an actual GSV report.
	s.NMEA += "\n" + nmeaLine("GPGSV,1,1,00")
	parseNMEA(&s, time.Now())
	if !s.Diagnostics.HasGSV || s.SatellitesVisible == nil || *s.SatellitesVisible != 0 {
		t.Fatal(s)
	}
}
func TestNMEADiagnosticsSignalTimeAndInvalidSentences(t *testing.T) {
	now := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	raw := nmeaLine("GPRMC,080000,A,4807.038,N,01131.000,E,0,0,071126,,,A") + "\n" + nmeaLine("GPGSV,1,1,04,01,40,083,42,02,30,090,,03,20,100,00,04,10,200,99")
	d := diagnoseNMEA(raw, now)
	if d.SatelliteTime != "fresh" || d.SNRReports != 3 || d.MaxSNR == nil || *d.MaxSNR != 99 || d.AntennaStatus != "unknown" {
		t.Fatal(d)
	}
	if diagnoseNMEA(raw, now.Add(91*time.Second)).SatelliteTime != "stale" {
		t.Fatal("stale clock")
	}
	if diagnoseNMEA(nmeaLine("GPRMC,080000,V,,,,,,,321126,,,N"), now).SatelliteTime != "invalid" {
		t.Fatal("invalid date")
	}
	d = diagnoseNMEA("$GPGSV,1,1,00*XX\n"+nmeaLine("GPGSV"), now)
	if d.HasGSV || d.SNRReports != 0 {
		t.Fatal("bad checksum/short report accepted")
	}
	// GSA constellations may overlap. Do not add multiple reports into a fabricated total.
	s := LocationStatus{GNSSOn: true, NMEA: nmeaLine("GPGSA,A,1,,,,,,,,,,,,,,,") + "\n" + nmeaLine("GNGSA,A,1,,,,,,,,,,,,,,,")}
	parseNMEA(&s, now)
	if s.Diagnostics.GSASatellitesUsed != nil || s.SatellitesUsed != nil {
		t.Fatal("overlapping GSA total invented")
	}
}
func TestLocationCacheObservationLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	s := LocationStatus{Available: true, GNSSOn: true, Sentences: 2, EnabledSources: 7, cacheIdentity: "ownerA-device", NMEA: "$A\r\n$B"}
	var o locationObserver
	first := o.observe(s, now)
	if first.State != "first" || first.Samples != 1 || first.LastObservedChange != nil {
		t.Fatal(first)
	}
	s.NMEA = " $B \n$A\n"
	same := o.observe(s, now.Add(30*time.Second))
	if same.State != "unchanged" || same.UnchangedSeconds != 30 || same.Samples != 2 || same.ContentChanges != 0 {
		t.Fatal(same)
	}
	s.NMEA = "$A\n$C"
	changed := o.observe(s, now.Add(40*time.Second))
	if changed.State != "changed" || changed.ContentChanges != 1 || changed.LastObservedChange == nil || changed.UnchangedSeconds != 0 {
		t.Fatal(changed)
	}
	same = o.observe(s, now.Add(50*time.Second))
	if same.State != "unchanged" || same.UnchangedSeconds != 10 || same.LastObservedChange == nil {
		t.Fatal(same)
	}
	if old := o.observe(s, now.Add(45*time.Second)); old.State != "unknown" {
		t.Fatal("older response counted", old)
	}
	resumed := o.observe(s, now.Add(3*time.Minute))
	if resumed.State != "first" || resumed.Reason != "observation_resumed" || resumed.Samples != 1 || resumed.ContentChanges != 0 {
		t.Fatal(resumed)
	}
	s.cacheIdentity = "ownerB-device"
	newDevice := o.observe(s, now.Add(3*time.Minute+time.Second))
	if newDevice.State != "first" || newDevice.Reason != "configuration_changed" {
		t.Fatal(newDevice)
	}
	s.GNSSOn = false
	if off := o.observe(s, now.Add(4*time.Minute)); off.State != "off" || off.ObservedSince != nil {
		t.Fatal(off)
	}
	s.GNSSOn = true
	if on := o.observe(s, now.Add(4*time.Minute+time.Second)); on.State != "first" {
		t.Fatal(on)
	}
	o.reset()
	if afterError := o.observe(s, now.Add(4*time.Minute+2*time.Second)); afterError.State != "first" {
		t.Fatal(afterError)
	}
	s.Sentences = 0
	if empty := o.observe(s, now.Add(5*time.Minute)); empty.State != "empty" || empty.Samples != 0 {
		t.Fatal(empty)
	}
	s.Available = false
	if unavailable := o.observe(s, now.Add(6*time.Minute)); unavailable.State != "unavailable" {
		t.Fatal(unavailable)
	}
}
func TestLocationObserverConcurrentReadsAndPrivacy(t *testing.T) {
	s := LocationStatus{Available: true, GNSSOn: true, Sentences: 1, EnabledSources: 7, cacheIdentity: "PRIVATE_DEVICE_ID", NMEA: "PRIVATE_LOCATION_NMEA"}
	var o locationObserver
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); o.observe(s, time.Now()) }()
	}
	wg.Wait()
	result := o.observe(s, time.Now())
	b, e := json.Marshal(result)
	if e != nil || strings.Contains(string(b), "PRIVATE_") {
		t.Fatal("cache observation leaks device/raw data", string(b))
	}
	b, e = json.Marshal(s)
	if e != nil || strings.Contains(string(b), "PRIVATE_DEVICE_ID") {
		t.Fatal("private cache identity serialized")
	}
}
