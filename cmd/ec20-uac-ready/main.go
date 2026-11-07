// ec20-uac-ready is a read-only readiness check. It never opens /dev/snd, AT
// ports, D-Bus, or any audio capture/playback process and never resets hardware.
package main

import (
	"encoding/json"
	"flag"
	"os"

	"github.com/kongfei605/ec20-toolbox/internal/voicepcm"
)

func main() {
	path := flag.String("usb-path", "1-2", "pinned EC20 USB topology, not an ALSA card number")
	flag.Parse()
	d, err := voicepcm.Discover("/sys", "/proc", *path)
	if err != nil {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": false, "reason": err.Error(), "hardware_opened": false, "configuration_changed": false})
		os.Exit(2)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "device": d, "hardware_opened": false, "configuration_changed": false, "call_audio_verified": false})
}
