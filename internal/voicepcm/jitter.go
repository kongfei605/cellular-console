package voicepcm

import "sync"

const windowSamples = FrameSamples * 8 // Absolute 160ms limit, never unbounded.

// Jitter reorders by RTP sample timestamp, removes duplicates and fills missing
// samples with mu-law silence. The consumer calls Pop at its playback clock.
// int32 timestamp differences remain correct across an RTP uint32 wrap.
type Jitter struct {
	mu      sync.Mutex
	started bool
	next    uint32
	head    int
	data    [windowSamples]byte
	stamp   [windowSamples]uint32
	valid   [windowSamples]bool
	queued  int
	Dropped uint64
	Missing uint64
}

func (j *Jitter) Push(timestamp uint32, payload []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(payload) == 0 || len(payload) > Rate/10 {
		j.Dropped += uint64(len(payload))
		return
	}
	if !j.started {
		j.started = true
		j.next = timestamp
	}
	for i, b := range payload {
		t := timestamp + uint32(i)
		delta := int32(t - j.next)
		if delta < 0 || delta >= windowSamples {
			j.Dropped++
			continue
		}
		index := (j.head + int(delta)) % windowSamples
		if j.valid[index] {
			j.Dropped++
			continue
		}
		j.data[index], j.stamp[index], j.valid[index] = b, t, true
		j.queued++
	}
}

func (j *Jitter) Pop() ([]byte, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.started {
		return nil, false
	}
	frame := make([]byte, FrameSamples)
	for i := range frame {
		t := j.next + uint32(i)
		index := (j.head + i) % windowSamples
		frame[i] = 0xff
		if j.valid[index] && j.stamp[index] == t {
			frame[i] = j.data[index]
			j.valid[index] = false
			j.queued--
		} else {
			j.Missing++
		}
	}
	j.next += FrameSamples
	j.head = (j.head + FrameSamples) % windowSamples
	return frame, true
}

func (j *Jitter) Stats() (queued int, dropped, missing uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.queued, j.Dropped, j.Missing
}
