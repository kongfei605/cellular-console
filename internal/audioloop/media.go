// Package audioloop implements a hardware-independent, authenticated WebRTC echo.
package audioloop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

var ErrBusy = errors.New("已有音频回环正在运行，请先停止或等待自动释放")

type Media struct {
	mu     sync.Mutex
	api    *webrtc.API
	mux    ice.UDPMux
	active *Peer
	closed bool
}

type Peer struct {
	ID         string
	Owner      string
	PC         *webrtc.PeerConnection
	Created    time.Time
	Expires    time.Time
	Received   atomic.Uint64
	Reflected  atomic.Uint64
	LastPacket atomic.Int64
	once       sync.Once
}

type MediaStats struct {
	Active    bool      `json:"active"`
	Owned     bool      `json:"owned"`
	ID        string    `json:"id,omitempty"`
	State     string    `json:"state,omitempty"`
	Received  uint64    `json:"received"`
	Reflected uint64    `json:"reflected"`
	Expires   time.Time `json:"expires,omitempty"`
}

func NewMedia(bind string) (*Media, error) {
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
	return &Media{api: webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se), webrtc.WithInterceptorRegistry(ir)), mux: mux}, nil
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
	p := &Peer{ID: token(), Owner: owner, PC: pc, Created: time.Now(), Expires: expires}
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
		p.PC.Close()
		m.mu.Lock()
		if m.active == p {
			m.active = nil
		}
		m.mu.Unlock()
	})
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
	return MediaStats{Active: true, Owned: true, ID: p.ID, State: p.PC.ConnectionState().String(), Received: p.Received.Load(), Reflected: p.Reflected.Load(), Expires: p.Expires}
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
