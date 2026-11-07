package voicepcm

import (
	"bytes"
	"testing"
)

func TestJitterOrderingDuplicatesAndLatePackets(t *testing.T) {
	var j Jitter
	a, b, c := bytes.Repeat([]byte{0xce}, 160), bytes.Repeat([]byte{0x4e}, 160), bytes.Repeat([]byte{0xd5}, 160)
	j.Push(1000, a)
	j.Push(1320, c)
	j.Push(1160, b)
	j.Push(1000, a)
	for _, want := range [][]byte{a, b, c} {
		got, ok := j.Pop()
		if !ok || !bytes.Equal(got, want) {
			t.Fatal("reordered audio mismatch")
		}
	}
	j.Push(1000, a)
	queued, dropped, _ := j.Stats()
	if queued != 0 || dropped != 320 {
		t.Fatalf("duplicates/late samples not discarded: %d %d", queued, dropped)
	}
}
func TestJitterRTPWrapAndSilenceForLoss(t *testing.T) {
	var j Jitter
	a := bytes.Repeat([]byte{0xce}, 160)
	b := bytes.Repeat([]byte{0x4e}, 160)
	start := uint32(0xffffffa0)
	j.Push(start, a)
	j.Push(start+160, b)
	x, _ := j.Pop()
	y, _ := j.Pop()
	z, _ := j.Pop()
	if !bytes.Equal(x, a) || !bytes.Equal(y, b) || !bytes.Equal(z, bytes.Repeat([]byte{0xff}, 160)) {
		t.Fatal("wrap or packet-loss silence failed")
	}
	_, _, missing := j.Stats()
	if missing != 160 {
		t.Fatal("missing sample count")
	}
}
func TestJitterWindowIsBounded(t *testing.T) {
	var j Jitter
	j.Push(0, []byte{1})
	for stamp := uint32(0); stamp < 80000; stamp += 160 {
		j.Push(stamp, bytes.Repeat([]byte{3}, 160))
	}
	queued, dropped, _ := j.Stats()
	if queued > windowSamples || dropped == 0 {
		t.Fatal("unbounded audio latency")
	}
	j.Push(0, make([]byte, 1000000))
	q, _, _ := j.Stats()
	if q != queued {
		t.Fatal("oversized packet retained")
	}
}
