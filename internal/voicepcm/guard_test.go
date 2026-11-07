package voicepcm

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallAndNetworkGuardRejectsUnsafeChanges(t *testing.T) {
	now := time.Now()
	good := State{Observed: now, FailoverUpdated: now, ModemID: "selected", Present: true, CallID: "42", CallActive: true, Primary: "wired", WiredHealthy: true, CellularHealthy: true, VPNsHealthy: true}
	if err := CheckState(good, "selected", "42", now); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*State){
		func(s *State) { s.Observed = now.Add(-4 * time.Second) }, func(s *State) { s.Observed = now.Add(time.Second) }, func(s *State) { s.FailoverUpdated = now.Add(-16 * time.Second) },
		func(s *State) { s.ModemID = "other" }, func(s *State) { s.Present = false }, func(s *State) { s.CallID = "next-call" }, func(s *State) { s.CallActive = false },
		func(s *State) { s.Primary = "cellular" }, func(s *State) { s.WiredHealthy = false }, func(s *State) { s.CellularHealthy = false }, func(s *State) { s.VPNsHealthy = false },
	} {
		s := good
		mutate(&s)
		if CheckState(s, "selected", "42", now) == nil {
			t.Fatal("unsafe call/network state accepted")
		}
	}
	if CheckState(good, "", "42", now) == nil {
		t.Fatal("unpinned modem allowed")
	}
}

type blockingPCM struct {
	done chan struct{}
	once sync.Once
}

func (b *blockingPCM) Read(p []byte) (int, error) { <-b.done; return 0, io.EOF }
func (b *blockingPCM) Write(p []byte) (int, error) {
	select {
	case <-b.done:
		return 0, io.ErrClosedPipe
	default:
		return len(p), nil
	}
}
func (b *blockingPCM) Close() error { b.once.Do(func() { close(b.done) }); return nil }

func TestProtectedStreamClosesBlockedIOOnGuardFailure(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	device := &blockingPCM{done: make(chan struct{})}
	factory := ProtectFactory(func(context.Context) (Stream, error) { return device, nil }, func(context.Context) error {
		if !allowed.Load() {
			return errors.New("call ended")
		}
		return nil
	})
	stream, err := factory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	unblocked := make(chan struct{})
	go func() { stream.Read(make([]byte, 2)); close(unblocked) }()
	allowed.Store(false)
	select {
	case <-unblocked:
	case <-time.After(time.Second):
		t.Fatal("guard did not release blocked capture")
	}
}
func TestGuardFailureDoesNotOpenAnyPCM(t *testing.T) {
	opened := false
	open := func(context.Context) (Stream, error) { opened = true; return nil, nil }
	_, err := ProtectFactory(open, func(context.Context) error { return errors.New("wired unavailable") })(context.Background())
	if err == nil || opened {
		t.Fatal("PCM opened despite failed guard")
	}
	if _, err = ALSAFactory("1-2", nil)(context.Background()); err == nil {
		t.Fatal("ALSA without live guard accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ProtectFactory(open, func(context.Context) error { return nil })(ctx)
	if err == nil || opened {
		t.Fatal("cancelled session opened PCM")
	}
}
