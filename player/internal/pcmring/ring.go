// Package pcmring is the buffer between a network receiver and a source's output
// loop. The writer blocks while it is full, which holds back a TCP sender.
package pcmring

import (
	"sync"
	"time"
)

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
