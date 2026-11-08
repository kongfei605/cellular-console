// ec20-audio-probe verifies the deployed HTTPS + DTLS-SRTP echo using synthetic
// PCMU packets. Administrator credentials are accepted only on stdin.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func run() error {
	origin := flag.String("origin", "https://127.0.0.1:8788", "audio service origin")
	certPath := flag.String("cert", "", "explicit trusted server certificate file")
	flag.Parse()
	if *certPath == "" {
		return fmt.Errorf("-cert required")
	}
	cert, err := os.ReadFile(*certPath)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		return fmt.Errorf("invalid certificate")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 15 * time.Second, Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err = json.NewDecoder(io.LimitReader(os.Stdin, 4096)).Decode(&creds); err != nil {
		return fmt.Errorf("credentials JSON required on stdin")
	}
	csrf := ""
	post := func(path string, body, out any) error {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r, e := http.NewRequestWithContext(ctx, "POST", *origin+path, bytes.NewReader(b))
		if e != nil {
			return e
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", *origin)
		r.Header.Set("X-CSRF-Token", csrf)
		res, e := client.Do(r)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("%s returned %d", path, res.StatusCode)
		}
		if out != nil {
			return json.NewDecoder(res.Body).Decode(out)
		}
		return nil
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	if err = post("/api/login", creds, &login); err != nil {
		return err
	}
	creds.Password = ""
	csrf = login.CSRF
	defer post("/api/logout", map[string]any{}, nil)
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "probe", "software-test")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	go func() {
		b := make([]byte, 1500)
		for {
			if _, _, e := sender.Read(b); e != nil {
				return
			}
		}
	}()
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = byte(25 + i)
	}
	var good, bad atomic.Uint64
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, e := remote.ReadRTP()
			if e != nil {
				return
			}
			if bytes.Equal(p.Payload, payload) {
				good.Add(1)
			} else {
				bad.Add(1)
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
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	}
	var result struct {
		ID     string                    `json:"id"`
		Answer webrtc.SessionDescription `json:"answer"`
	}
	if err = post("/api/loopback", pc.LocalDescription(), &result); err != nil {
		return err
	}
	defer post("/api/loopback/stop", map[string]string{"id": result.ID}, nil)
	if err = pc.SetRemoteDescription(result.Answer); err != nil {
		return err
	}
	select {
	case <-connected:
	case <-ctx.Done():
		return fmt.Errorf("ICE/DTLS: %w", ctx.Err())
	}
	for i := 0; i < 80; i++ {
		if err = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i * 160), SSRC: 3242}, Payload: payload}); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	end := time.Now().Add(3 * time.Second)
	for good.Load() < 70 && time.Now().Before(end) {
		time.Sleep(50 * time.Millisecond)
	}
	if good.Load() < 70 || bad.Load() != 0 {
		return fmt.Errorf("payload echo failed: good=%d bad=%d", good.Load(), bad.Load())
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"passed": true, "https_verified": true, "transport": "DTLS-SRTP", "codec": "PCMU/8000/1", "sent": 80, "received_identical": good.Load(), "payload_mismatches": bad.Load(), "hardware_access": false})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
