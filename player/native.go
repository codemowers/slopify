package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/audiodec"
	"github.com/codemowers/slopify/player/internal/rtp"
)

const (
	// Roughly six seconds of stereo 44.1 kHz between decoder and device.
	nativeChunkSamples = 16384
	nativeChunks       = 32
)

// nativeOutput receives whole frames of left-justified S32 samples.
type nativeOutput interface {
	Write(samples []int32) error
	// Delay is how many written frames have not been heard yet.
	Delay() int
	Drain() error
	Close()
	// Format is what the device actually receives.
	Format() localPCM
}

// nativeInputPCM describes a source; a lossy one has no sample format of its own.
func nativeInputPCM(f format, codec string, lossy bool) localPCM {
	input := localPCM{Codec: codec, Format: f.pcmName(), Rate: f.rate, Channels: f.channels}
	if lossy {
		input.DecodedBits = f.bits
		input.Format = ""
	}
	return input
}

// nativeBackend decodes FLAC, WAV, MP3 and Vorbis in Go and writes to the device itself.
// Unsupported formats fail visibly; decoding never uses an external process.
type nativeBackend struct {
	playbackServices
	// open gives Slopify originals a deep buffer; openLive is for LAN sources.
	open     func(format) (nativeOutput, error)
	openLive func(format) (nativeOutput, error)
	client   *http.Client
	// sender is the opt-in multicast copy of what this backend decodes.
	sender              *rtp.Sender
	multicastLocalDelay time.Duration
	// readAhead bounds originals downloaded ahead of decoding; nil disables it.
	readAhead *readAheadBudget

	// cues reports whether locally generated sounds may mark a seek or fill a
	// stall; nil allows them.
	cues     func() bool
	pauseCue func(format) error
	startCue func(format) error

	mu          sync.Mutex
	unsupported map[uint64]bool
	last        format // the most recent decoded format, for a seek cue

	// Decoded audio waiting for the device, for display.
	queuedSamples atomic.Int64
	samplesPerSec atomic.Int64
}

// queueFill returns the decoded audio queued for the device, and the most that
// can be, in milliseconds. It is zero before a session knows its format.
func (b *nativeBackend) queueFill() (queued, capacity int) {
	rate := b.samplesPerSec.Load()
	if rate == 0 {
		return 0, 0
	}
	return int(max(b.queuedSamples.Load(), 0) * 1000 / rate), int(nativeChunks * nativeChunkSamples * 1000 / rate)
}

func newNativeBackend(services playbackServices, output string) (*nativeBackend, error) {
	kind, target, _ := strings.Cut(output, ":")
	b := &nativeBackend{playbackServices: services, unsupported: map[uint64]bool{},
		client: &http.Client{Timeout: 20 * time.Second}}
	if readAheadBytes > 0 {
		b.readAhead = newReadAheadBudget(readAheadBytes)
	}
	switch kind {
	case "null":
		b.open = func(f format) (nativeOutput, error) { return &nullOutput{f: f, start: time.Now()}, nil }
		b.openLive = b.open
	case "aplay", "alsa":
		if target == "" {
			target = "hw:0,0"
		}
		if _, _, err := alsa.ParseDevice(target); err != nil {
			return nil, err
		}
		b.open = func(f format) (nativeOutput, error) { return openALSAOutput(target, f, false) }
		b.openLive = func(f format) (nativeOutput, error) { return openALSAOutput(target, f, true) }
	default:
		return nil, fmt.Errorf("playback supports alsa:hw:... and null outputs, not %q", kind)
	}
	return b, nil
}

func nativeCodec(e entry) bool {
	// VideoCodec is ignored: audio files often carry cover art as a video stream.
	// Decoders sniff the content and reject unsupported containers.
	switch {
	case e.Codec == "", e.Codec == "flac", e.Codec == "mp3", e.Codec == "vorbis", e.Codec == "pcm_s16le", e.Codec == "pcm_s24le", e.Codec == "pcm_s32le", e.Codec == "pcm_u8":
		return true
	}
	return false
}

func (b *nativeBackend) handles(id uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.unsupported[id] && nativeCodec(b.queue.entry(id))
}

func (b *nativeBackend) Start(id uint64, at float64) playbackSession {
	return b.start(id, at, false, false)
}

// StartSeek is Start for a seek within the playing entry, announced with a cue.
func (b *nativeBackend) StartSeek(id uint64, at float64) playbackSession {
	return b.start(id, at, true, false)
}

func (b *nativeBackend) StartAnnounced(id uint64, at float64) playbackSession {
	return b.start(id, at, false, true)
}

func (b *nativeBackend) cuesAllowed() bool { return b.cues == nil || b.cues() }

func (b *nativeBackend) start(id uint64, at float64, seek, announce bool) playbackSession {
	b.decodedState("loading")
	b.queuedSamples.Store(0)
	b.samplesPerSec.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{cancel: cancel, done: make(chan struct{}), first: id, startedAt: time.Now()}
	s.pos.Store(math.Float64bits(at))
	chunks := make(chan nativeChunk, nativeChunks)
	go func() {
		defer close(chunks)
		b.decodeQueue(ctx, s, id, at, chunks)
	}()
	go func() {
		defer close(s.done)
		defer cancel()
		defer b.decodedState("idle")
		defer b.samplesPerSec.Store(0)
		b.output(ctx, s, chunks, seek && b.cuesAllowed(), announce)
	}()
	return s
}

// slopifyRateOwner is the session whose decoded rate the input rate metric shows.
var slopifyRateOwner atomic.Pointer[session]

// nativeChunk is decoded audio, a track boundary or a decode failure.
type nativeChunk struct {
	// mirrored chunks were already copied to multicast by the delay feeder.
	mirrored  bool
	localCopy bool
	feedAck   chan struct{}
	localAt   time.Time
	input     *sourceInput
	entry     uint64
	begin     *format
	codec     string
	lossy     bool
	start     float64
	end       bool
	failure   error
	samples   []int32
}

var nativeBuffers = sync.Pool{New: func() any { s := make([]int32, nativeChunkSamples); return &s }}

func (b *nativeBackend) decodeQueue(ctx context.Context, s *session, id uint64, at float64, chunks chan<- nativeChunk) {
	send := func(c nativeChunk) bool {
		select {
		case chunks <- c:
			b.queuedSamples.Add(int64(len(c.samples)))
			return true
		case <-ctx.Done():
			return false
		}
	}
	var prepared *nextSource
	defer func() { prepared.discard() }()
	for first := true; ; first = false {
		if !first && !b.handles(id) {
			return // The controller reports the unsupported next entry separately.
		}
		var source *rangeSource
		if prepared != nil && prepared.id == id {
			select {
			case <-prepared.done:
				source = prepared.source // nil if preparing failed: open it again.
			case <-ctx.Done():
				return
			}
		} else {
			prepared.discard()
		}
		next, err := b.decodeEntry(ctx, id, at, source, send)
		prepared = next
		at = 0
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			reason := errorReason(err, "decode")
			if errors.Is(err, audiodec.ErrUnsupported) {
				reason = "unsupported"
			}
			errorAt(stageInput, "slopify", reason)
			slog.Warn("native decoding stopped", "entry", id, "err", err)
			if errors.Is(err, audiodec.ErrUnsupported) {
				b.mu.Lock()
				b.unsupported[id] = true
				b.mu.Unlock()
				if !first {
					return
				}
			}
			send(nativeChunk{entry: id, failure: err})
			return
		}
		following, ok := b.queue.after(id)
		if !ok {
			return
		}
		id = following
	}
}

// nextSource is the following entry's original, opened and downloading ahead.
type nextSource struct {
	id     uint64
	done   chan struct{}
	source *rangeSource // set before done closes; nil on failure
}

// discard releases a prepared source that will not be played.
func (n *nextSource) discard() {
	if n == nil {
		return
	}
	go func() {
		<-n.done
		if n.source != nil {
			n.source.close()
		}
	}()
}

// prepareAfter starts downloading the entry after id, if this backend plays it.
func (b *nativeBackend) prepareAfter(ctx context.Context, id uint64) *nextSource {
	next, ok := b.queue.after(id)
	if !ok || !b.handles(next) {
		return nil
	}
	n := &nextSource{id: next, done: make(chan struct{})}
	go func() {
		defer close(n.done)
		source, err := b.openSource(ctx, next)
		if err != nil {
			return
		}
		source.startReadAhead(b.readAhead)
		n.source = source
	}()
	return n
}

func (b *nativeBackend) openSource(ctx context.Context, id uint64) (*rangeSource, error) {
	url, err := b.streamURL(ctx, id)
	if err != nil {
		return nil, sourceFailure("stream_url", "cannot obtain original")
	}
	source := newRangeSource(url, b.client)
	if _, err := source.block(ctx, 0); err != nil {
		return nil, err
	}
	return source, nil
}

// decodeEntry decodes one entry, from source if it was prepared ahead. Once its
// original is fully downloaded, it starts downloading the next entry's, which it
// returns, even on failure, for the caller to use or discard.
func (b *nativeBackend) decodeEntry(ctx context.Context, id uint64, at float64, source *rangeSource, send func(nativeChunk) bool) (next *nextSource, err error) {
	startedAt := time.Now()
	if source == nil {
		if source, err = b.openSource(ctx, id); err != nil {
			return nil, err
		}
	}
	defer source.close()
	if b.readAhead != nil {
		prepared := make(chan *nextSource, 1)
		decoded := make(chan struct{})
		var watcher sync.WaitGroup
		watcher.Go(func() {
			select {
			case <-source.complete:
				prepared <- b.prepareAfter(ctx, id)
			case <-decoded:
			case <-ctx.Done():
			}
		})
		defer func() {
			close(decoded)
			watcher.Wait()
			select {
			case next = <-prepared:
			default:
			}
		}()
	}
	path := mediaPath(source.url)
	reader := &sourceReader{source: source, ctx: ctx}
	// undecodable reports a decoder that gave up on the content itself, rather than
	// a fetch failure or cancellation.
	undecodable := func(codec string, at float64, err error) {
		if reader.failed == nil && ctx.Err() == nil && !errors.Is(err, audiodec.ErrUnsupported) {
			reportMediaFailure(id, path, codec, at, err)
		}
	}
	dec, err := audiodec.OpenWith(reader, audiodec.Options{Report: func(i audiodec.Issue) { reportMediaIssue(id, path, i) }})
	if err != nil {
		undecodable(b.queue.entry(id).Codec, 0, err)
		return nil, err
	}
	if at > 0 {
		// Report where the seek really landed, which for MP3 is an estimate.
		target := at
		if at, err = dec.Seek(at); err != nil {
			undecodable(dec.Format().Codec, target, err)
			return nil, err
		}
	}
	// After any seek, so that it starts where decoding does.
	source.startReadAhead(b.readAhead)
	f := dec.Format()
	if metricsEnabled {
		metricStartup.WithLabelValues(audio(stageInput, "slopify")...).Observe(time.Since(startedAt).Seconds())
	}
	slog.Info("native decoding", "entry", id, "rate", f.Rate, "channels", f.Channels, "bits", f.Bits, "at", at)
	begin := format{rate: f.Rate, channels: f.Channels, bits: f.Bits}
	if !send(nativeChunk{entry: id, begin: &begin, codec: f.Codec, lossy: f.Lossy, start: at}) {
		return nil, ctx.Err()
	}
	var decoded int64
	for {
		buffer := nativeBuffers.Get().(*[]int32)
		n, err := readNativeChunk(dec, *buffer)
		decoded += int64(n)
		if n > 0 {
			input := sourceInput{Bytes: reader.bytesRead, Frames: uint64(decoded) / uint64(f.Channels), Rate: f.Rate}
			if measured, ok := dec.(audiodec.EncodedInput); ok {
				input.Bytes, input.Frames = measured.InputTotals()
			}
			if !send(nativeChunk{entry: id, samples: (*buffer)[:n], input: &input}) {
				return nil, ctx.Err()
			}
		}
		if n == 0 {
			nativeBuffers.Put(buffer)
		}
		if errors.Is(err, io.EOF) {
			if !send(nativeChunk{entry: id, end: true}) {
				return nil, ctx.Err()
			}
			return nil, nil
		}
		if err != nil {
			undecodable(f.Codec, at+float64(decoded/int64(f.Channels))/float64(f.Rate), err)
			return nil, err
		}
	}
}

// Decoders may return one codec frame per Read (MP3: usually 1152 frames).
// Fill the PCM block so queue slots represent the advertised sample capacity,
// rather than a codec-dependent number of short reads. Never cross track EOF.
func readNativeChunk(dec interface{ Read([]int32) (int, error) }, dst []int32) (int, error) {
	n := 0
	for n < len(dst) {
		read, err := dec.Read(dst[n:])
		n += read
		if err != nil || read == 0 {
			return n, err
		}
	}
	return n, nil
}

// nativeMark is a change at a frame written to the device, applied once heard so
// that the controller never sees a track before it is audible: a track's end, the
// start of the next one, or a stretch of stall sound that is not part of a track.
type nativeMark struct {
	frame uint64
	ended uint64 // entry that finished, or zero
	next  uint64 // entry that starts, or zero
	start float64
	gap   uint64 // frames of stall sound, which the position does not count
}

// nativeLowWater is how little audio, in frames, a device may hold before it is
// running short: 100 ms, or half its capacity when that is smaller, so that a
// shallow buffer is not always short.
func nativeLowWater(capacity, rate int) int {
	low := rate * 100 / 1000
	if capacity > 0 {
		low = min(low, capacity/2)
	}
	return low
}

// nativeRefillMark is the fill, in frames, below which a live source may read
// ahead of real time: 100 ms, or a smaller device's start threshold, so that a
// device recovering from an underrun refills rather than waiting at its threshold.
func nativeRefillMark(start, rate int) int {
	mark := rate * 100 / 1000
	if start > 0 {
		mark = min(mark, start)
	}
	return mark
}

const (
	// nativeStallChunk is how much stall sound is written at a time, in milliseconds.
	nativeStallChunk = 50
)

func (b *nativeBackend) output(ctx context.Context, s *session, chunks <-chan nativeChunk, seekCue, announce bool) {
	stopFeed := func() {}
	var pending []nativeChunk
	if announce {
		pending = waitStartupAudio(ctx, chunks, b.StartSound, time.Second)
	}
	var out nativeOutput
	var current format
	var written, trackStart uint64
	var start float64
	var marks []nativeMark
	starved := false
	running := false
	musicFrames := 0  // Do not treat startup buffering as a mid-track underrun.
	var stall []int32 // one loop period, for current
	var stallAt int   // next frame of it to play
	defer func() {
		stopFeed()
		if slopifyRateOwner.CompareAndSwap(s, nil) { // Not if a newer session set it.
			if metricsEnabled {
				metricRate.DeleteLabelValues(audio(stageInput, "slopify")...)
			}
		}
		if out != nil {
			out.Close()
		}
		if b.sender != nil {
			b.sender.Flush() // Whatever stopped this session stops the copy too.
		}
	}()
	peaks := nativePeaks{report: b.peak}
	playing := s.first
	fail := func(err error) {
		s.failed.Store(true)
		b.decodedState("error")
		slog.Warn("native playback failed", "entry", playing, "err", err)
	}
	// report advances the heard position and announces finished tracks.
	report := func() bool {
		if out == nil || current.rate == 0 {
			return true
		}
		heard := written - min(written, uint64(out.Delay()))
		for len(marks) > 0 && heard >= marks[0].frame {
			if !b.applyMark(ctx, s, marks[0], &trackStart, &start) {
				return false
			}
			marks = marks[1:]
		}
		if heard >= trackStart {
			s.pos.Store(math.Float64bits(start + float64(heard-trackStart)/float64(current.rate)))
		}
		return true
	}
	// A seek is heard at once, in the last format, rather than after the original opens.
	if seekCue {
		b.mu.Lock()
		f := b.last
		b.mu.Unlock()
		if f.rate == 0 || f.channels == 0 {
			f = format{rate: 48000, channels: 2, bits: 16}
		}
		if opened, err := b.open(f); err != nil {
			slog.Warn("seek sound unavailable", "err", err)
		} else if err = opened.Write(seekSound(f.rate, f.channels)); err != nil {
			opened.Close()
			slog.Warn("seek sound unavailable", "err", err)
		} else {
			out, current = opened, f
			written = uint64(len(seekSound(f.rate, f.channels)) / f.channels)
		}
	}
	if b.sender != nil && b.multicastLocalDelay > 0 {
		chunks, stopFeed = b.multicastFeed(ctx, chunks, pending)
		pending = nil
	}
	// fillStall covers a decoder or network stall with the looping stall sound, so
	// the device neither runs dry nor goes quiet. It is local: not on RTP, not in
	// the peaks that drive source arbitration, and not counted in the position.
	stallRoom := 0 // frames of stall sound one write may add; set with the low-water mark
	fillStall := func() error {
		if len(stall) == 0 {
			stall = stallSound(current.rate, current.channels)
			stallAt = 0
		}
		frames := len(stall) / current.channels
		n := min(current.rate*nativeStallChunk/1000, frames-stallAt, max(stallRoom, current.rate/200))
		if err := out.Write(stall[stallAt*current.channels : (stallAt+n)*current.channels]); err != nil {
			return err
		}
		stallAt = (stallAt + n) % frames
		if last := len(marks) - 1; last >= 0 && marks[last].gap != 0 && marks[last].frame+marks[last].gap == written {
			marks[last].gap += uint64(n)
		} else {
			marks = append(marks, nativeMark{frame: written, gap: uint64(n)})
		}
		written += uint64(n)
		return nil
	}
	// receive waits for the next chunk, filling the device while it waits once
	// playback is under way.
	var stallTimer *time.Timer
	defer func() {
		if stallTimer != nil {
			stallTimer.Stop()
		}
	}()
	receive := func() (nativeChunk, bool, error) {
		if ctx.Err() != nil {
			return nativeChunk{}, false, nil
		}
		if len(pending) > 0 {
			c := pending[0]
			pending = pending[1:]
			return c, true, nil
		}
		select {
		case c, ok := <-chunks:
			return c, ok, nil
		default:
		}
		if !running || out == nil || current.rate == 0 || musicFrames < current.rate || !nativeBuffered(out) || !b.cuesAllowed() {
			select {
			case c, ok := <-chunks:
				return c, ok, nil
			case <-ctx.Done():
				return nativeChunk{}, false, nil
			}
		}
		// Below this much queued audio, with nothing decoded, stall sound covers the gap.
		capacity := out.(interface{ BufferFrames() int }).BufferFrames()
		low := nativeLowWater(capacity, current.rate)
		stallRoom = capacity - low // a chunk bigger than the free space would block
		for {
			if ctx.Err() != nil {
				return nativeChunk{}, false, nil
			}
			delay := out.Delay()
			if delay <= low {
				if err := fillStall(); err != nil {
					return nativeChunk{}, false, err
				}
				if !report() {
					return nativeChunk{}, false, nil
				}
				delay = out.Delay()
			}
			wait := time.Duration(max(delay-low, current.rate/100)) * time.Second / time.Duration(current.rate)
			if stallTimer == nil {
				stallTimer = time.NewTimer(wait)
			} else {
				stallTimer.Reset(wait)
			}
			select {
			case c, ok := <-chunks:
				stallTimer.Stop()
				return c, ok, nil
			case <-stallTimer.C:
			case <-ctx.Done():
				return nativeChunk{}, false, nil
			}
		}
	}
	for {
		c, ok, err := receive()
		if err != nil {
			errorAt(stageOutput, "slopify", "write")
			fail(err)
			return
		}
		if !ok || ctx.Err() != nil {
			break
		}
		if !waitMulticastTime(ctx, c.localAt) {
			break
		}
		switch {
		case c.failure != nil:
			fail(c.failure)
			return
		case c.begin != nil:
			musicFrames = 0
			if out == nil || *c.begin != current {
				if out != nil { // Reopen only for a format change, after playing out.
					if err := out.Drain(); err != nil {
						fail(err)
						return
					}
					out.Close()
					out = nil
					written = 0
					for _, m := range marks {
						if !b.applyMark(ctx, s, m, &trackStart, &start) {
							return
						}
					}
					marks = nil
				}
				opened, err := b.open(*c.begin)
				if err != nil {
					fail(err)
					return
				}
				slog.Info("native output opened", "rate", c.begin.rate, "channels", c.begin.channels, "bits", c.begin.bits)
				out, current = opened, *c.begin
				stall = nil
				b.decodedState("running")
			} else if !running { // Already open in this format for the seek cue.
				b.decodedState("running")
			}
			running = true
			b.mu.Lock()
			b.last = current
			b.mu.Unlock()
			playing = c.entry
			slopifyRateOwner.Store(s)
			if metricsEnabled {
				metricRate.WithLabelValues(audio(stageInput, "slopify")...).Set(float64(c.begin.rate))
			}
			if b.sender != nil && !c.mirrored {
				e := b.queue.entry(c.entry)
				b.sender.Begin(rtp.Format{Rate: c.begin.rate, Channels: c.begin.channels, Bits: c.begin.bits}, rtp.Track{Title: e.Title, Artist: e.Artist, Album: e.Album})
			}
			b.samplesPerSec.Store(int64(c.begin.rate * c.begin.channels))
			if b.formats != nil {
				b.formats(c.entry, nativeInputPCM(*c.begin, c.codec, c.lossy), out.Format())
			}
			marks = append(marks, nativeMark{frame: written, next: c.entry, start: c.start})
		case c.end:
			marks = append(marks, nativeMark{frame: written, ended: c.entry})
		default:
			// An empty queue mid-playback means decoding or the network fell behind.
			if queued, _ := b.queueFill(); len(chunks) == 0 && !starved {
				starved = true
				if metricsEnabled {
					metricUnderruns.WithLabelValues(audio(stageInput, "slopify")...).Inc()
				}
			} else if len(chunks) > 0 {
				starved = false
				if metricsEnabled {
					metricBuffer.WithLabelValues(audio(stageInput, "slopify")...).Observe(float64(queued) / 1000)
				}
			}
			if !c.localCopy {
				b.queuedSamples.Add(-int64(len(c.samples)))
			}
			if b.sender != nil && !c.mirrored {
				b.sender.Write(c.samples)
			}
			writeAt := time.Now()
			if err := out.Write(c.samples); err != nil {
				errorAt(stageOutput, "slopify", "write")
				fail(err)
				return
			}
			if c.feedAck != nil {
				close(c.feedAck)
			}
			if metricsEnabled {
				metricWriteTime.WithLabelValues(audio(stageOutput, "slopify")...).Observe(time.Since(writeAt).Seconds())
				metricBuffer.WithLabelValues(audio(stageOutput, "slopify")...).Observe(float64(out.Delay()) / float64(current.rate))
			}
			musicFrames = min(current.rate, musicFrames+len(c.samples)/current.channels)
			if c.input != nil && b.inputStats != nil {
				b.inputStats(*c.input)
			}
			written += uint64(len(c.samples) / current.channels)
			peaks.add(c.samples, current, time.Duration(out.Delay())*time.Second/time.Duration(current.rate), time.Now())
			if !c.localCopy {
				buffer := c.samples[:cap(c.samples)]
				nativeBuffers.Put(&buffer)
			}
			if !report() {
				return
			}
		}
	}
	if ctx.Err() != nil || out == nil {
		return
	}
	if err := out.Drain(); err != nil {
		fail(err)
		return
	}
	for _, m := range marks {
		if !b.applyMark(ctx, s, m, &trackStart, &start) {
			return
		}
	}
}

// nativeBuffered reports whether out is a device with a buffer that can run dry,
// rather than one that only paces or discards, which stall sound cannot help.
func nativeBuffered(out nativeOutput) bool {
	sized, ok := out.(interface{ BufferFrames() int })
	return ok && sized.BufferFrames() > 0
}

func (b *nativeBackend) applyMark(ctx context.Context, s *session, m nativeMark, trackStart *uint64, start *float64) bool {
	if m.next != 0 {
		s.cur.Store(m.next)
		*trackStart, *start = m.frame, m.start
		s.pos.Store(math.Float64bits(m.start))
	}
	// The track resumes after the stall sound; until then its position holds.
	*trackStart += m.gap
	if m.ended != 0 {
		// Latch heard completion before the controller can receive an older
		// server snapshot while this session advances to the next track.
		s.noteCompleted(m.ended)
		select {
		case b.events <- event{ended: true, entry: m.ended, session: s}:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// nativePeaks summarises audio into 200 ms windows for the speaker page. Windows
// are stamped on the audio's own timeline, evenly, and released once heard, so a
// continuous stream charts continuously however bursty the writes are.
type nativePeaks struct {
	report  func(uint32, time.Time)
	peak    uint32
	frames  int
	next    time.Time
	pending []nativePeak
}

type nativePeak struct {
	peak uint32
	at   time.Time
}

const (
	nativePeakResync   = 300 * time.Millisecond
	nativePeakMaxAhead = time.Minute
)

// add takes samples just written; delay is the time until they will be heard.
func (p *nativePeaks) add(samples []int32, f format, delay time.Duration, now time.Time) {
	if p.report == nil {
		return
	}
	// Wall clock only: the speaker shows these stamps, and a monotonic reading would
	// hide an NTP step from the resync below, leaving every window behind for good.
	now = now.Round(0)
	window := max(f.rate/5, 1)
	windowTime := time.Duration(window) * time.Second / time.Duration(f.rate)
	total := len(samples) / f.channels
	for i, v := range samples {
		magnitude := uint32(v)
		if v < 0 {
			magnitude = -magnitude // Includes MinInt32 without signed overflow.
		}
		p.peak = max(p.peak, magnitude)
		if (i+1)%f.channels != 0 {
			continue
		}
		if p.frames++; p.frames < window {
			continue
		}
		heard := now.Add(delay - time.Duration(total-(i+1)/f.channels)*time.Second/time.Duration(f.rate))
		// Follow the timeline unless playback really was interrupted.
		at := p.next
		if at.IsZero() || at.Sub(heard).Abs() > nativePeakResync {
			at = heard
		}
		p.next = at.Add(windowTime)
		p.pending = append(p.pending, nativePeak{peak: p.peak, at: at})
		p.peak, p.frames = 0, 0
	}
	p.release(now)
}

func (p *nativePeaks) release(now time.Time) {
	n := 0
	// Anything far ahead was stamped before the clock stepped back.
	for n < len(p.pending) && (!p.pending[n].at.After(now) || p.pending[n].at.Sub(now) > nativePeakMaxAhead) {
		p.report(p.pending[n].peak, p.pending[n].at)
		n++
	}
	p.pending = append(p.pending[:0], p.pending[n:]...)
}

// nullOutput discards audio at real-time speed, for testing without hardware.
type nullOutput struct {
	f       format
	start   time.Time
	written int64
}

func (n *nullOutput) Write(samples []int32) error {
	n.written += int64(len(samples) / n.f.channels)
	due := time.Duration(n.written) * time.Second / time.Duration(n.f.rate)
	time.Sleep(time.Until(n.start.Add(due)))
	return nil
}
func (n *nullOutput) Delay() int   { return 0 }
func (n *nullOutput) Drain() error { return nil }
func (n *nullOutput) Close()       {}
func (n *nullOutput) Format() localPCM {
	return localPCM{Format: "S32_LE", Rate: n.f.rate, Channels: n.f.channels}
}

// alsaOutput packs S32 samples for the device. Precision is preserved unless
// the user explicitly permits the 16-bit fallback with -allow-truncate.
type alsaOutput struct {
	pcm       *alsa.PCM
	id        string // source slot writing, for metrics; empty is Slopify
	format    int
	source    int
	duplicate bool
	scratch   []byte
}

func nativeFormats(bits int, allowTruncate bool) []int {
	// S32 first: it carries everything, including decoded MP3 precision.
	formats := []int{alsa.FormatS32LE}
	if bits <= 24 {
		formats = append(formats, alsa.FormatS24LE, alsa.FormatS243LE)
	}
	if bits <= 16 || allowTruncate {
		formats = append(formats, alsa.FormatS16LE)
	}
	return formats
}

// outputBufferMS overrides ALSA output capacity for every source; zero keeps
// the automatic defaults for live audio and originals.
var outputBufferMS int

const outputBufferHelp = "ALSA output buffer for all sources in milliseconds, 40 to 10000; 0 negotiates 40 to 80 for LAN sources and 200 to 500 for originals"

func validOutputBuffer(ms int) bool { return ms == 0 || ms >= 40 && ms <= 10000 }

// allowOutputTruncate is opt-in and only adds a final S16 fallback.
var allowOutputTruncate bool

func openALSAOutput(device string, f format, live bool) (nativeOutput, error) {
	ms := outputBufferMS
	var errs []error
	for _, candidate := range nativeFormats(f.bits, allowOutputTruncate) {
		for _, channels := range []int{f.channels, 2} {
			if channels != f.channels && (f.channels != 1 || channels != 2) {
				continue
			}
			var pcm *alsa.PCM
			var err error
			if ms > 0 {
				// A window, not an exact size: period multiples rarely land on the exact time.
				us := uint32(ms) * 1000
				pcm, err = alsa.OpenBuffered(device, false, candidate, f.rate, channels, us*4/5, us*5/4, min(20_000, us/4), min(50_000, us/2))
			} else if live {
				pcm, err = alsa.OpenLowLatency(device, candidate, f.rate, channels)
			} else {
				pcm, err = alsa.Open(device, false, candidate, f.rate, channels)
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if cfg := pcm.Config(); !cfg.Tuned {
				slog.Warn("audio device rejected the preferred buffer; using its own", "bufferFrames", cfg.BufferFrames, "periodFrames", cfg.PeriodFrames, "rate", f.rate)
			}
			if candidate == alsa.FormatS16LE && f.bits > 16 {
				slog.Warn("truncating audio output", "inputBits", f.bits, "outputBits", 16, "rate", f.rate)
			}
			return &alsaOutput{pcm: pcm, format: candidate, duplicate: channels != f.channels}, nil
		}
	}
	policy := "lossless"
	if allowOutputTruncate {
		policy = "compatible"
	}
	return nil, fmt.Errorf("no %s device format for %d Hz, %d bits, %d channels: %w", policy, f.rate, f.bits, f.channels, errors.Join(errs...))
}

func (a *alsaOutput) Write(samples []int32) error {
	if a.duplicate {
		doubled := make([]int32, 0, len(samples)*2)
		for _, v := range samples {
			doubled = append(doubled, v, v)
		}
		samples = doubled
	}
	data := packSamples(a.scratch[:0], samples, a.format)
	a.scratch = data
	underruns, err := a.pcm.WriteFrames(data)
	if underruns > 0 {
		if metricsEnabled {
			metricUnderruns.WithLabelValues(audio(stageOutput, a.id)...).Add(float64(underruns))
		}
		slog.Warn("audio underrun", "source", a.id, "count", underruns)
	}
	return err
}

func packSamples(dst []byte, samples []int32, format int) []byte {
	for _, v := range samples {
		switch format {
		case alsa.FormatS32LE:
			dst = append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		case alsa.FormatS24LE:
			w := v >> 8
			dst = append(dst, byte(w), byte(w>>8), byte(w>>16), byte(w>>24))
		case alsa.FormatS243LE:
			w := v >> 8
			dst = append(dst, byte(w), byte(w>>8), byte(w>>16))
		case alsa.FormatS16LE:
			w := v >> 16
			dst = append(dst, byte(w), byte(w>>8))
		}
	}
	return dst
}

func (a *alsaOutput) Format() localPCM {
	config := a.pcm.Config()
	name := map[int]string{alsa.FormatS16LE: "S16_LE", alsa.FormatS24LE: "S24_LE", alsa.FormatS243LE: "S24_3LE", alsa.FormatS32LE: "S32_LE"}[a.format]
	return localPCM{Format: name, Rate: config.Rate, Channels: config.Channels}
}

func (a *alsaOutput) BufferFrames() int { return a.pcm.Config().BufferFrames }

// StartFrames is the fill at which a stopped device starts: its buffer less one period.
func (a *alsaOutput) StartFrames() int {
	cfg := a.pcm.Config()
	return cfg.BufferFrames - cfg.PeriodFrames
}
func (a *alsaOutput) Delay() int   { return a.pcm.Delay() }
func (a *alsaOutput) Drain() error { return a.pcm.Drain() }
func (a *alsaOutput) Close() {
	a.pcm.Drop()
	a.pcm.Close()
}
