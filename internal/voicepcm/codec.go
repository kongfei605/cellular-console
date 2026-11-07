// Package voicepcm prepares the EC20 call-audio data path. Nothing in this
// package can change modem configuration, dial, reset a device or alter routes.
package voicepcm

import (
	"encoding/binary"
	"errors"
)

const (
	Rate         = 8000
	FrameSamples = 160 // 20ms of mono PCMU / PCM16.
	FrameBytes   = FrameSamples * 2
)

// EncodeULaw selects the closest mu-law reconstruction level after 14-bit
// quantisation. Midpoint rounding matches the independent pcm_mulaw vectors.
// Searching 128 levels avoids a large encoder lookup table or a native codec.
func EncodeULaw(sample int16) byte {
	x := int(sample) >> 2
	negative := x < 0
	if negative {
		x = -x
	}
	lo, hi := 0, 127
	for lo < hi {
		mid := (lo + hi) / 2
		lower, upper := int(DecodeULaw(0xff-byte(mid))), int(DecodeULaw(0xff-byte(mid+1)))
		boundary := (lower + upper + 4) / 8
		if x < boundary {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	code := 0xff - byte(lo)
	if negative {
		code ^= 0x80
	}
	return code
}

func DecodeULaw(encoded byte) int16 {
	u := ^encoded
	x := ((int(u&0x0f) << 3) + 0x84) << ((u >> 4) & 7)
	x -= 0x84
	if u&0x80 != 0 {
		x = -x
	}
	return int16(x)
}

func EncodePCM16LE(pcm []byte) ([]byte, error) {
	if len(pcm)%2 != 0 {
		return nil, errors.New("PCM16 requires complete little-endian samples")
	}
	encoded := make([]byte, len(pcm)/2)
	for i := range encoded {
		encoded[i] = EncodeULaw(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}
	return encoded, nil
}

func DecodePCM16LE(encoded []byte) []byte {
	pcm := make([]byte, len(encoded)*2)
	for i, b := range encoded {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(DecodeULaw(b)))
	}
	return pcm
}
