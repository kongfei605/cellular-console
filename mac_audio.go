package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kongfei605/cellular-console/internal/atmodem"
	"github.com/kongfei605/cellular-console/internal/audioloop"
	"github.com/kongfei605/cellular-console/internal/voicepcm"
)

// The web signalling keeps its existing opaque call-path wire format. The AT
// backend resolves its numeric suffix to a generation-scoped CLCC observation.
const callWirePrefix = "/org/freedesktop/ModemManager1/Call/"

type macVoice struct {
	backend *ATBackend
	app     *App
}

func (v macVoice) Ready() error {
	b := v.backend
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil || !b.client.Alive() || !b.reviewed() {
		return errors.New("Mac AT 后端未就绪")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := atmodem.ReadAudioInfo(ctx, b.helper, b.location)
	if err != nil {
		return err
	}
	if info.Session != b.device.Session {
		return errors.New("EC20 USB 音频代际已变化")
	}
	return nil
}
func (v macVoice) prepareControl(ctx context.Context, phase, wire string) error {
	f, err := v.app.failover()
	if err != nil || f.Active != "wired" || !f.WiredHealthy || !f.CellularHealthy {
		return errors.New("主网络和蜂窝备用必须健康")
	}
	if err := v.Ready(); err != nil {
		return err
	}
	b := v.backend
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.actionReady(ctx); err != nil {
		return err
	}
	if _, err := b.readCalls(ctx); err != nil {
		return err
	}
	id := strings.TrimPrefix(wire, callWirePrefix)
	found := false
	for _, call := range b.calls {
		if phase == "dial" || call.ID != id {
			return errors.New("存在其他呼叫")
		}
		if phase == "accept" && (call.StateCode != 3 || call.Direction != "incoming") || phase == "active" && call.StateCode != 4 {
			return errors.New("呼叫状态已变化")
		}
		found = true
	}
	if phase != "dial" && (!found || !strings.HasPrefix(wire, callWirePrefix)) {
		return errors.New("呼叫已结束")
	}
	if phase != "dial" && phase != "accept" && phase != "active" {
		return errors.New("invalid voice phase")
	}
	lines, err := b.command(ctx, "AT+QPCMV?")
	if err != nil {
		return err
	}
	fields := atFields(atValue(lines, "+QPCMV:"))
	on := len(fields) > 0 && fields[0] == "1" && (len(fields) == 1 || len(fields) == 2 && fields[1] == "2")
	if !on {
		if phase == "active" {
			return errors.New("USB 语音路由未准备，请在拨号或接听前准备")
		}
		if _, err = b.command(ctx, "AT+QPCMV=1,2"); err != nil {
			return err
		}
		lines, err = b.command(ctx, "AT+QPCMV?")
		if err != nil {
			return err
		}
		fields = atFields(atValue(lines, "+QPCMV:"))
		if len(fields) == 0 || fields[0] != "1" || len(fields) > 1 && fields[1] != "2" {
			return errors.New("USB 语音路由未确认")
		}
	}
	return nil
}
func (v macVoice) Prepare(ctx context.Context, wire string) (voicepcm.Factory, func(), error) {
	if err := v.prepareControl(ctx, "active", wire); err != nil {
		return nil, nil, err
	}
	b := v.backend
	b.mu.Lock()
	owner, location, session, helper := b.owner, b.location, b.device.Session, b.helper
	b.mu.Unlock()
	id := strings.TrimPrefix(wire, callWirePrefix)
	guard := func(ctx context.Context) error {
		f, err := v.app.failover()
		if err != nil || f.Active != "wired" || !f.WiredHealthy || !f.CellularHealthy {
			return errors.New("Mac 网络保护状态已变化")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.owner != owner || b.device.Session != session || b.client == nil || !b.client.Alive() {
			return errors.New("AT session changed")
		}
		if _, err := b.readCalls(ctx); err != nil {
			return err
		}
		for _, call := range b.calls {
			if call.ID == id && call.StateCode == 4 {
				return nil
			}
		}
		return errors.New("call ended")
	}
	open := func(ctx context.Context) (voicepcm.Stream, error) {
		info, err := atmodem.ReadAudioInfo(ctx, helper, location)
		if err != nil {
			return nil, err
		}
		if info.Session != session {
			return nil, errors.New("audio USB generation changed")
		}
		return atmodem.OpenAudio(helper, location)
	}
	return voicepcm.ProtectFactory(open, guard), func() {}, nil
}
func (a *App) startMacAudio(ctx context.Context, backend *ATBackend, voice bool) (func(), error) {
	media, err := audioloop.NewMedia("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	key := randomToken()
	gateway, err := audioloop.NewGatewayServer(media, key)
	if err != nil {
		media.Close()
		return nil, err
	}
	if voice {
		provider := macVoice{backend: backend, app: a}
		gateway.Voice = provider
		a.voicePrepare = provider.prepareControl
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		media.Close()
		return nil, err
	}
	proxy, err := newAudioProxy("http://"+listener.Addr().String(), key)
	if err != nil {
		listener.Close()
		media.Close()
		return nil, err
	}
	a.audio = proxy
	srv := &http.Server{Handler: gateway.GatewayHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, MaxHeaderBytes: 16384}
	go func() { _ = srv.Serve(listener) }()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				media.Sweep(now)
			}
		}
	}()
	return func() { srv.Close(); media.Close() }, nil
}
