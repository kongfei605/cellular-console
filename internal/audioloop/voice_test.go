package audioloop

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kongfei605/ec20-toolbox/internal/voicepcm"
	"github.com/pion/webrtc/v4"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type refusingVoice struct{ calls int }

func (v *refusingVoice) Ready() error { return nil }
func (v *refusingVoice) Prepare(context.Context, string) (voicepcm.Factory, func(), error) {
	v.calls++
	return nil, nil, errors.New("no active call")
}
func TestRealAudioCannotFallBackToSoftwareEcho(t *testing.T) {
	s, e := NewGatewayServer(testMedia(t), strings.Repeat("a", 64))
	if e != nil {
		t.Fatal(e)
	}
	pc, e := webrtc.NewPeerConnection(webrtc.Configuration{})
	if e != nil {
		t.Fatal(e)
	}
	defer pc.Close()
	if _, e = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); e != nil {
		t.Fatal(e)
	}
	offer, e := pc.CreateOffer(nil)
	if e != nil {
		t.Fatal(e)
	}
	run := func(sdp string) int {
		b, _ := json.Marshal(map[string]any{"type": "offer", "sdp": sdp, "mode": "ec20-call", "call": "/org/freedesktop/ModemManager1/Call/42"})
		r := httptest.NewRequest("POST", "http://127.0.0.1/api/loopback", strings.NewReader(string(b)))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-EC20-Gateway-Key", strings.Repeat("a", 64))
		r.Header.Set("X-EC20-Owner", strings.Repeat("b", 64))
		r.Header.Set("X-EC20-Expires", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w := httptest.NewRecorder()
		s.GatewayHandler().ServeHTTP(w, r)
		return w.Code
	}
	if run(offer.SDP) != 503 {
		t.Fatal("unconfigured voice fell back to echo")
	}
	v := &refusingVoice{}
	s.Voice = v
	if run("bad SDP") != 400 || v.calls != 0 {
		t.Fatal("bad SDP reached hardware preparation")
	}
	if run(offer.SDP) != 409 || v.calls != 1 || s.media.Stats("").Active {
		t.Fatal("unready call occupied peer or fell back")
	}
}
