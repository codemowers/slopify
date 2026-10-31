package pulse

import (
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultTargetMicros = 250_000
	maxTargetMicros     = 2_000_000
	unset               = 0xffffffff
)

type bufferAttr struct {
	maxLength, tLength, prebuf, minReq uint32
}

// Stream is one sender's playback stream. Bytes are unchanged; the consumer owns
// any widening, because it alone knows the output's format.
type Stream struct {
	client *client
	ID     uint32
	Input  uint32 // Sink-input index, unique per connection.

	Format, Channels, Rate int
	Peer                   string

	mu        sync.Mutex
	props     map[string]string
	attr      bufferAttr
	ring      []byte
	head, len int
	written   int64
	readIndex int64
	missing   uint32
	pending   uint32
	corked    bool
	prebuf    bool
	started   bool
	underrun  bool
	drainTag  *uint32
	closed    bool
	done      chan struct{}

	channelMap []byte
	volume     uint32
	deviceUsec uint64
	clipped    atomic.Uint64
	underflows uint64
	overflows  uint64
	onReceive  func(gap time.Duration)
	lastBlock  time.Time
}

// Volume returns the stream's volume, where 0x10000 is 100%.
func (s *Stream) Volume() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volume
}

func (s *Stream) storeVolume(volume uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = volume
}

// SetVolume changes what the sender sees as this stream's volume. It moves the
// sender's slider only: samples here are never scaled.
func (s *Stream) SetVolume(volume uint32) {
	s.mu.Lock()
	changed := s.volume != volume
	s.volume = volume
	input := s.Input
	s.mu.Unlock()
	if changed {
		s.client.server.notifyInputChange(input)
	}
}

func (s *Stream) inputInfo(w *writer, version uint32) *writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := s.props["media.name"]
	if name == "" {
		name = "Playback"
	}
	w.u32(s.Input).str(name).u32(invalidIndex).u32(s.client.index).u32(sinkIndex)
	w.sampleSpec(uint8(s.Format), uint8(s.Channels), uint32(s.Rate)).channelMap(s.channelMap)
	w.cvolume(s.Channels, s.volume).usec(s.latencyLocked()).usec(s.deviceUsec)
	w.str("none").str("slopify").boolean(false).proplist(s.props)
	if version >= 19 {
		w.boolean(s.corked)
	}
	if version >= 20 {
		w.boolean(true).boolean(true)
	}
	if version >= 21 {
		w.pcmFormatInfo()
	}
	return w
}

// Stats describes a stream's buffering for display.
type Stats struct {
	BufferedUsec, TargetUsec uint64
	Underflows, Overflows    uint64
	Clipped                  uint64
}

// ObserveReceive reports the time between blocks arriving from the sender.
func (s *Stream) ObserveReceive(observe func(gap time.Duration)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReceive = observe
}

// Stats reports what is queued here and how often the stream ran dry.
// Decode converts raw samples for output, counting float samples that had to clip.
func (s *Stream) Decode(dst []int32, raw []byte) int {
	n := DecodeS32(dst, raw, s.Format)
	if s.Format == FormatFloat32LE {
		for i := range n {
			if float32Clips(binary.LittleEndian.Uint32(raw[i*4:])) {
				s.clipped.Add(1)
			}
		}
	}
	return n
}

func (s *Stream) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame, rate := uint64(s.frameSize()), uint64(s.Rate)
	return Stats{BufferedUsec: uint64(s.len) / frame * 1_000_000 / rate,
		TargetUsec: uint64(s.attr.tLength) / frame * 1_000_000 / rate, Underflows: s.underflows, Overflows: s.overflows, Clipped: s.clipped.Load()}
}

// SetDeviceDelay tells senders how much written audio the output has yet to play.
// They add it to their own latency, which is what keeps video in step with sound.
func (s *Stream) SetDeviceDelay(usec uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceUsec = usec
}

func newStream(c *client, id, input uint32, format, channels, rate int, props map[string]string) *Stream {
	return &Stream{client: c, ID: id, Input: input, volume: volumeNorm, Format: format, Channels: channels, Rate: rate,
		Peer: c.peer, props: props, prebuf: true, done: make(chan struct{})}
}

// Done closes when the sender deletes the stream or disconnects.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Props returns a copy of the stream's descriptive properties. Their values come
// from the sender and are not authentication.
func (s *Stream) Props() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]string, len(s.props))
	for k, v := range s.props {
		result[k] = v
	}
	return result
}

func (s *Stream) setProps(props map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range props {
		s.props[k] = v
	}
}

func (s *Stream) frameSize() int { return s.Channels * BytesPerSample(s.Format) }

func (s *Stream) microsToBytes(us uint32) uint32 {
	return uint32(int64(us) * int64(s.Rate) / 1_000_000 * int64(s.frameSize()))
}

// setAttr applies a client's buffer request within bounds, keeping whole frames.
func (s *Stream) setAttr(maxLength, tLength, prebuf, minReq uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := uint32(s.frameSize())
	target := s.microsToBytes(defaultTargetMicros)
	if tLength != unset && tLength > 0 {
		target = tLength
	}
	target = max(target, s.microsToBytes(uint32(s.client.server.config.MinBufferMillis)*1000))
	target = min(target, s.microsToBytes(maxTargetMicros))
	target = max(target-target%frame, frame)
	limit := min(maxBufferBytes, target*2)
	if maxLength != unset && maxLength > 0 {
		limit = min(maxBufferBytes, max(maxLength, target))
	}
	limit -= limit % frame
	request := target / 4
	if minReq != unset && minReq > 0 {
		request = min(minReq, target)
	}
	request = max(request-request%frame, frame)
	// The start threshold must be reachable: requests come in minReq steps, so a
	// threshold within minReq of the target would wait on data never asked for.
	reachable := target - request + frame
	start := reachable
	if prebuf != unset {
		start = min(prebuf, reachable)
		start -= start % frame
	}
	s.attr = bufferAttr{maxLength: limit, tLength: target, prebuf: start, minReq: request}
	if s.ring == nil || len(s.ring) != int(limit) {
		ring := make([]byte, limit)
		n := min(s.len, len(ring))
		for i := 0; i < n; i++ {
			ring[i] = s.ring[(s.head+i)%len(s.ring)]
		}
		s.ring, s.head, s.len = ring, 0, n
	}
	s.updateRequest()
}

func (s *Stream) attrs() bufferAttr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attr
}

// updateRequest computes how much more the sender may send, with the lock held.
func (s *Stream) updateRequest() {
	wanted := int64(s.attr.tLength) - int64(s.len) - int64(s.pending)
	if wanted > 0 {
		s.missing = uint32(wanted)
	} else {
		s.missing = 0
	}
}

func (s *Stream) takeRequest() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grant()
}

func (s *Stream) grant() uint32 {
	s.updateRequest()
	n := s.missing
	s.pending += n
	s.missing = 0
	return n
}

// receive stores one memblock. A hostile sender cannot grow the fixed ring.
func (s *Stream) receive(r io.Reader, length uint32, offset int64, flags uint32) error {
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	now := time.Now()
	s.mu.Lock()
	observe, last := s.onReceive, s.lastBlock
	s.lastBlock = now
	dropped := 0
	if room := len(s.ring) - s.len; len(buf) > room {
		dropped = len(buf) - room
		buf = buf[:room]
	}
	if len(buf) > 0 {
		at := (s.head + s.len) % len(s.ring)
		n := copy(s.ring[at:], buf)
		copy(s.ring, buf[n:])
		s.len += len(buf)
	}
	s.written += int64(len(buf))
	if s.pending >= length {
		s.pending -= length
	} else {
		s.pending = 0
	}
	id := s.ID
	if dropped > 0 {
		s.overflows++
	}
	s.mu.Unlock()
	if observe != nil && !last.IsZero() {
		observe(now.Sub(last))
	}
	if dropped > 0 {
		return s.client.finish(s.client.message(cmdOverflow, unset).u32(id))
	}
	return nil
}

// Read takes bytes for output, returning fewer than asked while short of data
// or held back by cork or prebuffering. Callers treat a shortfall as silence and
// must not block on it, since the stream's clock is theirs.
func (s *Stream) Read(p []byte) int {
	s.mu.Lock()
	if s.corked || s.closed {
		s.mu.Unlock()
		return 0
	}
	if s.prebuf {
		if s.len < int(s.attr.prebuf) || s.len == 0 {
			// Keep asking while waiting, or a shortfall could never be filled.
			request := s.grant()
			id := s.ID
			s.mu.Unlock()
			if request > 0 {
				s.client.finish(s.client.message(cmdRequest, unset).u32(id).u32(request))
			}
			return 0
		}
		s.prebuf = false
	}
	frame := s.frameSize()
	n := min(len(p), s.len)
	n -= n % frame
	if n > 0 {
		first := copy(p[:n], s.ring[s.head:])
		copy(p[first:n], s.ring)
		s.head = (s.head + n) % len(s.ring)
	}
	s.len -= n
	s.readIndex += int64(n)
	notifyStart := n > 0 && !s.started
	if notifyStart {
		s.started = true
	}
	underflow := n > 0 && s.len == 0 && s.attr.prebuf > 0 && s.drainTag == nil
	var request uint32
	s.updateRequest()
	if s.missing >= s.attr.minReq {
		request = s.grant()
	}
	id := s.ID
	readIndex := s.readIndex
	var drained *uint32
	if s.len == 0 && s.drainTag != nil {
		drained, s.drainTag = s.drainTag, nil
	}
	if underflow {
		s.prebuf = true
		s.underflows++
	}
	s.mu.Unlock()
	if notifyStart {
		s.client.finish(s.client.message(cmdStarted, unset).u32(id))
	}
	if request > 0 {
		s.client.finish(s.client.message(cmdRequest, unset).u32(id).u32(request))
	}
	if underflow {
		s.client.finish(s.client.message(cmdUnderflow, unset).u32(id).s64(readIndex))
	}
	if drained != nil {
		s.client.finish(s.client.reply(*drained))
	}
	return n
}

// drain replies once everything sent so far has been read.
func (s *Stream) drain(tag uint32) {
	s.mu.Lock()
	if s.len > 0 && !s.closed {
		s.prebuf = false
		s.drainTag = &tag
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.client.finish(s.client.reply(tag))
}

func (s *Stream) flush() {
	s.mu.Lock()
	s.head, s.len = 0, 0
	s.prebuf = true
	s.started = false
	request := s.grant()
	id := s.ID
	s.mu.Unlock()
	if request > 0 {
		s.client.finish(s.client.message(cmdRequest, unset).u32(id).u32(request))
	}
}

func (s *Stream) setPrebuf(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prebuf = on
}

func (s *Stream) setCorked(corked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corked = corked
}

// Corked reports whether the sender has paused this stream.
func (s *Stream) Corked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.corked
}

func (s *Stream) latencyUsec() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latencyLocked()
}

func (s *Stream) latencyLocked() uint64 {
	if s.frameSize() == 0 || s.Rate == 0 {
		return 0
	}
	return uint64(s.len) / uint64(s.frameSize()) * 1_000_000 / uint64(s.Rate)
}

func (s *Stream) latencyReply(w *writer, sec, usec uint32) *writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// No device delay here: a PipeWire tunnel steers this latency plus its own ring
	// to a 200 ms target, so reporting more drains the ring and it then sends silence,
	// heard as steady ticks. That costs senders A/V sync, which is not a priority.
	w.usec(0).usec(0).boolean(s.started && !s.corked)
	w.timeval(sec, usec).timeval(uint32(now.Unix()), uint32(now.Nanosecond()/1000))
	w.s64(s.written).s64(s.readIndex)
	w.u64(0).u64(0)
	return w
}

func (s *Stream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
}
