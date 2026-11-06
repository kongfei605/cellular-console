package audioloop

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func testMedia(t *testing.T) *Media {
	t.Helper()
	m, err := NewMedia("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// Exercise ICE, DTLS-SRTP and an exact PCMU payload round trip through the
// production echo implementation; no microphone, ALSA, modem or routes involved.
func TestEncryptedAudioRoundTripAndOwnership(t *testing.T) {
	m := testMedia(t)
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "input", "test")
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
			if _, _, err := sender.Read(b); err != nil {
				return
			}
		}
	}()
	connected := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	returned := make(chan []byte, 1)
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		p, _, e := remote.ReadRTP()
		if e == nil {
			returned <- p.Payload
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("gather timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, answer, err := m.Offer(ctx, "owner", time.Now().Add(time.Hour), *pc.LocalDescription())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.Offer(ctx, "other", time.Now().Add(time.Hour), *pc.LocalDescription()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent peer accepted: %v", err)
	}
	if m.Stop("other", id) {
		t.Fatal("other session stopped owner")
	}
	if stats := m.Stats("other"); !stats.Active || stats.Owned || stats.ID != "" || stats.Received != 0 {
		t.Fatal("owner data leaked")
	}
	if err = pc.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("ICE/DTLS failed")
	}
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = byte(i)
	}
	for i := 0; i < 6; i++ {
		err = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i * 160), SSRC: 1234}, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case got := <-returned:
		if !bytes.Equal(payload, got) {
			t.Fatal("audio payload changed")
		}
	case <-ctx.Done():
		t.Fatal("no reflected audio")
	}
	stats := m.Stats("owner")
	if stats.Received < 1 || stats.Reflected < 1 || !stats.Owned {
		t.Fatalf("missing media counters: %+v", stats)
	}
	if stats.Expires.After(time.Now().Add(3 * time.Minute)) {
		t.Fatal("absolute limit missing")
	}
	if !m.Stop("owner", id) || m.Stats("owner").Active {
		t.Fatal("stop did not release media")
	}
}

func TestInvalidOfferDoesNotOccupySlot(t *testing.T) {
	m := testMedia(t)
	if _, _, err := m.Offer(context.Background(), "x", time.Now(), webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "bad"}); err == nil {
		t.Fatal("bad SDP accepted")
	}
	if m.Stats("x").Active {
		t.Fatal("bad SDP occupied slot")
	}
}

func TestIdleAndAbsoluteExpiry(t *testing.T) {
	for _, reason := range []string{"idle", "expired"} {
		t.Run(reason, func(t *testing.T) {
			m := testMedia(t)
			pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			p := &Peer{ID: "test", Owner: "x", PC: pc, Created: now, Expires: now.Add(time.Hour)}
			if reason == "idle" {
				p.Created = now.Add(-31 * time.Second)
			} else {
				p.Expires = now.Add(-time.Second)
				p.LastPacket.Store(now.UnixNano())
			}
			m.mu.Lock()
			m.active = p
			m.mu.Unlock()
			m.Sweep(now)
			if m.Stats("x").Active {
				t.Fatal("expired media retained")
			}
		})
	}
}

func TestWildcardBindRejected(t *testing.T) {
	if m, err := NewMedia("0.0.0.0:0"); err == nil {
		m.Close()
		t.Fatal("wildcard media bind allowed")
	}
}
