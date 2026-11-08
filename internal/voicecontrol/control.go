// Package voicecontrol verifies a root-prepared UAC generation without opening AT ports.
package voicecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/godbus/dbus/v5"
	"github.com/kongfei605/ec20-toolbox/internal/gnsscontrol"
	"github.com/kongfei605/ec20-toolbox/internal/voicepcm"
	"os"
	"regexp"
	"strings"
	"syscall"
)

var CallPath = regexp.MustCompile(`^/org/freedesktop/ModemManager1/Call/[0-9]{1,12}$`)

type Request struct {
	Phase string
	Call  string
}
type Reply struct{ Owner string }
type Snapshot struct {
	Owner, DeviceID, Model, Firmware, Primary string
	Calls                                     map[string]int32
}

func Validate(s Snapshot, r Request, device string) error {
	if s.Owner == "" || s.DeviceID != device || device == "" || s.Model != "EC20F" || s.Firmware != "EC20CEFAGR08A03M4G" {
		return errors.New("unreviewed voice profile")
	}
	if r.Phase == "dial" {
		if r.Call != "" {
			return errors.New("dial must not select an existing call")
		}
	} else if (r.Phase != "accept" && r.Phase != "active") || !CallPath.MatchString(r.Call) {
		return errors.New("invalid voice operation")
	}
	found := false
	for path, state := range s.Calls {
		if state == 7 {
			continue
		}
		if r.Phase == "dial" || path != r.Call {
			return errors.New("another call exists")
		}
		if (r.Phase == "accept" && state != 3) || (r.Phase == "active" && state != 4) {
			return errors.New("call is no longer in the required state")
		}
		found = true
	}
	if r.Phase != "dial" && !found {
		return errors.New("selected call absent")
	}
	return nil
}

type lease struct {
	Fingerprint string `json:"fingerprint"`
	Boot        string `json:"boot"`
	Verified    bool   `json:"verified"`
}

func validateLease(raw []byte, fingerprint, boot string) error {
	var l lease
	if len(raw) > 4096 || json.Unmarshal(raw, &l) != nil || !l.Verified || l.Fingerprint != fingerprint || l.Boot != strings.TrimSpace(boot) {
		return errors.New("本次USB代际尚未确认语音路由，请等待受控初始化")
	}
	return nil
}
func Ready() error {
	d, e := voicepcm.Discover("/sys", "/proc", "1-2")
	if e != nil {
		return e
	}
	const path = "/run/ec20-voice-route/ready.json"
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != 0 || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 {
		return errors.New("USB语音初始化凭证不可信")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return e
	}
	return validateLease(raw, d.Fingerprint, string(boot))
}

type Client struct{}

func (Client) Prepare(ctx context.Context, phase, call string) (Reply, error) {
	var out Reply
	r := Request{Phase: phase, Call: call}
	if phase != "dial" && phase != "accept" && phase != "active" {
		return out, errors.New("invalid voice phase")
	}
	if e := Ready(); e != nil {
		return out, e
	}
	conn, e := dbus.ConnectSystemBus()
	if e != nil {
		return out, e
	}
	defer conn.Close()
	m := Modem{Conn: conn, DeviceID: "0000000000000000000000000000000000000000"}
	g := gnsscontrol.NetworkGuard{Failover: "/run/ec20-failover/status.json"}
	if e = g.Check(); e != nil {
		return out, e
	}
	signature, e := g.Signature()
	if e != nil {
		return out, e
	}
	before, e := m.Read(ctx)
	if e != nil {
		return out, e
	}
	if e = Validate(before, r, m.DeviceID); e != nil {
		return out, e
	}
	after, e := m.Read(ctx)
	if e != nil {
		return out, e
	}
	observed, e := g.Signature()
	if e != nil {
		return out, e
	}
	if after.Owner != before.Owner || observed != signature {
		return out, errors.New("通话服务或网络代际已变化")
	}
	if e = Validate(after, r, m.DeviceID); e != nil {
		return out, e
	}
	if e = g.Check(); e != nil {
		return out, e
	}
	if e = Ready(); e != nil {
		return out, e
	}
	return Reply{Owner: after.Owner}, nil
}
