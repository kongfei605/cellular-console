// Package gnsscontrol provides the host-local, narrowly scoped GNSS privilege boundary.
package gnsscontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

type State struct {
	Owner, Path, DeviceID, Model, Firmware, Version string
	Sources, Capabilities                           uint32
	Signals, CallActive                             bool
}
type Driver interface {
	Read(context.Context) (State, error)
	Setup(context.Context, State, uint32) error
}
type Guard interface {
	Check() error
	Signature() (string, error)
}
type Server struct {
	Driver   Driver
	Guard    Guard
	DeviceID string
	mu       sync.Mutex
}

func next(s State, device string, on bool) (uint32, error) {
	if device == "" || s.DeviceID != device || s.Model != "EC20F" || s.Firmware != "EC20CEFAGR08A03M4G" || s.Version != "1.20.4" || s.Capabilities&6 != 6 || s.Signals || (s.Sources != 1 && s.Sources != 7) {
		return s.Sources, errors.New("unreviewed location configuration")
	}
	if on {
		if s.CallActive {
			return s.Sources, errors.New("call active")
		}
		return s.Sources | 6, nil
	}
	return s.Sources &^ 6, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	fail := func(code int) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"GNSS broker refused or could not confirm operation"}`))
	}
	if r.Method != "POST" || r.URL.Path != "/gnss" || r.URL.RawQuery != "" {
		fail(404)
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128))
	d.DisallowUnknownFields()
	if e := d.Decode(&input); e != nil || input.Enabled == nil {
		fail(400)
		return
	}
	var trailing any
	if e := d.Decode(&trailing); e != io.EOF {
		fail(400)
		return
	}
	if !s.mu.TryLock() {
		fail(409)
		return
	}
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	before, e := s.Driver.Read(ctx)
	if e != nil {
		fail(503)
		return
	}
	mask, e := next(before, s.DeviceID, *input.Enabled)
	if e != nil {
		fail(409)
		return
	}
	if *input.Enabled {
		if e = s.Guard.Check(); e != nil {
			fail(409)
			return
		}
	}
	signature, e := s.Guard.Signature()
	if e != nil {
		fail(503)
		return
	}
	if mask != before.Sources {
		if e = s.Driver.Setup(ctx, before, mask); e != nil {
			fail(502)
			return
		}
	}
	after, e := s.Driver.Read(ctx)
	if e != nil || after.Owner != before.Owner || after.Path != before.Path || after.DeviceID != before.DeviceID || after.Sources != mask || after.Signals != before.Signals {
		fail(502)
		return
	}
	observed, e := s.Guard.Signature()
	bad := e != nil || observed != signature
	if *input.Enabled && s.Guard.Check() != nil {
		bad = true
	}
	if bad {
		// Restore only the sources changed by this request, and only on the same MM instance.
		if mask != before.Sources {
			_ = s.Driver.Setup(ctx, after, before.Sources)
		}
		fail(503)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]uint32{"enabled_sources": after.Sources})
}
