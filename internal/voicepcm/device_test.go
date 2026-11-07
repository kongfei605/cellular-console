package voicepcm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const duplexStream = "Playback:\n  Interface 6\n    Altset 1\n    Format: S16_LE\n    Channels: 1\n    Rates: 8000\nCapture:\n  Interface 7\n    Altset 1\n    Format: S16_LE\n    Channels: 1\n    Rates: 8000\n"

func fixture(t *testing.T) (sys, proc, usb string) {
	t.Helper()
	root := t.TempDir()
	sys = filepath.Join(root, "sys")
	proc = filepath.Join(root, "proc")
	usb = filepath.Join(sys, "devices/ec20")
	writeFixture(t, filepath.Join(usb, "idVendor"), "2c7c")
	writeFixture(t, filepath.Join(usb, "idProduct"), "0125")
	writeFixture(t, filepath.Join(usb, "devnum"), "41")
	linkFixture(t, usb, filepath.Join(sys, "bus/usb/devices/1-2"))
	return
}
func writeFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
func linkFixture(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
func addCard(t *testing.T, sys, proc, device string, index int) {
	t.Helper()
	iface := filepath.Join(device, fmt.Sprintf("1-2:1.%d", index))
	writeFixture(t, filepath.Join(iface, "bInterfaceClass"), "01")
	linkFixture(t, iface, filepath.Join(sys, fmt.Sprintf("class/sound/card%d/device", index)))
	writeFixture(t, filepath.Join(proc, fmt.Sprintf("asound/card%d/id", index)), fmt.Sprintf("Voice%d", index))
	writeFixture(t, filepath.Join(proc, fmt.Sprintf("asound/card%d/stream0", index)), duplexStream)
	for _, dir := range []string{"c", "p"} {
		node := fmt.Sprintf("pcmC%dD0%s", index, dir)
		target := filepath.Join(iface, "sound", fmt.Sprintf("card%d", index), node)
		if err := os.MkdirAll(target, 0755); err != nil {
			t.Fatal(err)
		}
		linkFixture(t, target, filepath.Join(sys, "class/sound", node))
	}
}
func TestDiscoverNeverUsesMotherboardOrDefaultCard(t *testing.T) {
	sys, proc, usb := fixture(t)
	addCard(t, sys, proc, filepath.Join(sys, "devices/motherboard"), 2)
	if _, err := Discover(sys, proc, "1-2"); !errors.Is(err, ErrNotReady) {
		t.Fatal("motherboard card selected")
	}
	addCard(t, sys, proc, usb, 3)
	d, err := Discover(sys, proc, "1-2")
	if err != nil {
		t.Fatal(err)
	}
	if d.Card != 3 || d.CardID != "Voice3" || d.USBIdentity != "2c7c:0125" {
		t.Fatalf("wrong device: %+v", d)
	}
	writeFixture(t, filepath.Join(usb, "devnum"), "42")
	next, err := Discover(sys, proc, "1-2")
	if err != nil || d.Fingerprint == next.Fingerprint {
		t.Fatal("USB generation change missed")
	}
}
func TestDiscoverRejectsWrongIdentityAndIncompleteDuplex(t *testing.T) {
	sys, proc, usb := fixture(t)
	addCard(t, sys, proc, usb, 3)
	writeFixture(t, filepath.Join(usb, "idVendor"), "0db0")
	if _, err := Discover(sys, proc, "1-2"); err == nil {
		t.Fatal("wrong vendor accepted")
	}
	writeFixture(t, filepath.Join(usb, "idVendor"), "2c7c")
	os.Remove(filepath.Join(sys, "class/sound/pcmC3D0c"))
	if _, err := Discover(sys, proc, "1-2"); err == nil {
		t.Fatal("capture missing accepted")
	}
	if _, err := Discover(sys, proc, "../1-6"); err == nil {
		t.Fatal("path traversal accepted")
	}
}
func TestDiscoverRejectsAmbiguousCards(t *testing.T) {
	sys, proc, usb := fixture(t)
	addCard(t, sys, proc, usb, 3)
	addCard(t, sys, proc, usb, 4)
	if _, err := Discover(sys, proc, "1-2"); err == nil {
		t.Fatal("ambiguous EC20 endpoint selected")
	}
}
func TestCapabilitiesDoNotCombineDifferentAlternateSettings(t *testing.T) {
	bad := "Playback:\n Altset 1\n Format: S16_LE\n Channels: 2\n Rates: 48000\n Altset 2\n Format: S24_LE\n Channels: 1\n Rates: 8000\nCapture:\n Altset 1\n Format: S16_LE\n Channels: 1\n Rates: 8000\n"
	if supportsDuplex(bad) {
		t.Fatal("format/rate/channels combined across incompatible altsets")
	}
}
