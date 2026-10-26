package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// format is the source's own sample rate, channel count and bit depth. Audio
// is never resampled or converted: the output device is opened with exactly
// this format, so it reaches the DAC unchanged. Volume is the DAC's own.
type format struct{ rate, channels, bits int }

// container is the bytes per sample sent to ALSA. 24-bit samples travel in
// the low three bytes of 32-bit words (S24_LE), as I2S DACs take them.
func (f format) container() int {
	if f.bits == 16 {
		return 2
	}
	return 4
}

func (f format) frameSize() int { return f.channels * f.container() }

func (f format) alsa() string {
	switch f.bits {
	case 16:
		return "S16_LE"
	case 24:
		return "S24_LE"
	}
	return "S32_LE"
}

// sink plays PCM. Write blocks at the playback rate.
type sink interface {
	Write([]byte) error
	// Latency is how much written audio has not been heard yet.
	Latency() time.Duration
	Close()
}

func sinkFor(spec string) (func(format) (sink, error), error) {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "null":
		return func(f format) (sink, error) { return &nullSink{f: f, start: time.Now()}, nil }, nil
	case "aplay":
		if arg == "" {
			arg = "hw:0,0"
		}
		return func(f format) (sink, error) { return openAplay(arg, f) }, nil
	}
	return nil, fmt.Errorf("unknown output %q: use aplay:<ALSA hw device> or null", spec)
}

// nullSink discards audio at real-time speed, for testing without hardware.
type nullSink struct {
	f      format
	start  time.Time
	frames int64
}

func (n *nullSink) Write(b []byte) error {
	n.frames += int64(len(b) / n.f.frameSize())
	due := n.start.Add(time.Duration(float64(n.frames) / float64(n.f.rate) * float64(time.Second)))
	time.Sleep(time.Until(due))
	return nil
}
func (n *nullSink) Latency() time.Duration { return 0 }
func (n *nullSink) Close()                 {}

// aplaySink feeds ALSA through aplay, which owns the device only while
// something plays, so other sources can use it in between. Use a hw: device:
// "default" and plughw: devices may resample or convert.
type aplaySink struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	conv  conversion
}

const aplayBuffer = 200 * time.Millisecond

func openAplay(device string, f format) (sink, error) {
	c, err := probeAplay(device)
	if err != nil {
		return nil, err
	}
	conv, err := c.fit(f)
	if err != nil {
		return nil, err
	}
	slog.Info("output opened", "device", device, "rate", f.rate, "source", fmt.Sprintf("%d-bit %d ch", f.bits, f.channels),
		"device format", conv.alsa, "device channels", conv.channels)
	cmd := exec.Command("aplay", "-q", "-D", device, "-t", "raw", "-f", conv.alsa,
		"-c", strconv.Itoa(conv.channels), "-r", strconv.Itoa(f.rate),
		"--buffer-time="+strconv.Itoa(int(aplayBuffer/time.Microsecond)))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &aplaySink{cmd: cmd, stdin: stdin, conv: conv}, nil
}

func (a *aplaySink) Write(b []byte) error {
	_, err := a.stdin.Write(a.conv.apply(b))
	return err
}
func (a *aplaySink) Latency() time.Duration { return aplayBuffer }

// Close stops at once, discarding buffered audio, as a pause should.
func (a *aplaySink) Close() {
	a.cmd.Process.Kill()
	a.stdin.Close()
	a.cmd.Wait()
}

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

func (q *queueView) sourceBits(id uint64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.entries[id].Bits
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

// chunk is decoded audio or a track boundary.
type chunk struct {
	entry uint64
	begin *format // a track starts, at offset seconds
	start float64
	end   bool // the track ended
	data  []byte
}

// session plays one track and the ones after it, gaplessly.
type session struct {
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

func (c *controller) startSession(id uint64, at float64) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{cancel: cancel, done: make(chan struct{}), first: id, startedAt: time.Now()}
	s.pos.Store(math.Float64bits(at))
	// About six seconds of CD audio: enough to start the next track's
	// download and decoder before the current one runs out.
	pcm := make(chan chunk, 64)
	go func() {
		defer close(pcm)
		for start := at; ; start = 0 {
			c.decode(ctx, id, start, pcm)
			next, ok := c.queue.after(id)
			if !ok || ctx.Err() != nil {
				return
			}
			id = next
		}
	}()
	go c.output(ctx, s, pcm)
	return s
}

// decode fetches an entry's original file and decodes it locally.
func (c *controller) decode(ctx context.Context, id uint64, start float64, pcm chan<- chunk) {
	send := func(ch chunk) bool {
		select {
		case pcm <- ch:
			return true
		case <-ctx.Done():
			return false
		}
	}
	defer send(chunk{entry: id, end: true})
	u, err := c.streamURL(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("cannot stream entry", "entry", id, "err", err)
		}
		return
	}
	probeStart := time.Now()
	bits := c.queue.sourceBits(id)
	metadataSource := "sidecar"
	if bits != 16 && bits != 24 && bits != 32 {
		metadataSource = "ffprobe"
		slog.Info("probing source", "entry", id)
		bits, err = c.sourceBits(ctx, u)
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("probing failed", "entry", id, "err", err)
		}
		return
	}
	slog.Info("source format", "entry", id, "metadata", metadataSource, "bits", bits, "took", time.Since(probeStart))
	args := []string{"-nostdin", "-loglevel", "error"}
	if start > 0 {
		// ffmpeg seeks within the remote file with HTTP range requests.
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	// No -ar or -ac: the sample rate and channels stay the source's own.
	args = append(args, "-i", u, "-map", "0:a:0", "-vn", "-c:a", fmt.Sprintf("pcm_s%dle", bits), "-f", "wav", "pipe:1")
	cmd := exec.CommandContext(ctx, c.ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	if err := cmd.Start(); err != nil {
		slog.Error("starting ffmpeg", "err", err)
		return
	}
	defer func() {
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			slog.Warn("decoding failed", "entry", id, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		}
	}()
	r := bufio.NewReaderSize(out, 64<<10)
	f, err := readWAVHeader(r, bits)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("decoding failed", "entry", id, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		}
		io.Copy(io.Discard, r)
		return
	}
	if !send(chunk{entry: id, begin: &f, start: start}) {
		return
	}
	wire := f.channels * bits / 8 // 24-bit samples arrive packed in three bytes
	buf := make([]byte, 4096*wire)
	for {
		n, err := io.ReadFull(r, buf)
		n -= n % wire
		if n > 0 {
			data := make([]byte, n/wire*f.frameSize())
			if bits == 24 {
				unpack24(data, buf[:n])
			} else {
				copy(data, buf[:n])
			}
			if !send(chunk{entry: id, data: data}) {
				return
			}
		}
		if err != nil {
			return // end of track, or ffmpeg failed and Wait reports it
		}
	}
}

// unpack24 widens packed 24-bit samples to S24_LE words, sign-extended.
func unpack24(dst, src []byte) {
	for i, j := 0, 0; i+3 <= len(src); i, j = i+3, j+4 {
		dst[j], dst[j+1], dst[j+2] = src[i], src[i+1], src[i+2]
		dst[j+3] = byte(int8(src[i+2]) >> 7)
	}
}

// sourceBits is the bit depth to decode a file to: the source's own for PCM
// codecs such as FLAC, ALAC and WAV, and 24 bits for lossy codecs, whose
// decoders produce floating point and have no bit depth of their own.
func (c *controller) sourceBits(ctx context.Context, u string) (int, error) {
	out, err := exec.CommandContext(ctx, c.ffprobe, "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=sample_fmt,bits_per_raw_sample,bits_per_sample", "-of", "json", u).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return 0, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return 0, err
	}
	var probe struct {
		Streams []struct {
			SampleFmt     string `json:"sample_fmt"`
			BitsPerRaw    string `json:"bits_per_raw_sample"`
			BitsPerSample int    `json:"bits_per_sample"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, err
	}
	if len(probe.Streams) == 0 {
		return 0, errors.New("no audio stream")
	}
	st := probe.Streams[0]
	switch strings.TrimSuffix(st.SampleFmt, "p") {
	case "u8", "s16":
		return 16, nil
	case "s32", "s64":
		raw, _ := strconv.Atoi(st.BitsPerRaw)
		if raw == 0 {
			raw = st.BitsPerSample
		}
		switch {
		case raw > 0 && raw <= 16:
			return 16, nil
		case raw > 0 && raw <= 24:
			return 24, nil
		}
		return 32, nil
	}
	return 24, nil
}

// readWAVHeader reads ffmpeg's streamed WAV header up to the audio data.
func readWAVHeader(r io.Reader, bits int) (format, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return format{}, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return format{}, errors.New("not a WAV stream")
	}
	var f format
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return format{}, err
		}
		size := binary.LittleEndian.Uint32(hdr[4:])
		switch string(hdr[:4]) {
		case "fmt ":
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return format{}, err
			}
			f.channels = int(binary.LittleEndian.Uint16(b[2:]))
			f.rate = int(binary.LittleEndian.Uint32(b[4:]))
			if got := int(binary.LittleEndian.Uint16(b[14:])); got != bits {
				return format{}, fmt.Errorf("decoder produced %d-bit samples, not %d", got, bits)
			}
			f.bits = bits
		case "data":
			if f.rate == 0 || f.channels == 0 {
				return format{}, errors.New("WAV data before format")
			}
			return f, nil
		default:
			if _, err := io.CopyN(io.Discard, r, int64(size+size%2)); err != nil {
				return format{}, err
			}
		}
	}
}

// output writes decoded audio to the sink, reopening it when the format
// changes, and tells the controller when tracks end.
func (c *controller) output(ctx context.Context, s *session, pcm <-chan chunk) {
	var (
		out     sink
		cur     format
		base    float64
		frames  int64
		started bool
	)
	defer func() {
		if out != nil {
			out.Close()
		}
		close(s.done)
	}()
	for {
		var ch chunk
		var ok bool
		select {
		case <-ctx.Done():
			return
		case ch, ok = <-pcm:
			if !ok {
				return
			}
		}
		// A prefetched item may have been removed while the previous one played.
		if !c.queue.contains(ch.entry) {
			continue
		}
		switch {
		case ch.begin != nil:
			if out == nil || *ch.begin != cur {
				if out != nil {
					out.Close()
				}
				var err error
				if out, err = c.open(*ch.begin); err != nil {
					slog.Error("opening output", "err", err)
					return
				}
				cur = *ch.begin
			}
			s.cur.Store(ch.entry)
			base, frames = ch.start, 0
			s.pos.Store(math.Float64bits(base))
		case ch.end:
			select {
			case c.events <- event{ended: true, entry: ch.entry}:
			case <-ctx.Done():
				return
			}
		default:
			if out == nil {
				continue
			}
			if err := out.Write(ch.data); err != nil {
				slog.Error("writing audio", "err", err)
				return
			}
			if !started {
				slog.Info("audio started", "entry", ch.entry, "startup", time.Since(s.startedAt), "rate", cur.rate, "channels", cur.channels, "bits", cur.bits)
				started = true
			}
			frames += int64(len(ch.data) / cur.frameSize())
			pos := base + float64(frames)/float64(cur.rate) - out.Latency().Seconds()
			s.pos.Store(math.Float64bits(max(pos, base)))
		}
	}
}
