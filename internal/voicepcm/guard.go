package voicepcm

import (
	"errors"
	"time"
)

// State deliberately contains no telephone number or message data. A future
// read-only modem adapter must supply a new observation on each guard check.
type State struct {
	Observed        time.Time
	FailoverUpdated time.Time
	ModemID         string
	Present         bool
	CallID          string
	CallActive      bool
	Primary         string
	WiredHealthy    bool
	CellularHealthy bool
	VPNsHealthy     bool
}

func CheckState(s State, expectedModem, expectedCall string, now time.Time) error {
	if expectedModem == "" || expectedCall == "" {
		return errors.New("explicit modem and active call identity required")
	}
	if s.Observed.IsZero() || now.Sub(s.Observed) < 0 || now.Sub(s.Observed) > 3*time.Second {
		return errors.New("voice observation is stale")
	}
	if s.FailoverUpdated.IsZero() || now.Sub(s.FailoverUpdated) < 0 || now.Sub(s.FailoverUpdated) > 15*time.Second {
		return errors.New("failover observation is stale")
	}
	if !s.Present || s.ModemID != expectedModem {
		return errors.New("selected EC20 is unavailable")
	}
	if !s.CallActive || s.CallID != expectedCall {
		return errors.New("the selected call is no longer active")
	}
	if s.Primary != "wired" || !s.WiredHealthy || !s.CellularHealthy || !s.VPNsHealthy {
		return errors.New("network protection requires healthy wired primary and cellular standby")
	}
	return nil
}
