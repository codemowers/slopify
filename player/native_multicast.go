package main

import (
	"context"
	"time"

	"github.com/codemowers/slopify/player/internal/rtp"
)

// multicastFeed sends on a real-time timeline and holds the local copy back.
// This is additional delay before the device, not an ALSA capacity request.
// The separate queue is bounded to 256 PCM blocks (16 MiB); it normally holds
// just the configured delay plus a block. Backpressure never waits on UDP IO.
func (b *nativeBackend) multicastFeed(ctx context.Context, input <-chan nativeChunk, pending []nativeChunk) (<-chan nativeChunk, func()) {
	ctx, cancel := context.WithCancel(ctx)
	output := make(chan nativeChunk, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(output)
		var next time.Time
		var f format
		var track rtp.Track
		active := false
		for {
			var c nativeChunk
			var ok bool
			if len(pending) > 0 {
				c, pending = pending[0], pending[1:]
			} else {
				select {
				case c, ok = <-input:
					if !ok {
						return
					}
				case <-ctx.Done():
					return
				}
			}
			c.mirrored = true
			if c.begin != nil {
				f = *c.begin
				e := b.queue.entry(c.entry)
				track = rtp.Track{Title: e.Title, Artist: e.Artist, Album: e.Album}
				// Re-anchor at each track without moving a continuous boundary
				// backwards or discarding samples from the local copy.
				next = laterMulticastTime(next, time.Now())
				b.sender.Begin(rtp.Format{Rate: f.rate, Channels: f.channels, Bits: f.bits}, track)
				active = b.sender.Enabled()
			}
			if len(c.samples) > 0 && f.rate > 0 && f.channels > 0 {
				enabled := b.sender.Enabled()
				if enabled {
					now := time.Now()
					if !active || next.IsZero() || now.Sub(next) > 100*time.Millisecond {
						// Resume after silence, source changes or decoder stalls.
						// Begin marks the receiver's opportunity to trim drift.
						next = now
						b.sender.Begin(rtp.Format{Rate: f.rate, Channels: f.channels, Bits: f.bits}, track)
					}
					if !waitMulticastTime(ctx, next) {
						return
					}
					b.sender.WriteAt(c.samples, next)
					// Short local writes keep small device buffers fed without
					// pooling slices that still belong to another queued block.
					b.queuedSamples.Add(-int64(len(c.samples)))
					frames := len(c.samples) / f.channels
					for at := 0; at < frames; {
						n := min(max(f.rate/100, 1), frames-at)
						part := c
						part.localCopy = true
						part.samples = append([]int32(nil), c.samples[at*f.channels:(at+n)*f.channels]...)
						part.localAt = next.Add(b.multicastLocalDelay + time.Duration(at)*time.Second/time.Duration(f.rate))
						if at+n < frames {
							part.input = nil
						}
						select {
						case output <- part:
						case <-ctx.Done():
							return
						}
						at += n
					}
					next = next.Add(time.Duration(frames) * time.Second / time.Duration(f.rate))
					buffer := c.samples[:cap(c.samples)]
					nativeBuffers.Put(&buffer)
					active = true
					continue
				} else {
					next = time.Time{}
				}
				active = enabled
				if !enabled {
					c.feedAck = make(chan struct{})
				}
			}
			select {
			case output <- c:
			case <-ctx.Done():
				return
			}
			if c.feedAck != nil {
				select {
				case <-c.feedAck:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return output, func() { cancel(); <-done }
}

func laterMulticastTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func waitMulticastTime(ctx context.Context, at time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	if wait := time.Until(at); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return ctx.Err() == nil
}
