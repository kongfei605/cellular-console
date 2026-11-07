package voicepcm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Stream is full-duplex: Read is modem -> browser, Write is browser -> modem.
// Close MUST unblock pending reads/writes. No implementation may record audio.
type Stream interface {
	io.Reader
	io.Writer
	io.Closer
}
type Factory func(context.Context) (Stream, error)
type Guard func(context.Context) error

// ProtectFactory closes PCM I/O on guard failure or cancellation. The guard
// supplies the policy; it must never repair networks or automatically dial.
func ProtectFactory(open Factory, guard Guard) Factory {
	return func(parent context.Context) (Stream, error) {
		if err := parent.Err(); err != nil {
			return nil, err
		}
		if open == nil || guard == nil {
			return nil, errors.New("PCM opener and live guard required")
		}
		if err := guard(parent); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(parent)
		stream, err := open(ctx)
		if err != nil {
			cancel()
			return nil, err
		}
		if stream == nil {
			cancel()
			return nil, errors.New("PCM opener returned no stream")
		}
		s := &protectedStream{Stream: stream, cancel: cancel}
		if err = guard(ctx); err != nil {
			s.Close()
			return nil, err
		}
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					s.Close()
					return
				case <-ticker.C:
					if guard(ctx) != nil {
						s.Close()
						return
					}
				}
			}
		}()
		return s, nil
	}
}

type protectedStream struct {
	Stream
	cancel context.CancelFunc
	once   sync.Once
}

func (s *protectedStream) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.Stream.Close() })
	return err
}

// ALSAFactory is intentionally not wired to a CLI mode or production unit yet.
// Its guard is mandatory and must bind a fresh selected modem + live call and
// network state; granting all host audio devices is not part of this feature.
func ALSAFactory(usbPath string, guard Guard) Factory {
	return func(ctx context.Context) (Stream, error) {
		if guard == nil {
			return nil, errors.New("live call/network guard required")
		}
		if err := guard(ctx); err != nil {
			return nil, err
		}
		device, err := Discover("/sys", "/proc", usbPath)
		if err != nil {
			return nil, err
		}
		return ProtectFactory(func(c context.Context) (Stream, error) {
			return openALSA(c, device, guard, func() (Device, error) { return Discover("/sys", "/proc", usbPath) }, exec.CommandContext)
		}, guard)(ctx)
	}
}

func alsaArgs(device Device) []string {
	return []string{"-q", "-N", "-D", fmt.Sprintf("hw:CARD=%s,DEV=0", device.CardID), "-t", "raw", "-f", "S16_LE", "-r", "8000", "-c", "1", "--period-size=160", "--buffer-size=800"}
}

type alsaStream struct {
	capture  io.ReadCloser
	playback io.WriteCloser
	cancel   context.CancelFunc
	once     sync.Once
	done     chan struct{}
}

func openALSA(parent context.Context, device Device, guard Guard, discover func() (Device, error), command func(context.Context, string, ...string) *exec.Cmd) (Stream, error) {
	ctx, cancel := context.WithCancel(parent)
	reader := command(ctx, "/usr/bin/arecord", alsaArgs(device)...)
	writer := command(ctx, "/usr/bin/aplay", alsaArgs(device)...)
	reader.WaitDelay, writer.WaitDelay = 500*time.Millisecond, 500*time.Millisecond
	// No audio files and no unbounded stderr buffers. Failures close the peer.
	reader.Stderr, writer.Stderr = io.Discard, io.Discard
	input, err := reader.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := writer.StdinPipe()
	if err != nil {
		input.Close()
		cancel()
		return nil, err
	}
	s := &alsaStream{capture: input, playback: output, cancel: cancel, done: make(chan struct{})}
	if err = reader.Start(); err != nil {
		input.Close()
		output.Close()
		cancel()
		return nil, err
	}
	if err = writer.Start(); err != nil {
		input.Close()
		output.Close()
		cancel()
		reader.Wait()
		return nil, err
	}
	var workers sync.WaitGroup
	workers.Add(2)
	for _, cmd := range []*exec.Cmd{reader, writer} {
		go func(c *exec.Cmd) { defer workers.Done(); c.Wait(); cancel(); input.Close(); output.Close() }(cmd)
	}
	go func() { workers.Wait(); close(s.done) }()
	check := func() error {
		if err := guard(ctx); err != nil {
			return err
		}
		now, err := discover()
		if err != nil {
			return err
		}
		if now.Fingerprint != device.Fingerprint {
			return errors.New("EC20 ALSA device generation changed")
		}
		return nil
	}
	if err = check(); err != nil {
		s.Close()
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				s.Close()
				return
			case <-ticker.C:
				if check() != nil {
					s.Close()
					return
				}
			}
		}
	}()
	return s, nil
}

func (s *alsaStream) Read(b []byte) (int, error)  { return s.capture.Read(b) }
func (s *alsaStream) Write(b []byte) (int, error) { return s.playback.Write(b) }
func (s *alsaStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.capture.Close()
		s.playback.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	})
	return nil
}
