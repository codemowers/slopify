package main

import (
	"context"
	"log/slog"
	"time"
)

// cueLevelDB is a peak ceiling in dBFS, configured once at startup.
var cueLevelDB = -6

// cueAmplitude uses fixed-point attenuation, keeping cue synthesis integer-only.
func cueAmplitude(db int) int64 {
	scale := int64(1<<31) - 1
	for i := 0; i < -db; i++ {
		scale = scale * 956973408 >> 30
	}
	return scale
}

// transitionSound synthesizes an original ascending two-note cue using
// integer phase, a rounded triangle wave and attack/release envelopes. It is
// separate from source PCM, so it cannot trigger arbitration or reach RTP.
func transitionSound(rate, channels int) []int32 {
	frames := rate * 220 / 1000
	pcm := make([]int32, frames*channels)
	for note, hz := range []uint64{440, 660} {
		start := note * rate * 110 / 1000
		count := rate * 110 / 1000
		attack := max(1, rate*10/1000)
		release := max(1, rate*65/1000)
		var phase uint32
		step := uint32(hz * (1 << 32) / uint64(rate))
		for i := 0; i < count && start+i < frames; i++ {
			wave := roundedTriangle(phase)
			envelope := min(int64(32768), min(int64(i)*32768/int64(attack), int64(count-1-i)*32768/int64(release)))
			sample := int32(wave * envelope * cueAmplitude(cueLevelDB) / (1 << 30))
			for ch := 0; ch < channels; ch++ {
				pcm[(start+i)*channels+ch] = sample
			}
			phase += step
		}
	}
	return pcm
}
func (r *nativeRouter) playTransitionSound(f format) error {
	if f.rate == 0 || f.channels == 0 {
		f = format{rate: 48000, channels: 2, bits: 16}
	}
	output, err := r.open("transition", f)
	if err != nil {
		return err
	}
	defer output.Close()
	if err = output.Write(transitionSound(f.rate, f.channels)); err != nil {
		return err
	}
	return output.Drain()
}

// roundedTriangle turns a 32-bit phase into a near sine of at most ±2^15, using
// a triangle rounded by a cubic, without floating point.
func roundedTriangle(phase uint32) int64 {
	q := int64(phase >> 16)
	var x int64
	switch {
	case q < 16384:
		x = q * 2
	case q < 49152:
		x = 65536 - q*2
	default:
		x = q*2 - 131072
	}
	return x * (3*(1<<30) - x*x) >> 31
}

// glide writes count frames from start into pcm: a tone sliding linearly from
// fromHz to toHz, with attack/release ramps, optionally fading to silence over
// its length. scale sets the peak amplitude in signed 32-bit PCM.
func glide(pcm []int32, channels, rate, start, count int, fromHz, toHz int64, attackMS, releaseMS int, fade bool, scale int64) {
	attack := max(1, rate*attackMS/1000)
	release := max(1, rate*releaseMS/1000)
	var phase uint32
	for i := 0; i < count; i++ {
		hz := fromHz + (toHz-fromHz)*int64(i)/int64(count)
		envelope := min(int64(32768), min(int64(i)*32768/int64(attack), int64(count-1-i)*32768/int64(release)))
		if fade {
			envelope = envelope * int64(count-i) / int64(count)
		}
		sample := int32(roundedTriangle(phase) * envelope * scale / (1 << 30))
		for ch := 0; ch < channels; ch++ {
			pcm[(start+i)*channels+ch] = sample
		}
		phase += uint32(hz * (1 << 32) / int64(rate))
	}
}

// seekSound is an original, quiet cue for a seek: a quick upward sweep, unlike
// the two stepped notes of the transition sound. Like that one it is local only.
func seekSound(rate, channels int) []int32 {
	frames := rate * 120 / 1000
	pcm := make([]int32, frames*channels)
	glide(pcm, channels, rate, 0, rate*90/1000, 330, 880, 5, 40, false, cueAmplitude(cueLevelDB))
	return pcm
}

// stallSound is one period of a loop that fills a stalled stream: a pitch that
// winds down while dwindling away, then a pause. It starts and ends silent, so
// consecutive periods join without a click.
func stallSound(rate, channels int) []int32 {
	frames := rate * 900 / 1000
	pcm := make([]int32, frames*channels)
	glide(pcm, channels, rate, 0, rate*700/1000, 440, 220, 10, 30, true, cueAmplitude(cueLevelDB-6))
	return pcm
}

// pauseSound is a descending, two-note bebop, with a short gap between notes.
func pauseSound(rate, channels int) []int32 {
	pcm := make([]int32, rate*220/1000*channels)
	glide(pcm, channels, rate, 0, rate*90/1000, 660, 660, 5, 35, false, cueAmplitude(cueLevelDB))
	glide(pcm, channels, rate, rate*110/1000, rate*110/1000, 440, 440, 5, 50, false, cueAmplitude(cueLevelDB))
	return pcm
}

func writePauseSound(open func(format) (nativeOutput, error), f format) error {
	return writeControlSound(open, f, pauseSound)
}

func writeControlSound(open func(format) (nativeOutput, error), f format, sound func(int, int) []int32) error {
	// The cue itself needs only 16 bits, including on a 16-bit-only DAC.
	f.bits = 16
	output, err := open(f)
	if err != nil {
		return err
	}
	defer output.Close()
	if err := output.Write(sound(f.rate, f.channels)); err != nil {
		return err
	}
	return output.Drain()
}

// PauseSound runs only after the playback session has closed its output.
func (b *nativeBackend) PauseSound() {
	if !b.cuesAllowed() {
		return
	}
	b.mu.Lock()
	f := b.last
	b.mu.Unlock()
	if f.rate == 0 || f.channels == 0 {
		return
	}
	play := b.pauseCue
	if play == nil {
		play = func(f format) error { return writePauseSound(b.open, f) }
	}
	if err := play(f); err != nil {
		slog.Warn("pause sound unavailable", "err", err)
	}
}

func (r *nativeRouter) playPauseSound(f format) error {
	return r.playControlSound(f, pauseSound)
}

func (r *nativeRouter) playControlSound(f format, sound func(int, int) []int32) error {
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	r.mu.Lock()
	// A pause caused by selecting another source must not sound over that source.
	if r.selected != "slopify" || r.device != nil {
		r.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	r.transition = done
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.transition = nil; close(done); r.mu.Unlock() }()
	return writeControlSound(func(f format) (nativeOutput, error) { return r.open("transition", f) }, f, sound)
}

func (b *nativeBackend) StartSound() {
	if !b.cuesAllowed() {
		return
	}
	b.mu.Lock()
	f := b.last
	b.mu.Unlock()
	if f.rate == 0 || f.channels == 0 {
		f = format{48000, 2, 16}
	}
	play := b.startCue
	if play == nil {
		play = func(f format) error { return writeControlSound(b.open, f, transitionSound) }
	}
	if err := play(f); err != nil {
		slog.Warn("start sound unavailable", "err", err)
	}
}

// Hold at most the format marker and first audio chunk while startup buffers.
// Cues drain and release the device before the normal output opens it.
func waitStartupAudio(ctx context.Context, chunks <-chan nativeChunk, cue func(), interval time.Duration) []nativeChunk {
	if ctx.Err() != nil {
		return nil
	}
	cue()
	timer := time.NewTicker(interval)
	defer timer.Stop()
	var pending []nativeChunk
	for {
		select {
		case <-ctx.Done():
			return pending
		case c, ok := <-chunks:
			if !ok {
				return pending
			}
			pending = append(pending, c)
			if len(c.samples) > 0 || c.failure != nil || c.end || len(pending) >= 2 {
				return pending
			}
		case <-timer.C:
			if ctx.Err() == nil {
				cue()
			}
		}
	}
}
