package voicepcm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestCodecAgainstIndependentFullRangeReference(t *testing.T) {
	var ref struct {
		Vectors []struct {
			PCM  int16 `json:"pcm"`
			PCMU byte  `json:"pcmu"`
		}
		SHA string `json:"full_int16_range_sha256"`
	}
	b, err := os.ReadFile("testdata/g711-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	for _, v := range ref.Vectors {
		if got := EncodeULaw(v.PCM); got != v.PCMU {
			t.Fatalf("PCM %d got %02x want %02x", v.PCM, got, v.PCMU)
		}
	}
	all := make([]byte, 65536)
	golden, err := os.ReadFile("testdata/g711-full-range.bin")
	if err != nil {
		t.Fatal(err)
	}
	for i := -32768; i <= 32767; i++ {
		all[i+32768] = EncodeULaw(int16(i))
		if all[i+32768] != golden[i+32768] {
			t.Fatalf("first full reference mismatch at %d: got %02x want %02x", i, all[i+32768], golden[i+32768])
		}
		delta := i - int(DecodeULaw(all[i+32768]))
		if delta < 0 {
			delta = -delta
		}
		if delta > 644 {
			t.Fatalf("excess quantisation error at %d: %d", i, delta)
		}
	}
	sum := sha256.Sum256(all)
	if hex.EncodeToString(sum[:]) != ref.SHA {
		t.Fatal("full-range G.711 reference mismatch")
	}
}

func TestDecodeVectorsAndPCMEndian(t *testing.T) {
	for encoded, want := range map[byte]int16{0xff: 0, 0x7f: 0, 0x80: 32124, 0x00: -32124, 0xd5: 716, 0x55: -716, 0xce: 988, 0x4e: -988} {
		if got := DecodeULaw(encoded); got != want {
			t.Fatalf("decode %02x=%d want %d", encoded, got, want)
		}
	}
	pcm := []byte{0xe8, 0x03, 0x18, 0xfc}
	enc, err := EncodePCM16LE(pcm)
	if err != nil || len(enc) != 2 || enc[0] != 0xce || enc[1] != 0x4e {
		t.Fatalf("LE codec: %v %v", enc, err)
	}
	decoded := DecodePCM16LE(enc)
	if string(decoded) != string([]byte{0xdc, 0x03, 0x24, 0xfc}) {
		t.Fatal("wrong PCM16LE decoding")
	}
	if _, err = EncodePCM16LE([]byte{1}); err == nil {
		t.Fatal("partial sample accepted")
	}
	for i := 0; i < 256; i++ {
		got := EncodeULaw(DecodeULaw(byte(i)))
		want := byte(i)
		if want == 0x7f {
			want = 0xff
		}
		if got != want {
			t.Fatalf("canonical code round trip %02x=%02x", i, got)
		}
	}
}
