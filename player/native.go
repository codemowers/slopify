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
		input.Format = ""
	}
	return input
}

// nativeBackend decodes FLAC, WAV and MP3 in Go and writes to the device itself.
// Entries it cannot decode go to fallback (mpv) when that is available.
type nativeBackend struct {
	playbackServices
	open     func(format) (nativeOutput, error)
	openLive func(format) (nativeOutput, error)
	client   *http.Client
	fallback playbackBackend

	mu          sync.Mutex
	unsupported map[uint64]bool

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

func newNativeBackend(services playbackServices, output string, fallback playbackBackend) (playbackBackend, error) {
	kind, target, _ := strings.Cut(output, ":")
	b := &nativeBackend{playbackServices: services, fallback: fallback, unsupported: map[uint64]bool{},
		client: &http.Client{Timeout: 20 * time.Second}}
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
		return nil, fmt.Errorf("native playback supports alsa:hw:... and null outputs, not %q", kind)
	}
	return b, nil
}

func (b *nativeBackend) Close() {
	if b.fallback != nil {
		b.fallback.Close()
	}
}

func nativeCodec(e entry) bool {
	// VideoCodec is ignored: audio files often carry cover art as a video stream.
	// Decoders sniff the content, and real video containers go to the fallback.
	switch {
	case e.Codec == "", e.Codec == "flac", e.Codec == "mp3", e.Codec == "pcm_s16le", e.Codec == "pcm_s24le", e.Codec == "pcm_s32le", e.Codec == "pcm_u8":
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
	if !b.handles(id) && b.fallback != nil {
		return b.fallback.Start(id, at)
	}
	b.decodedState(format{}, "loading")
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
		defer b.decodedState(format{}, "idle")
		defer b.samplesPerSec.Store(0)
		b.output(ctx, s, chunks)
	}()
	return s
}

// nativeChunk is decoded audio, a track boundary or a decode failure.
type nativeChunk struct {
	entry   uint64
	begin   *format
	codec   string
	lossy   bool
	start   float64
	end     bool
	failure error
	samples []int32
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
	for first := true; ; first = false {
		if !first && !b.handles(id) {
			return // The controller starts the next entry on the right backend.
		}
		err := b.decodeEntry(ctx, id, at, send)
		at = 0
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			metricDecodeFailures.Inc()
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
		next, ok := b.queue.after(id)
		if !ok {
			return
		}
		id = next
	}
}

func (b *nativeBackend) decodeEntry(ctx context.Context, id uint64, at float64, send func(nativeChunk) bool) error {
	startedAt := time.Now()
	url, err := b.streamURL(ctx, id)
	if err != nil {
		return errors.New("cannot obtain original")
	}
	source := &rangeSource{url: url, client: b.client, blocks: map[int64][]byte{}}
	if _, err := source.block(ctx, 0); err != nil {
		return err
	}
	dec, err := audiodec.Open(&sourceReader{source: source, ctx: ctx})
	if err != nil {
		return err
	}
	if at > 0 {
		// Report where the seek really landed, which for MP3 is an estimate.
		if at, err = dec.Seek(at); err != nil {
			return err
		}
	}
	f := dec.Format()
	metricStartup.Observe(time.Since(startedAt).Seconds())
	slog.Info("native decoding", "entry", id, "rate", f.Rate, "channels", f.Channels, "bits", f.Bits, "at", at)
	begin := format{rate: f.Rate, channels: f.Channels, bits: f.Bits}
	if !send(nativeChunk{entry: id, begin: &begin, codec: f.Codec, lossy: f.Lossy, start: at}) {
		return ctx.Err()
	}
	for {
		buffer := nativeBuffers.Get().(*[]int32)
		n, err := dec.Read(*buffer)
		if n > 0 && !send(nativeChunk{entry: id, samples: (*buffer)[:n]}) {
			return ctx.Err()
		}
		if n == 0 {
			nativeBuffers.Put(buffer)
		}
		if errors.Is(err, io.EOF) {
			if !send(nativeChunk{entry: id, end: true}) {
				return ctx.Err()
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// nativeMark is a change at a frame written to the device, applied once heard so
// that the controller never sees a track before it is audible: a track's end, or
// the start of the next one.
type nativeMark struct {
	frame uint64
	ended uint64 // entry that finished, or zero
	next  uint64 // entry that starts, or zero
	start float64
}

func (b *nativeBackend) output(ctx context.Context, s *session, chunks <-chan nativeChunk) {
	var out nativeOutput
	var current format
	var written, trackStart uint64
	var start float64
	var marks []nativeMark
	defer func() {
		if out != nil {
			out.Close()
		}
	}()
	peaks := nativePeaks{report: b.peak}
	playing := s.first
	fail := func(err error) {
		s.failed.Store(true)
		b.decodedState(format{}, "error")
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
	for c := range chunks {
		if ctx.Err() != nil {
			return
		}
		switch {
		case c.failure != nil:
			fail(c.failure)
			return
		case c.begin != nil:
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
				b.decodedState(current, "running")
			}
			playing = c.entry
			b.samplesPerSec.Store(int64(c.begin.rate * c.begin.channels))
			if b.formats != nil {
				b.formats(c.entry, nativeInputPCM(*c.begin, c.codec, c.lossy), out.Format())
			}
			marks = append(marks, nativeMark{frame: written, next: c.entry, start: c.start})
		case c.end:
			marks = append(marks, nativeMark{frame: written, ended: c.entry})
		default:
			metricDecodeQueue.Observe(float64(len(chunks)))
			b.queuedSamples.Add(-int64(len(c.samples)))
			writeAt := time.Now()
			if err := out.Write(c.samples); err != nil {
				metricOutputErrors.WithLabelValues("slopify").Inc()
				fail(err)
				return
			}
			metricWriteTime.WithLabelValues("slopify").Observe(millis(time.Since(writeAt)))
			metricDeviceDelay.WithLabelValues("slopify").Observe(float64(out.Delay()) * 1000 / float64(current.rate))
			written += uint64(len(c.samples) / current.channels)
			peaks.add(c.samples, current, time.Duration(out.Delay())*time.Second/time.Duration(current.rate), time.Now())
			buffer := c.samples[:cap(c.samples)]
			nativeBuffers.Put(&buffer)
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

func (b *nativeBackend) applyMark(ctx context.Context, s *session, m nativeMark, trackStart *uint64, start *float64) bool {
	if m.next != 0 {
		s.cur.Store(m.next)
		*trackStart, *start = m.frame, m.start
		s.pos.Store(math.Float64bits(m.start))
	}
	if m.ended != 0 {
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
	report  func(float64, time.Time)
	peak    uint32
	frames  int
	next    time.Time
	pending []nativePeak
}

type nativePeak struct {
	peak float64
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
		if v < 0 {
			v = ^v
		}
		p.peak = max(p.peak, uint32(v))
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
		p.pending = append(p.pending, nativePeak{peak: float64(p.peak) / (1 << 31), at: at})
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

// alsaOutput packs S32 samples for the device without losing precision: it only
// narrows words whose low bits are known to be unused by the source.
type alsaOutput struct {
	pcm       *alsa.PCM
	format    int
	source    int
	duplicate bool
	scratch   []byte
}

func nativeFormats(bits int) []int {
	// S32 first: it carries everything, including decoded MP3 precision.
	formats := []int{alsa.FormatS32LE}
	if bits <= 24 {
		formats = append(formats, alsa.FormatS24LE, alsa.FormatS243LE)
	}
	if bits <= 16 {
		formats = append(formats, alsa.FormatS16LE)
	}
	return formats
}

// liveDeviceBufferMS is the -live-device-buffer-ms flag; 0 means OpenLowLatency.
var liveDeviceBufferMS int

func openALSAOutput(device string, f format, live bool) (nativeOutput, error) {
	var errs []error
	for _, candidate := range nativeFormats(f.bits) {
		for _, channels := range []int{f.channels, 2} {
			if channels != f.channels && (f.channels != 1 || channels != 2) {
				continue
			}
			var pcm *alsa.PCM
			var err error
			if live {
				if ms := liveDeviceBufferMS; ms > 0 {
					us := uint32(ms) * 1000
					pcm, err = alsa.OpenBuffered(device, false, candidate, f.rate, channels, us, us, min(20_000, us/2), min(50_000, us/2))
				} else {
					pcm, err = alsa.OpenLowLatency(device, candidate, f.rate, channels)
				}
			} else {
				pcm, err = alsa.Open(device, false, candidate, f.rate, channels)
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			return &alsaOutput{pcm: pcm, format: candidate, duplicate: channels != f.channels}, nil
		}
	}
	return nil, fmt.Errorf("no lossless device format for %d Hz, %d bits, %d channels: %w", f.rate, f.bits, f.channels, errors.Join(errs...))
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
		metricDeviceUnderruns.Add(float64(underruns))
		slog.Warn("audio underrun", "count", underruns)
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
func (a *alsaOutput) Delay() int        { return a.pcm.Delay() }
func (a *alsaOutput) Drain() error      { return a.pcm.Drain() }
func (a *alsaOutput) Close() {
	a.pcm.Drop()
	a.pcm.Close()
}
