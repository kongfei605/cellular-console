package main

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Derived only from checksum-verified, cached NMEA; absent reports remain absent.
type NMEADiagnostics struct {
	HasRMC            bool     `json:"has_rmc"`
	HasGGA            bool     `json:"has_gga"`
	HasGSA            bool     `json:"has_gsa"`
	HasGSV            bool     `json:"has_gsv"`
	RMCStatus         string   `json:"rmc_status"`
	GGAQuality        *int     `json:"gga_quality,omitempty"`
	GSAFixTypes       []int    `json:"gsa_fix_types"`
	GSASatellitesUsed *int     `json:"gsa_satellites_used,omitempty"`
	SNRReports        int      `json:"snr_reports"`
	MaxSNR            *float64 `json:"max_snr_dbhz,omitempty"`
	SatelliteTime     string   `json:"satellite_time"`
	AntennaStatus     string   `json:"antenna_status"`
}

func diagnoseNMEA(raw string, now time.Time) NMEADiagnostics {
	d := NMEADiagnostics{AntennaStatus: "unknown", SatelliteTime: "missing", GSAFixTypes: []int{}}
	gsaReports := 0
	gsaUsed := 0
	fixTypes := map[int]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		f, ok := nmeaFields(line)
		if !ok {
			continue
		}
		switch f[0][2:] {
		case "RMC":
			if len(f) < 10 {
				continue
			}
			d.HasRMC = true
			d.RMCStatus = f[2]
			if f[1] != "" || f[9] != "" {
				stamp, ok := nmeaTime(f[9], f[1])
				if !ok {
					d.SatelliteTime = "invalid"
				} else if now.Sub(stamp) > 90*time.Second || stamp.Sub(now) > 5*time.Second {
					d.SatelliteTime = "stale"
				} else {
					d.SatelliteTime = "fresh"
				}
			}
		case "GGA":
			if len(f) < 9 {
				continue
			}
			d.HasGGA = true
			d.GGAQuality = nmeaInt(f[6], 8)
		case "GSA":
			if len(f) < 18 {
				continue
			}
			d.HasGSA = true
			gsaReports++
			if n := nmeaInt(f[2], 3); n != nil && *n >= 1 {
				fixTypes[*n] = true
			}
			for _, id := range f[3:15] {
				if n := nmeaInt(id, 999); n != nil && *n > 0 {
					gsaUsed++
				}
			}
		case "GSV":
			if len(f) < 4 {
				continue
			}
			d.HasGSV = true
			for i := 4; i+3 < len(f); i += 4 {
				if nmeaInt(f[i], 999) == nil {
					continue
				}
				if snr := finiteNumber(f[i+3], 0, 99); snr != nil {
					d.SNRReports++
					if d.MaxSNR == nil || *snr > *d.MaxSNR {
						d.MaxSNR = snr
					}
				}
			}
		}
	}
	for n := range fixTypes {
		d.GSAFixTypes = append(d.GSAFixTypes, n)
	}
	sort.Ints(d.GSAFixTypes)
	// Multiple GSA reports may overlap across constellations. Do not invent a total.
	if gsaReports == 1 {
		d.GSASatellitesUsed = &gsaUsed
	}
	return d
}

type LocationCacheStatus struct {
	State              string     `json:"state"`
	Reason             string     `json:"reason,omitempty"`
	Samples            int        `json:"samples"`
	ContentChanges     int        `json:"content_changes"`
	ObservedSince      *time.Time `json:"observed_since,omitempty"`
	LastObservedChange *time.Time `json:"last_observed_change,omitempty"`
	UnchangedSeconds   int64      `json:"unchanged_seconds"`
}

// Only digests, device/config identity and observation times live in memory.
// Nothing is archived or included in general diagnostics exports.
type locationObserver struct {
	mu                          sync.Mutex
	have                        bool
	identity                    string
	digest                      [32]byte
	first, last, unchangedSince time.Time
	lastChange                  *time.Time
	samples, changes            int
}

func (o *locationObserver) clear() {
	o.have = false
	o.identity = ""
	o.digest = [32]byte{}
	o.first = time.Time{}
	o.last = time.Time{}
	o.unchangedSince = time.Time{}
	o.lastChange = nil
	o.samples = 0
	o.changes = 0
}
func (o *locationObserver) reset() { o.mu.Lock(); defer o.mu.Unlock(); o.clear() }
func canonicalNMEA(raw string) string {
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
func (o *locationObserver) observe(s LocationStatus, now time.Time) LocationCacheStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := ""
	if !s.Available {
		state = "unavailable"
	} else if !s.GNSSOn {
		state = "off"
	} else if s.Sentences == 0 {
		state = "empty"
	}
	if state != "" {
		o.clear()
		return LocationCacheStatus{State: state}
	}
	// Concurrent requests may finish out of order; do not count an older read as new evidence.
	if o.have && now.Before(o.last) {
		return LocationCacheStatus{State: "unknown", Reason: "out_of_order"}
	}
	identity := s.cacheIdentity + ":" + strconv.FormatUint(uint64(s.EnabledSources), 10) + ":" + strconv.FormatBool(s.Signals)
	digest := sha256.Sum256([]byte(canonicalNMEA(s.NMEA)))
	reason := ""
	if !o.have {
		reason = "first_read"
	} else if identity != o.identity {
		reason = "configuration_changed"
	} else if now.Sub(o.last) > 2*time.Minute {
		reason = "observation_resumed"
	}
	resultState := "unchanged"
	if reason != "" {
		o.clear()
		o.have = true
		o.identity = identity
		o.digest = digest
		o.first = now
		o.unchangedSince = now
		resultState = "first"
	} else if digest != o.digest {
		o.digest = digest
		o.changes++
		o.unchangedSince = now
		t := now
		o.lastChange = &t
		resultState = "changed"
	}
	o.last = now
	o.samples++
	first := o.first
	var lastChange *time.Time
	if o.lastChange != nil {
		t := *o.lastChange
		lastChange = &t
	}
	return LocationCacheStatus{State: resultState, Reason: reason, Samples: o.samples, ContentChanges: o.changes, ObservedSince: &first, LastObservedChange: lastChange, UnchangedSeconds: int64(now.Sub(o.unchangedSince).Seconds())}
}
