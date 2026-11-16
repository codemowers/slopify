// Package pcmring is the buffer between a network receiver and a source's output
// loop. The writer blocks while it is full, which holds back a TCP sender.
package pcmring

import (
	"sync"
	"time"
)

type StreamEvent int

const (
	EventCork StreamEvent = iota
	EventUncork
	EventFlush
	EventSeek
	EventRewind
	EventTiming
	StreamEventCount
)

func (e StreamEvent) String() string {
	return [...]string{"cork", "uncork", "flush", "seek", "rewind", "timing"}[e]
}

// Stats describes a live stream's buffering for display and metrics. Each receiver
// fills what it can know; zero means none.
type Stats struct {
	Events                   [StreamEventCount]uint64
	BufferedUsec, TargetUsec uint64
	Underflows               uint64
	Overflows                uint64 // Audio discarded because the sender outran the output.
	Clipped                  uint64
	Lost                     uint64 // Frames that never arrived, played as silence.
}

// Ring holds whole-frame PCM bytes.
type Ring struct {
	frame     int
	threshold int
	onClose   func()
	done      chan struct{}

	mu         sync.Mutex
	space      *sync.Cond
	ring       []byte
	head, size int
	prebuf     bool
	finished   bool
	closed     bool
	underflows uint64
	lastAt     time.Time
	observe    func(time.Duration)
}

// New returns a ring of size bytes that starts playing at threshold bytes, or when
// the writer finishes. onClose runs once, when the ring closes.
func New(size, threshold, frame int, onClose func()) *Ring {
	r := &Ring{frame: frame, threshold: threshold, onClose: onClose, done: make(chan struct{}), ring: make([]byte, size), prebuf: true}
	r.space = sync.NewCond(&r.mu)
	return r
}

// Write stores b and reports false once the ring has closed.
func (r *Ring) Write(b []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.lastAt.IsZero() && r.observe != nil {
		r.observe(now.Sub(r.lastAt))
	}
	r.lastAt = now
	for len(b) > 0 {
		for r.size == len(r.ring) && !r.closed {
			r.space.Wait()
		}
		if r.closed {
			return false
		}
		tail := (r.head + r.size) % len(r.ring)
		n := copy(r.ring[tail:min(len(r.ring), tail+len(r.ring)-r.size)], b)
		r.size += n
		b = b[n:]
	}
	return true
}

// WriteLatest stores b without blocking, for a sender that cannot be held back. When
// b does not fit it discards the oldest audio down to the start threshold, which keeps
// the delay bounded, and reports the bytes discarded. It reports false once closed.
func (r *Ring) WriteLatest(b []byte) (discarded int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, false
	}
	now := time.Now()
	if !r.lastAt.IsZero() && r.observe != nil {
		r.observe(now.Sub(r.lastAt))
	}
	r.lastAt = now
	if len(b) > len(r.ring) {
		discarded = len(b) - len(r.ring)
		b = b[discarded:]
	}
	if r.size+len(b) > len(r.ring) {
		keep := min(max(r.threshold-len(b), 0), len(r.ring)-len(b))
		keep -= keep % r.frame
		discarded += r.size - keep
		r.head = (r.head + r.size - keep) % len(r.ring)
		r.size = keep
	}
	tail := (r.head + r.size) % len(r.ring)
	n := copy(r.ring[tail:], b)
	copy(r.ring, b[n:])
	r.size += len(b)
	return discarded, true
}

// Trim discards the oldest audio beyond keep bytes, returning a drifted stream to its
// intended delay at a moment when a jump is not heard as a fault.
func (r *Ring) Trim(keep int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if drop := r.size - keep; drop > 0 {
		drop -= drop % r.frame
		r.head = (r.head + drop) % len(r.ring)
		r.size -= drop
		r.space.Broadcast()
	}
}

// Clear discards everything held and buffers afresh, for a sender that has jumped.
func (r *Ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head, r.size, r.prebuf = 0, 0, true
	r.space.Broadcast()
}

// Finish marks the end of the writer's data; what is buffered still plays.
func (r *Ring) Finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	if r.size < r.frame {
		r.closeLocked()
	}
}

func (r *Ring) closeLocked() {
	if r.closed {
		return
	}
	r.closed = true
	if r.onClose != nil {
		r.onClose()
	}
	close(r.done)
	r.space.Broadcast()
}

// Close ends the ring and releases a blocked writer.
func (r *Ring) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

// Done is closed once the ring has closed.
func (r *Ring) Done() <-chan struct{} { return r.done }

// Read returns whole frames without blocking, and 0 while buffering.
func (r *Ring) Read(p []byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0
	}
	if r.prebuf {
		if r.size < r.threshold && !r.finished {
			return 0
		}
		r.prebuf = false
	}
	n := min(len(p), r.size)
	n -= n % r.frame
	if n > 0 {
		first := copy(p[:n], r.ring[r.head:])
		copy(p[first:n], r.ring)
		r.head = (r.head + n) % len(r.ring)
		r.size -= n
		r.space.Broadcast()
	}
	switch {
	case r.finished && r.size < r.frame:
		r.closeLocked()
	case n > 0 && r.size == 0:
		r.prebuf = true
		r.underflows++
	}
	return n
}

// Stats returns the bytes held and how often the ring has run empty.
func (r *Ring) Stats() (buffered int, underflows uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size, r.underflows
}

// Observe reports gaps between writes.
func (r *Ring) Observe(observe func(time.Duration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observe = observe
}
