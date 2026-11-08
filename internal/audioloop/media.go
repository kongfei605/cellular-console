// Package audioloop implements authenticated software echo and explicitly guarded EC20 UAC media.
package audioloop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kongfei605/cellular-console/internal/voicepcm"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

var ErrBusy = errors.New("已有音频回环正在运行，请先停止或等待自动释放")

type Media struct {
	mu         sync.Mutex
	api        *webrtc.API
	mux        ice.UDPMux
	active     *Peer
	closed     bool
	pcmFactory voicepcm.Factory
}

type Peer struct {
	Mode       string
	Call       string
	factory    voicepcm.Factory
	cleanup    func()
	ID         string
	Owner      string
	PC         *webrtc.PeerConnection
	Created    time.Time
	Expires    time.Time
	Received   atomic.Uint64
	Reflected  atomic.Uint64
	LastPacket atomic.Int64
	once       sync.Once
	ctx        context.Context
	cancel     context.CancelFunc
	startPCM   sync.Once
	resourceMu sync.Mutex
	pcm        voicepcm.Stream
	jitter     voicepcm.Jitter
}

type MediaStats struct {
	VoiceReady  bool      `json:"voice_ready"`
	VoiceReason string    `json:"voice_reason,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Call        string    `json:"call,omitempty"`
	Active      bool      `json:"active"`
	Owned       bool      `json:"owned"`
	ID          string    `json:"id,omitempty"`
	State       string    `json:"state,omitempty"`
	Received    uint64    `json:"received"`
	Reflected   uint64    `json:"reflected"`
	Expires     time.Time `json:"expires,omitempty"`
}

func NewMedia(bind string) (*Media, error) {
	return newMedia(bind, nil)
}

// NewMediaWithPCM is a library entry point. Production call offers supply a
// per-peer EC20-only guarded factory and call identity instead of a global factory.
// All ordinary callers continue to use NewMedia's hardware-free RTP echo.
func NewMediaWithPCM(bind string, factory voicepcm.Factory) (*Media, error) {
	if factory == nil {
		return nil, errors.New("explicit guarded PCM factory required")
	}
	return newMedia(bind, factory)
}

func newMedia(bind string, factory voicepcm.Factory) (*Media, error) {
	addr, err := net.ResolveUDPAddr("udp4", bind)
	if err != nil || addr.IP == nil || addr.IP.IsUnspecified() {
		return nil, errors.New("media bind must specify one IPv4 address")
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn})
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return ip.Equal(addr.IP) })
	se.SetIncludeLoopbackCandidate(addr.IP.IsLoopback())
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetICETimeouts(10*time.Second, 20*time.Second, 2*time.Second)
	me := &webrtc.MediaEngine{}
	err = me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, PayloadType: 0}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		mux.Close()
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err = webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		mux.Close()
		return nil, err
	}
	return &Media{api: webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se), webrtc.WithInterceptorRegistry(ir)), mux: mux, pcmFactory: factory}, nil
}

func validateOffer(offer webrtc.SessionDescription) error {
	if offer.Type != webrtc.SDPTypeOffer || len(offer.SDP) > 60000 {
		return errors.New("invalid offer")
	}
	var desc sdp.SessionDescription
	if err := desc.Unmarshal([]byte(offer.SDP)); err != nil {
		return err
	}
	if len(desc.MediaDescriptions) != 1 || desc.MediaDescriptions[0].MediaName.Media != "audio" {
		return errors.New("one audio track required")
	}
	md := desc.MediaDescriptions[0]
	if _, ok := md.Attribute("sendrecv"); !ok {
		return errors.New("sendrecv audio required")
	}
	return nil
}

func (m *Media) Offer(ctx context.Context, owner string, expires time.Time, offer webrtc.SessionDescription) (string, webrtc.SessionDescription, error) {
	return m.offer(ctx, owner, expires, offer, "software-loopback", "", m.pcmFactory, nil)
}

func (m *Media) offer(ctx context.Context, owner string, expires time.Time, offer webrtc.SessionDescription, mode, call string, factory voicepcm.Factory, cleanup func()) (string, webrtc.SessionDescription, error) {
	if err := validateOffer(offer); err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", webrtc.SessionDescription{}, errors.New("service stopped")
	}
	if m.active != nil {
		m.mu.Unlock()
		return "", webrtc.SessionDescription{}, ErrBusy
	}
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		m.mu.Unlock()
		return "", webrtc.SessionDescription{}, err
	}
	p := &Peer{ID: token(), Owner: owner, PC: pc, Created: time.Now(), Expires: expires, Mode: mode, Call: call, factory: factory, cleanup: cleanup}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	if max := p.Created.Add(3 * time.Minute); p.Expires.After(max) {
		p.Expires = max
	}
	m.active = p
	m.mu.Unlock()
	success := false
	defer func() {
		if !success {
			m.stop(p)
		}
	}()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "echo", "ec20-audio-loopback")
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	go func() {
		b := make([]byte, 1500)
		for {
			if _, _, e := sender.Read(b); e != nil {
				return
			}
		}
	}()
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected && p.factory != nil {
			p.startPCM.Do(func() { go m.runPCM(p, track) })
		}
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			go m.stop(p)
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.Kind() != webrtc.RTPCodecTypeAudio || !strings.EqualFold(remote.Codec().MimeType, webrtc.MimeTypePCMU) {
			go m.stop(p)
			return
		}
		for {
			packet, _, e := remote.ReadRTP()
			if e != nil {
				return
			}
			p.Received.Add(1)
			p.LastPacket.Store(time.Now().UnixNano())
			if len(packet.Payload) == 0 || len(packet.Payload) > 1600 {
				continue
			}
			if p.factory != nil {
				p.jitter.Push(packet.Timestamp, packet.Payload)
				continue
			}
			// Do not reflect client-specific MID/audio-level extensions into the
			// server's negotiated extension map. Pion rewrites SSRC/payload type.
			packet.Header.Extension = false
			packet.Header.Extensions = nil
			packet.Header.ExtensionProfile = 0
			if e = track.WriteRTP(packet); e != nil {
				return
			}
			p.Reflected.Add(1)
		}
	})
	if err = pc.SetRemoteDescription(offer); err != nil {
		return "", webrtc.SessionDescription{}, fmt.Errorf("remote offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return "", webrtc.SessionDescription{}, err
	}
	select {
	case <-ctx.Done():
		return "", webrtc.SessionDescription{}, ctx.Err()
	case <-gathered:
	}
	success = true
	return p.ID, *pc.LocalDescription(), nil
}

func (m *Media) stop(p *Peer) {
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		p.resourceMu.Lock()
		pcm := p.pcm
		p.pcm = nil
		p.resourceMu.Unlock()
		if pcm != nil {
			pcm.Close()
		}
		if p.cleanup != nil {
			p.cleanup()
		}
		p.PC.Close()
		m.mu.Lock()
		if m.active == p {
			m.active = nil
		}
		m.mu.Unlock()
	})
}

func (m *Media) runPCM(p *Peer, track *webrtc.TrackLocalStaticRTP) {
	stream, err := p.factory(p.ctx)
	if err != nil || stream == nil {
		m.stop(p)
		return
	}
	p.resourceMu.Lock()
	if p.ctx.Err() != nil {
		p.resourceMu.Unlock()
		stream.Close()
		return
	}
	p.pcm = stream
	p.resourceMu.Unlock()
	// Capture is paced at 20ms even when the USB driver supplies 40ms bursts.
	go func() {
		defer m.stop(p)
		frame := make([]byte, voicepcm.FrameBytes)
		seed := make([]byte, 6)
		if _, err := rand.Read(seed); err != nil {
			return
		}
		seq, stamp := binary.BigEndian.Uint16(seed), binary.BigEndian.Uint32(seed[2:])
		var next time.Time
		for {
			if _, err := io.ReadFull(stream, frame); err != nil {
				return
			}
			if next.IsZero() {
				next = time.Now()
			}
			if time.Since(next) > 200*time.Millisecond {
				return
			}
			wait := time.NewTimer(time.Until(next))
			select {
			case <-p.ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
			encoded, err := voicepcm.EncodePCM16LE(frame)
			if err != nil {
				return
			}
			packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: stamp}, Payload: encoded}
			if err = track.WriteRTP(packet); err != nil {
				return
			}
			p.Reflected.Add(1)
			seq++
			stamp += voicepcm.FrameSamples
			next = next.Add(20 * time.Millisecond)
		}
	}()
	defer m.stop(p)
	// Start with 40ms prebuffering; late packets are dropped rather than
	// accumulating latency. Missing samples become silence, never old speech.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var ready time.Time
	last := time.Now()
	for {
		select {
		case <-p.ctx.Done():
			return
		case now := <-ticker.C:
			if now.Sub(last) > 200*time.Millisecond {
				return
			}
			last = now
			if ready.IsZero() {
				queued, _, _ := p.jitter.Stats()
				if queued == 0 {
					continue
				}
				ready = now.Add(40 * time.Millisecond)
				continue
			}
			if now.Before(ready) {
				continue
			}
			frame, started := p.jitter.Pop()
			if !started {
				continue
			}
			pcm := voicepcm.DecodePCM16LE(frame)
			if _, err := io.Copy(stream, bytes.NewReader(pcm)); err != nil {
				return
			}
		}
	}
}

func (m *Media) Stop(owner, id string) bool {
	m.mu.Lock()
	p := m.active
	m.mu.Unlock()
	if p == nil {
		return true
	}
	if p.Owner != owner || p.ID != id {
		return false
	}
	m.stop(p)
	return true
}

func (m *Media) StopOwner(owner string) {
	m.mu.Lock()
	p := m.active
	m.mu.Unlock()
	if p != nil && p.Owner == owner {
		m.stop(p)
	}
}

func (m *Media) Stats(owner string) MediaStats {
	m.mu.Lock()
	p := m.active
	m.mu.Unlock()
	if p == nil {
		return MediaStats{}
	}
	if p.Owner != owner {
		return MediaStats{Active: true}
	}
	return MediaStats{Mode: p.Mode, Call: p.Call, Active: true, Owned: true, ID: p.ID, State: p.PC.ConnectionState().String(), Received: p.Received.Load(), Reflected: p.Reflected.Load(), Expires: p.Expires}
}

func (m *Media) Sweep(now time.Time) {
	m.mu.Lock()
	p := m.active
	m.mu.Unlock()
	if p == nil {
		return
	}
	last := p.Created
	if n := p.LastPacket.Load(); n != 0 {
		last = time.Unix(0, n)
	}
	if now.After(p.Expires) || now.Sub(last) > 30*time.Second {
		m.stop(p)
	}
}

func (m *Media) Close() error {
	m.mu.Lock()
	m.closed = true
	p := m.active
	m.mu.Unlock()
	if p != nil {
		m.stop(p)
	}
	return m.mux.Close()
}
