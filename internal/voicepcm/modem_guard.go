package voicepcm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const mmName = "org.freedesktop.ModemManager1"
const modemIF = mmName + ".Modem"
const voiceIF = modemIF + ".Voice"
const callIF = mmName + ".Call"

var callPathPattern = regexp.MustCompile(`^/org/freedesktop/ModemManager1/Call/[0-9]{1,12}$`)

type managedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

// SystemGuard reads cached D-Bus properties and the independent failover state.
// It never calls a modem control method. A unique bus owner is pinned so a
// daemon restart cannot silently attach this audio session to a different call.
type SystemGuard struct {
	mu                           sync.Mutex
	conn                         *dbus.Conn
	owner, modemID, failoverFile string
	call                         dbus.ObjectPath
	vpnInterfaces                []string
}

func NewSystemGuard(ctx context.Context, modemID, callPath, failoverFile string, vpnInterfaces ...string) (*SystemGuard, error) {
	if len(vpnInterfaces) == 0 {
		return nil, errors.New("explicit required VPN set required")
	}
	if modemID == "" || !callPathPattern.MatchString(callPath) || failoverFile == "" {
		return nil, errors.New("pinned modem, full call path and failover file required")
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	var owner string
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, mmName).Store(&owner)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &SystemGuard{conn: conn, owner: owner, modemID: modemID, call: dbus.ObjectPath(callPath), failoverFile: failoverFile, vpnInterfaces: vpnInterfaces}, nil
}

func (g *SystemGuard) Close() error  { g.mu.Lock(); defer g.mu.Unlock(); return g.conn.Close() }
func (g *SystemGuard) Owner() string { return g.owner }
func (g *SystemGuard) Check(parent context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
	defer cancel()
	var objects managedObjects
	err := g.conn.Object(g.owner, "/org/freedesktop/ModemManager1").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", dbus.FlagNoAutoStart).Store(&objects)
	if err != nil {
		return err
	}
	if err = hydrateCalls(objects, g.modemID, func(path dbus.ObjectPath) (map[string]dbus.Variant, error) {
		var p map[string]dbus.Variant
		e := g.conn.Object(g.owner, path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, callIF).Store(&p)
		return p, e
	}); err != nil {
		return err
	}
	var call map[string]dbus.Variant
	err = g.conn.Object(g.owner, g.call).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, callIF).Store(&call)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(g.failoverFile)
	if err != nil {
		return err
	}
	state, err := readState(objects, call, b, g.modemID, g.call, time.Now().UTC(), g.vpnInterfaces...)
	if err != nil {
		return err
	}
	return CheckState(state, g.modemID, string(g.call), time.Now().UTC())
}

func hydrateCalls(objects managedObjects, modemID string, readCall func(dbus.ObjectPath) (map[string]dbus.Variant, error)) error {
	for _, ifaces := range objects {
		id, _ := ifaces[modemIF]["DeviceIdentifier"].Value().(string)
		if id != modemID {
			continue
		}
		paths, ok := ifaces[voiceIF]["Calls"].Value().([]dbus.ObjectPath)
		if !ok {
			return errors.New("call list unavailable")
		}
		for _, path := range paths {
			if !callPathPattern.MatchString(string(path)) {
				return errors.New("invalid call path")
			}
			p, e := readCall(path)
			if e != nil {
				return e
			}
			if _, ok := p["State"].Value().(int32); !ok {
				return errors.New("call state unavailable")
			}
			if objects[path] == nil {
				objects[path] = map[string]map[string]dbus.Variant{}
			}
			objects[path][callIF] = p
		}
	}
	return nil
}

func readState(objects managedObjects, call map[string]dbus.Variant, failover []byte, modemID string, callPath dbus.ObjectPath, observed time.Time, vpnInterfaces ...string) (State, error) {
	var out State
	var f struct {
		Updated  string          `json:"updated"`
		Primary  string          `json:"active"`
		Wired    bool            `json:"wired_healthy"`
		Cellular bool            `json:"cellular_healthy"`
		Present  bool            `json:"cellular_present"`
		VPNs     map[string]bool `json:"vpns"`
	}
	if err := json.Unmarshal(failover, &f); err != nil {
		return out, err
	}
	t, err := time.Parse("2006-01-02T15:04:05-0700", f.Updated)
	if err != nil {
		t, err = time.Parse(time.RFC3339, f.Updated)
	}
	if err != nil {
		return out, err
	}
	matches := 0
	belongs := false
	for _, ifaces := range objects {
		id, _ := ifaces[modemIF]["DeviceIdentifier"].Value().(string)
		if id != modemID {
			continue
		}
		matches++
		calls, _ := ifaces[voiceIF]["Calls"].Value().([]dbus.ObjectPath)
		for _, p := range calls {
			if p == callPath {
				belongs = true
			} else {
				other, _ := objects[p][callIF]["State"].Value().(int32)
				if other != 7 {
					return out, errors.New("another call exists")
				}
			}
		}
	}
	if matches != 1 || !belongs {
		return out, errors.New("call does not belong to the pinned EC20")
	}
	code, _ := call["State"].Value().(int32)
	return State{Observed: observed, FailoverUpdated: t, ModemID: modemID, Present: f.Present, CallID: string(callPath), CallActive: code == 4,
		Primary: f.Primary, WiredHealthy: f.Wired, CellularHealthy: f.Cellular, VPNsHealthy: vpnsHealthy(f.VPNs, vpnInterfaces)}, nil
}

func vpnsHealthy(actual map[string]bool, expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	if len(actual) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, name := range expected {
		if seen[name] || !actual[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
