package audioloop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kongfei605/cellular-console/internal/voicepcm"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type syntheticModemPCM struct {
	capture  chan []byte
	playback chan []byte
	done     chan struct{}
	pending  []byte
	once     sync.Once
}

func (s *syntheticModemPCM) Read(b []byte) (int, error) {
	if len(s.pending) == 0 {
		select {
		case <-s.done:
			return 0, io.EOF
		case s.pending = <-s.capture:
		}
	}
	n := copy(b, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
func (s *syntheticModemPCM) Write(b []byte) (int, error) {
	select {
	case <-s.done:
		return 0, io.ErrClosedPipe
	case s.playback <- append([]byte(nil), b...):
		return len(b), nil
	}
}
func (s *syntheticModemPCM) Close() error { s.once.Do(func() { close(s.done) }); return nil }

// Verify both directions with DIFFERENT signals. A direct RTP echo cannot pass:
// browser sends negative speech samples; modem capture supplies positive ones.
func TestWebRTCPCMBridgeBothDirectionsAndCallGuard(t *testing.T) {
	modem := &syntheticModemPCM{capture: make(chan []byte, 4), playback: make(chan []byte, 16), done: make(chan struct{})}
	var safe atomic.Bool
	safe.Store(true)
	factory := voicepcm.ProtectFactory(func(context.Context) (voicepcm.Stream, error) { return modem, nil }, func(context.Context) error {
		if !safe.Load() {
			return errors.New("call/network no longer safe")
		}
		return nil
	})
	m, err := NewMedia("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "mic", "pcm-test")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b := make([]byte, 1500)
		for {
			if _, _, e := sender.Read(b); e != nil {
				return
			}
		}
	}()
	returned := make(chan []byte, 4)
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, e := remote.ReadRTP()
			if e != nil {
				return
			}
			select {
			case returned <- p.Payload:
			default:
			}
		}
	})
	connected := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal("ICE timeout")
	}
	var cleaned atomic.Int32
	_, answer, err := m.offer(ctx, "test-browser", time.Now().Add(time.Minute), *pc.LocalDescription(), "ec20-call", "/org/freedesktop/ModemManager1/Call/42", factory, func() { cleaned.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	if err = pc.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("DTLS failed")
	}
	pcm := bytes.Repeat([]byte{0xe8, 0x03}, 160)
	modem.capture <- pcm
	for i := 0; i < 6; i++ {
		if err = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i * 160), SSRC: 1234}, Payload: bytes.Repeat([]byte{0x4e}, 160)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case got := <-returned:
		if !bytes.Equal(got, bytes.Repeat([]byte{0xce}, 160)) {
			t.Fatal("modem PCM not encoded into browser RTP")
		}
	case <-ctx.Done():
		t.Fatal("no modem-to-browser media")
	}
	select {
	case got := <-modem.playback:
		if !bytes.Equal(got, bytes.Repeat([]byte{0x24, 0xfc}, 160)) {
			t.Fatal("browser PCMU not decoded into modem PCM")
		}
	case <-ctx.Done():
		t.Fatal("no browser-to-modem media")
	}
	safe.Store(false)
	select {
	case <-modem.done:
	case <-time.After(2 * time.Second):
		t.Fatal("unsafe call did not close PCM")
	}
	end := time.Now().Add(time.Second)
	for m.Stats("test-browser").Active && time.Now().Before(end) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Stats("test-browser").Active {
		t.Fatal("unsafe peer retained")
	}
	if cleaned.Load() != 1 {
		t.Fatal("per-call guard resources not released exactly once")
	}
}
