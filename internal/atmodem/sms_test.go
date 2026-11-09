package atmodem

import (
	"strings"
	"testing"
)

func TestSubmitGoldenPDUs(t *testing.T) {
	for _, tc := range []struct{ text, pdu string }{
		{"hello", "0001000B912120550521F3000005E8329BFD06"},
		{"你好", "0001000B912120550521F30008044F60597D"},
	} {
		parts, err := EncodeSubmit("+12025550123", tc.text, 42)
		if err != nil || len(parts) != 1 || parts[0].Hex != tc.pdu || parts[0].Length != len(tc.pdu)/2-1 {
			t.Fatal(parts, err)
		}
	}
}
func TestSubmitMultipartEscapeAndLimits(t *testing.T) {
	parts, err := EncodeSubmit("12025550123", strings.Repeat("a", 152)+"^"+strings.Repeat("b", 10), 42)
	if err != nil || len(parts) != 2 {
		t.Fatal(parts, err)
	}
	if !strings.Contains(parts[0].Hex, "0500032A0201") || !strings.Contains(parts[1].Hex, "0500032A0202") {
		t.Fatal("missing concatenation header", parts)
	}
	for _, text := range []string{strings.Repeat("中", 202), "😀", ""} {
		if _, err := EncodeSubmit("12025550123", text, 1); err == nil {
			t.Fatal("invalid payload accepted")
		}
	}
	if _, err := EncodeSubmit("1555\rATD1", "hello", 1); err == nil {
		t.Fatal("invalid number accepted")
	}
}
func TestDeliverGoldenPDUsAndMalformedInput(t *testing.T) {
	for _, tc := range []struct{ pdu, text string }{
		{"00000B912120550521F300004211902143650005E8329BFD06", "hello"},
		{"00000B912120550521F3000842119021436500044F60597D", "你好"},
	} {
		d, err := DecodeDelivery(tc.pdu)
		if err != nil || d.Sender != "+12025550123" || d.Text != tc.text || d.Timestamp != "2024-11-09T12:34:56Z" {
			t.Fatal(d, err)
		}
	}
	for _, pdu := range []string{"", "00", "FF00", "00000B912120550521F3000842119021436500044F"} {
		if _, err := DecodeDelivery(pdu); err == nil {
			t.Fatal("truncated delivery accepted")
		}
	}
}
func TestDeliverMultipartAndTimezone(t *testing.T) {
	d, err := DecodeDelivery("00400B912120550521F3000842119021436523080500032A02014F60")
	if err != nil || d.Text != "你" || d.Total != 2 || d.Part != 1 || d.Reference != 42 || d.Timestamp != "2024-11-09T12:34:56+08:00" {
		t.Fatal(d, err)
	}
}
