package voicepcm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var ErrNotReady = errors.New("EC20 UAC is not ready")
var usbPathPattern = regexp.MustCompile(`^[0-9]+-[0-9]+(?:\.[0-9]+)*$`)
var alsaIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

type Device struct {
	USBPath      string `json:"usb_path"`
	USBIdentity  string `json:"usb_identity"`
	Card         int    `json:"card"`
	CardID       string `json:"card_id"`
	CaptureNode  string `json:"capture_node"`
	PlaybackNode string `json:"playback_node"`
	Fingerprint  string `json:"fingerprint"`
}

func contained(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func read(path string) string { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }

// Discover only reads sysfs/procfs. USB identity and topology are mandatory;
// a generic/default ALSA card is never used as a substitute for the EC20.
func Discover(sysRoot, procRoot, usbPath string) (Device, error) {
	var none Device
	if !usbPathPattern.MatchString(usbPath) {
		return none, errors.New("invalid pinned USB topology")
	}
	usb, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "bus/usb/devices", usbPath))
	if err != nil {
		return none, fmt.Errorf("%w: pinned USB device absent", ErrNotReady)
	}
	if strings.ToLower(read(filepath.Join(usb, "idVendor"))) != "2c7c" || strings.ToLower(read(filepath.Join(usb, "idProduct"))) != "0125" {
		return none, fmt.Errorf("%w: USB identity mismatch", ErrNotReady)
	}
	cards, _ := filepath.Glob(filepath.Join(sysRoot, "class/sound/card*"))
	var matches []Device
	for _, card := range cards {
		index, e := strconv.Atoi(strings.TrimPrefix(filepath.Base(card), "card"))
		if e != nil {
			continue
		}
		device, e := filepath.EvalSymlinks(filepath.Join(card, "device"))
		if e != nil || !contained(usb, device) {
			continue
		}
		if read(filepath.Join(device, "bInterfaceClass")) != "01" {
			continue
		}
		id := read(filepath.Join(procRoot, "asound", fmt.Sprintf("card%d", index), "id"))
		if !alsaIDPattern.MatchString(id) {
			continue
		}
		stream := read(filepath.Join(procRoot, "asound", fmt.Sprintf("card%d", index), "stream0"))
		if !supportsDuplex(stream) {
			continue
		}
		d := Device{USBPath: usbPath, USBIdentity: "2c7c:0125", Card: index, CardID: id,
			CaptureNode: fmt.Sprintf("/dev/snd/pcmC%dD0c", index), PlaybackNode: fmt.Sprintf("/dev/snd/pcmC%dD0p", index)}
		valid := true
		for _, suffix := range []string{"c", "p"} {
			node := filepath.Join(sysRoot, "class/sound", fmt.Sprintf("pcmC%dD0%s", index, suffix))
			nodeDevice, e := filepath.EvalSymlinks(node)
			if e != nil || !contained(usb, nodeDevice) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		// Generation changes after re-enumeration even when a card number is
		// reused. The factory rechecks this fingerprint before/after opening.
		d.Fingerprint = strings.Join([]string{usb, read(filepath.Join(usb, "devnum")), device, strconv.Itoa(index), id}, "|")
		matches = append(matches, d)
	}
	if len(matches) != 1 {
		return none, fmt.Errorf("%w: need exactly one EC20 duplex S16_LE/8000/mono endpoint, found %d", ErrNotReady, len(matches))
	}
	return matches[0], nil
}

func supportsDuplex(stream string) bool {
	play, capture := strings.Index(stream, "Playback:"), strings.Index(stream, "Capture:")
	if play < 0 || capture < 0 {
		return false
	}
	blocks := []string{}
	if play < capture {
		blocks = []string{stream[play:capture], stream[capture:]}
	} else {
		blocks = []string{stream[capture:play], stream[play:]}
	}
	for _, direction := range blocks {
		supported := false
		for _, alt := range strings.Split(direction, "Altset") {
			format := regexp.MustCompile(`(?m)^\s*Format:\s*S16_LE\s*$`).MatchString(alt)
			mono := regexp.MustCompile(`(?m)^\s*Channels:\s*1\s*$`).MatchString(alt)
			rates := regexp.MustCompile(`(?m)^\s*Rates:[^\n]*\b8000\b`).MatchString(alt)
			if format && mono && rates {
				supported = true
				break
			}
		}
		if !supported {
			return false
		}
	}
	return true
}
