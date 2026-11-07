package voicepcm

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestALSAPipeProcessesStopWithoutHardware(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var commands []*exec.Cmd
	var mu sync.Mutex
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "hw:CARD=EC20Test,DEV=0") || strings.Contains(joined, "default") {
			t.Fatal("unexpected ALSA target")
		}
		cmd := exec.CommandContext(ctx, path, "-test.run=^TestALSAFakeProcess$")
		cmd.Env = append(os.Environ(), "EC20_FAKE_ALSA="+name)
		mu.Lock()
		commands = append(commands, cmd)
		mu.Unlock()
		return cmd
	}
	d := Device{CardID: "EC20Test", Fingerprint: "generation-1"}
	var replaced atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := openALSA(ctx, d, func(context.Context) error { return nil }, func() (Device, error) {
		if replaced.Load() {
			return Device{Fingerprint: "generation-2"}, nil
		}
		return d, nil
	}, command)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, FrameBytes)
	if _, err = io.ReadFull(stream, frame); err != nil {
		t.Fatal(err)
	}
	if frame[0] != 0xe8 || frame[1] != 3 {
		t.Fatal("capture pipe corrupted")
	}
	if _, err = stream.Write(frame); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	replaced.Store(true)
	select {
	case <-stream.(*alsaStream).done:
	case <-time.After(2 * time.Second):
		t.Fatal("device replacement did not stop ALSA workers")
	}
	stream.Close()
	if time.Since(start) > 3*time.Second {
		t.Fatal("ALSA cleanup blocked")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, cmd := range commands {
		if cmd.ProcessState == nil {
			t.Fatal("ALSA child was not reaped")
		}
	}
}

// Invoked only as this test binary's child, never as an ALSA device process.
func TestALSAFakeProcess(t *testing.T) {
	mode := os.Getenv("EC20_FAKE_ALSA")
	if mode == "" {
		return
	}
	if strings.HasSuffix(mode, "arecord") {
		frame := make([]byte, FrameBytes)
		for i := 0; i < len(frame); i += 2 {
			frame[i] = 0xe8
			frame[i+1] = 3
		}
		for {
			if _, err := os.Stdout.Write(frame); err != nil {
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
	} else {
		io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(0)
}
