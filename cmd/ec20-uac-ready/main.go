// ec20-uac-ready is a read-only readiness check. It never opens /dev/snd, AT
// ports, D-Bus, or any audio capture/playback process and never resets hardware.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/kongfei605/ec20-toolbox/internal/voicecontrol"
	"os"
	"time"

	"github.com/kongfei605/ec20-toolbox/internal/voicepcm"
)

func main() {
	path := flag.String("usb-path", "1-2", "pinned EC20 USB topology, not an ALSA card number")
	voice := flag.Bool("voice-route", false, "also verify the root-prepared route and read-only modem/call/network guards")
	flag.Parse()
	d, err := voicepcm.Discover("/sys", "/proc", *path)
	if err != nil {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": false, "reason": err.Error(), "hardware_opened": false, "configuration_changed": false})
		os.Exit(2)
	}
	if *voice {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if _, e := (voicecontrol.Client{}).Prepare(ctx, "dial", ""); e != nil {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": false, "reason": e.Error(), "hardware_opened": false})
			os.Exit(2)
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "device": d, "hardware_opened": false, "configuration_changed": false, "call_audio_verified": false})
}
