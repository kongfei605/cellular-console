package atmodem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

type Device struct {
	Location string `json:"location"`
	Session  string `json:"session"`
	Network  string `json:"network"`
}

var LocationPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

func Devices(ctx context.Context, helper string) ([]Device, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, helper, "list").Output()
	if err != nil {
		return nil, fmt.Errorf("macOS USB helper unavailable; run make build-mac: %w", err)
	}
	var devices []Device
	if len(data) > 16384 || json.Unmarshal(data, &devices) != nil {
		return nil, errors.New("invalid USB discovery response")
	}
	for _, d := range devices {
		if !LocationPattern.MatchString(d.Location) || d.Session == "" {
			return nil, errors.New("invalid USB identity")
		}
	}
	return devices, nil
}

type processStream struct {
	in   io.WriteCloser
	out  io.ReadCloser
	cmd  *exec.Cmd
	once sync.Once
}

func (s *processStream) Read(b []byte) (int, error)  { return s.out.Read(b) }
func (s *processStream) Write(b []byte) (int, error) { return s.in.Write(b) }
func (s *processStream) Close() error {
	s.once.Do(func() { s.in.Close(); s.out.Close(); _ = s.cmd.Process.Kill(); _ = s.cmd.Wait() })
	return nil
}
func Open(helper, location string) (*Client, error) {
	if !LocationPattern.MatchString(location) {
		return nil, errors.New("invalid USB location")
	}
	stream, err := openProcess(helper, "at", location)
	if err != nil {
		return nil, err
	}
	c := New(stream)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Synchronize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func OpenAudio(helper, location string) (io.ReadWriteCloser, error) {
	if !LocationPattern.MatchString(location) {
		return nil, errors.New("invalid USB location")
	}
	return openProcess(helper, "audio", location)
}
func openProcess(helper, mode, location string) (io.ReadWriteCloser, error) {
	cmd := exec.Command(helper, mode, location)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	// Helper diagnostics contain no AT payloads. A closed stream is surfaced as
	// an unavailable control interface, never as an unplugged USB device.
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	return &processStream{in: in, out: out, cmd: cmd}, nil
}

type AudioInfo struct{ Input, Output, Session string }

func ReadAudioInfo(ctx context.Context, helper, location string) (AudioInfo, error) {
	var info AudioInfo
	if !LocationPattern.MatchString(location) {
		return info, errors.New("invalid USB location")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, helper, "audio-info", location).Output()
	if err != nil {
		return info, errors.New("EC20 CoreAudio 8000 Hz 单声道端点未就绪")
	}
	if len(data) > 8192 || json.Unmarshal(data, &info) != nil || info.Input == "" || info.Output == "" || info.Session == "" {
		return info, errors.New("invalid CoreAudio device identity")
	}
	return info, nil
}
