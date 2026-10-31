package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// format is a stream's sample rate, channel count and bit depth. Audio is never
// resampled: the backend validates that its output preserves this format.
type format struct{ rate, channels, bits int }

// pcmName is the sample format's ALSA-style name, as the local UI renders it.
func (f format) pcmName() string { return fmt.Sprintf("S%d_LE", f.bits) }

// queueView gives the decoder the latest queue order for gapless playback.
type queueView struct {
	mu      sync.Mutex
	ids     []uint64
	entries map[uint64]entry
}

func (q *queueView) set(entries []entry) {
	ids := make([]uint64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	q.mu.Lock()
	q.ids = ids
	q.entries = map[uint64]entry{}
	for _, entry := range entries {
		q.entries[entry.ID] = entry
	}
	q.mu.Unlock()
}

func (q *queueView) entry(id uint64) entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.entries[id]
}

func (q *queueView) contains(id uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, entry := range q.ids {
		if entry == id {
			return true
		}
	}
	return false
}

func (q *queueView) after(id uint64) (uint64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.ids {
		if x == id && i+1 < len(q.ids) {
			return q.ids[i+1], true
		}
	}
	return 0, false
}

// session plays one track and the ones after it, gaplessly.
type session struct {
	failed    atomic.Bool
	startedAt time.Time
	cancel    context.CancelFunc
	done      chan struct{}
	first     uint64
	cur       atomic.Uint64
	pos       atomic.Uint64 // float64 bits: seconds into the current track
}

func (s *session) current() uint64 {
	if id := s.cur.Load(); id != 0 {
		return id
	}
	return s.first
}
func (s *session) position() float64 { return math.Float64frombits(s.pos.Load()) }
func (s *session) close() {
	s.cancel()
	<-s.done
}
func (s *session) finished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
