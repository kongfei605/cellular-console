// ec20-uac-ready checks sysfs/procfs without opening sound/AT devices.
// -voice-route additionally reads the configured MM/call/network guards;
// neither mode writes modem configuration or resets hardware.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/kongfei605/cellular-console/internal/hostconfig"
	"github.com/kongfei605/cellular-console/internal/voicecontrol"
	"os"
	"time"

	"github.com/kongfei605/cellular-console/internal/voicepcm"
)

func main() {
	path := flag.String("usb-path", "1-2", "pinned EC20 USB topology, not an ALSA card number")
	voice := flag.Bool("voice-route", false, "also verify the root-prepared route and read-only modem/call/network guards")
	hostPath := flag.String("host-config", hostconfig.DefaultPath, "root-owned reviewed hardware profile")
	failover := flag.String("failover-state", "/run/ec20-failover/status.json", "independent failover status")
	flag.Parse()
	var host hostconfig.Config
	if *voice {
		var e error
		host, e = hostconfig.Load(*hostPath)
		if e != nil {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": false, "reason": e.Error()})
			os.Exit(2)
		}
		*path = host.USBPath
	}
	d, err := voicepcm.Discover("/sys", "/proc", *path)
	if err != nil {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": false, "reason": err.Error(), "hardware_opened": false, "configuration_changed": false})
		os.Exit(2)
	}
	if *voice {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if _, e := (voicecontrol.Client{Config: host, Failover: *failover}).Prepare(ctx, "dial", ""); e != nil {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": false, "reason": e.Error(), "hardware_opened": false})
			os.Exit(2)
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true, "device": d, "hardware_opened": false, "configuration_changed": false, "call_audio_verified": false})
}
