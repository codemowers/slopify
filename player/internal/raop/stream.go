package raop

import (
	"context"
	"encoding/binary"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemowers/slopify/player/internal/audiodec"
	"github.com/codemowers/slopify/player/internal/pcmring"
)

const (
	reorderSlots = 256 // About two seconds of 352-frame packets.
	slotBytes    = 2048
	gapPatience  = 120 * time.Millisecond // How long a retransmission may take.
)

type slot struct {
	full bool
	seq  uint16
	size int
	data []byte
}

// Stream is one session's audio on its way to a source card. Samples are held as
// little-endian left-justified int32, the decoder's output, so that every ALAC depth
// takes one path. Like multicast RTP it cannot hold a sender back: the output's pace
// drains it, and overflow discards the oldest audio since drift cannot be resampled.
type Stream struct {
	Name, Peer           string
	Rate, Channels, Bits int

	ring     *pcmring.Ring
	metadata atomic.Pointer[Metadata]
	control  *remoteControl
	target   int
	frame    int
	lost     atomic.Uint64
	discard  atomic.Uint64

	mu       sync.Mutex
	decoder  *audiodec.ALAC
	cipher   packetCipher
	samples  []int32
	pcm      []byte
	slots    [reorderSlots]slot
	synced   bool
	next     uint16
	highest  uint16
	gapSince time.Time
	resend   func(first, count uint16)
	frames   int
	warned   bool
	played   bool
}

func newStream(decoder *audiodec.ALAC, config audiodec.ALACConfig, c packetCipher, bufferMillis int, onClose func()) *Stream {
	format := decoder.Format()
	frame := format.Channels * 4
	target := max(format.Rate*bufferMillis/1000, 1) * frame
	return &Stream{Rate: format.Rate, Channels: format.Channels, Bits: format.Bits,
		ring: pcmring.New(target*2, target, frame, onClose), target: target, frame: frame,
		decoder: decoder, cipher: c, frames: int(config.FrameLength),
		samples: make([]int32, int(max(config.MaxFrames, config.FrameLength))*format.Channels),
		pcm:     make([]byte, int(max(config.MaxFrames, config.FrameLength))*frame)}
}

// Metadata returns the sender's latest track description.
func (s *Stream) Metadata() Metadata {
	if m := s.metadata.Load(); m != nil {
		return *m
	}
	return Metadata{}
}

// Controllable reports whether the sender accepts remote control.
func (s *Stream) Controllable() bool { return s.control.available() }

// Control asks the sender to play, pause, toggle, skip forward or go back.
func (s *Stream) Control(ctx context.Context, op string) error { return s.control.send(ctx, op) }

// Read returns whole frames without blocking, and 0 while buffering.
func (s *Stream) Read(p []byte) int { return s.ring.Read(p) }

// Done is closed once the stream has ended.
func (s *Stream) Done() <-chan struct{} { return s.ring.Done() }

// Close ends the stream and its session.
func (s *Stream) Close() { s.ring.Close() }

// ObserveReceive reports the time between packets.
func (s *Stream) ObserveReceive(observe func(time.Duration)) { s.ring.Observe(observe) }

// Decode returns the stored samples, which are already in output form.
func (s *Stream) Decode(dst []int32, raw []byte) int {
	n := min(len(dst), len(raw)/4)
	for i := range n {
		dst[i] = int32(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return n
}

// Stats reports what is queued and what was lost.
func (s *Stream) Stats() pcmring.Stats {
	size, underflows := s.ring.Stats()
	rate := uint64(s.Rate) * uint64(s.frame)
	return pcmring.Stats{BufferedUsec: uint64(size) * 1_000_000 / rate, TargetUsec: uint64(s.target) * 1_000_000 / rate,
		Underflows: underflows, Overflows: s.discard.Load(), Lost: s.lost.Load()}
}

func (s *Stream) start(seq uint16, known bool, resend func(first, count uint16)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resend = resend
	s.synced = known
	s.next, s.highest = seq, seq-1
}

// flush drops everything before seq: the sender paused or seeked.
func (s *Stream) flush(seq uint16, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetSlots()
	s.synced = known
	s.next, s.highest = seq, seq-1
	s.gapSince = time.Time{}
	s.ring.Clear()
}

// realign returns a stream that drifted long to its intended delay. A sender's first
// sync after starting playback marks a moment when the jump is not heard as a fault.
func (s *Stream) realign() { s.ring.Trim(s.target) }

func (s *Stream) resetSlots() {
	for i := range s.slots {
		s.slots[i].full = false
	}
}

// receive takes one RTP packet's sequence number and audio. Reordered and retransmitted
// packets are put back in place; contiguous audio goes straight through.
func (s *Stream) receive(seq uint16, payload []byte) {
	if len(payload) > slotBytes*8 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.synced {
		s.synced = true
		s.next, s.highest = seq, seq-1
	}
	offset := int16(seq - s.next)
	if offset < 0 {
		return // Already played or written off.
	}
	if offset >= reorderSlots {
		// The sender jumped further than any gap we could fill. Before anything
		// has played, the jump only means RTP-Info was stale.
		if s.played {
			s.lost.Add(uint64(offset) * uint64(s.frames))
		}
		s.resetSlots()
		s.next, s.highest = seq, seq-1
		s.gapSince = time.Time{}
	}
	entry := &s.slots[seq%reorderSlots]
	if entry.data == nil || len(entry.data) < len(payload) {
		entry.data = make([]byte, max(slotBytes, len(payload)))
	}
	entry.full, entry.seq, entry.size = true, seq, copy(entry.data, payload)
	if ahead := int16(seq - s.highest); ahead > 0 {
		if ahead > 1 && s.resend != nil {
			s.resend(s.highest+1, uint16(ahead-1))
		}
		s.highest = seq
	}
	s.drain()
	if s.next-1 != s.highest && s.gapSince.IsZero() {
		s.gapSince = time.Now()
	}
}

// tick writes off a gap once a retransmission has had its chance.
func (s *Stream) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.gapSince.IsZero() && now.Sub(s.gapSince) >= gapPatience {
		if s.next-1 == s.highest {
			s.gapSince = time.Time{}
			return
		}
		entry := &s.slots[s.next%reorderSlots]
		if entry.full && entry.seq == s.next {
			s.drain()
			continue
		}
		s.writeSilence()
		s.next++
		s.drain()
	}
}

func (s *Stream) drain() {
	for {
		entry := &s.slots[s.next%reorderSlots]
		if !entry.full || entry.seq != s.next {
			return
		}
		entry.full = false
		s.play(entry.data[:entry.size])
		s.next++
		if s.next-1 == s.highest {
			s.gapSince = time.Time{}
		}
	}
}

func (s *Stream) play(payload []byte) {
	if s.cipher != nil {
		s.cipher.decrypt(payload)
	}
	frames, err := s.decoder.Decode(payload, s.samples)
	if err != nil {
		if !s.warned {
			s.warned = true
			slog.Warn("raop packet undecodable; later failures are counted as lost frames", "peer", s.Peer, "bytes", len(payload), "err", err)
		}
		s.writeSilence() // One damaged packet must not end the session.
		return
	}
	s.frames, s.played = frames, true
	count := frames * s.Channels
	for i, v := range s.samples[:count] {
		binary.LittleEndian.PutUint32(s.pcm[i*4:], uint32(v))
	}
	s.write(s.pcm[:count*4])
}

func (s *Stream) writeSilence() {
	s.lost.Add(uint64(s.frames))
	bytes := s.frames * s.frame
	clear(s.pcm[:bytes])
	s.write(s.pcm[:bytes])
}

func (s *Stream) write(b []byte) {
	discarded, _ := s.ring.WriteLatest(b)
	if discarded > 0 {
		s.discard.Add(uint64(discarded))
	}
}
