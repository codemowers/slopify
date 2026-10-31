package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/alsa"
)

// fakeOutput records what reaches the device, and how often it was opened.
type fakeOutput struct {
	formats *[]format
	f       format
	samples *[]int32
	failAt  int
}

func (o *fakeOutput) Write(s []int32) error {
	if o.failAt > 0 && len(*o.samples)+len(s) >= o.failAt {
		return errors.New("device gone")
	}
	*o.samples = append(*o.samples, s...)
	return nil
}
func (o *fakeOutput) Delay() int   { return 0 }
func (o *fakeOutput) Drain() error { return nil }
func (o *fakeOutput) Close()       {}
func (o *fakeOutput) Format() localPCM {
	return localPCM{Format: "S32_LE", Rate: o.f.rate, Channels: o.f.channels}
}

func wavFile(rate, words int, base int32) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+words*4))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, []any{uint32(16)}[0])
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*8))
	binary.Write(&b, binary.LittleEndian, uint16(8))
	binary.Write(&b, binary.LittleEndian, uint16(32))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(words*4))
	for i := 0; i < words; i++ {
		binary.Write(&b, binary.LittleEndian, base+int32(i))
	}
	return b.Bytes()
}

func nativeHarness(t *testing.T, files map[uint64][]byte, entries []entry, failAt int) (*nativeBackend, *[]format, *[]int32, chan event) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id uint64
		fmt.Sscanf(r.URL.Path, "/%d", &id)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(files[id]))
	}))
	t.Cleanup(server.Close)
	queue := &queueView{}
	queue.set(entries)
	events := make(chan event, 16)
	formats, samples := &[]format{}, &[]int32{}
	b := &nativeBackend{
		playbackServices: playbackServices{queue: queue, events: events, decodedState: (*localStatus)(nil).decodedState,
			streamURL: func(_ context.Context, id uint64) (string, error) { return fmt.Sprintf("%s/%d", server.URL, id), nil }},
		client: server.Client(), unsupported: map[uint64]bool{},
		open: func(f format) (nativeOutput, error) {
			*formats = append(*formats, f)
			return &fakeOutput{formats: formats, f: f, samples: samples, failAt: failAt}, nil
		},
	}
	return b, formats, samples, events
}

func TestNativeGaplessKeepsDeviceOpen(t *testing.T) {
	files := map[uint64][]byte{1: wavFile(44100, 20000, 0), 2: wavFile(44100, 20000, 1<<20), 3: wavFile(96000, 2000, 1<<24)}
	b, formats, samples, events := nativeHarness(t, files, []entry{{ID: 1}, {ID: 2}, {ID: 3}}, 0)
	s := b.Start(1, 0)
	var ended []uint64
	for len(ended) < 3 {
		select {
		case e := <-events:
			ended = append(ended, e.entry)
			if e.session != s {
				t.Fatal("event from another session")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("ended %v", ended)
		}
	}
	s.close()
	if fmt.Sprint(ended) != "[1 2 3]" {
		t.Fatalf("ended %v", ended)
	}
	if len(*formats) != 2 || (*formats)[0].rate != 44100 || (*formats)[1].rate != 96000 {
		t.Fatalf("device opened %+v; want one reopen for the rate change", *formats)
	}
	if len(*samples) != 42000 {
		t.Fatalf("%d samples", len(*samples))
	}
	for i := 0; i < 20000; i++ { // The boundary is seamless and untouched.
		if (*samples)[i] != int32(i) || (*samples)[20000+i] != 1<<20+int32(i) {
			t.Fatalf("sample %d changed", i)
		}
	}
	if s.failure() {
		t.Fatal("clean playback marked as failed")
	}
}

// The next track must not be announced until it is heard: the controller would
// otherwise restart the track it still believes is current.
func TestNativeTrackChangesWhenHeard(t *testing.T) {
	const delay = 3000
	files := map[uint64][]byte{1: wavFile(1000, 4000, 0), 2: wavFile(1000, 4000, 0)}
	b, _, _, events := nativeHarness(t, files, []entry{{ID: 1}, {ID: 2}}, 0)
	delayed := &delayedOutput{}
	delayed.delay.Store(delay)
	b.open = func(format) (nativeOutput, error) { return delayed, nil }
	s := b.Start(1, 0).(*session)
	defer s.close()
	select {
	case <-events:
		t.Fatal("track ended while its end was still in the device buffer")
	case <-time.After(100 * time.Millisecond):
	}
	if s.current() != 1 {
		t.Fatalf("current %d before the first track was heard out", s.current())
	}
	delayed.delay.Store(0)
}

type delayedOutput struct {
	delay atomicInt
}

type atomicInt = atomic.Int64

func (o *delayedOutput) Write([]int32) error { return nil }
func (o *delayedOutput) Delay() int          { return int(o.delay.Load()) }
func (o *delayedOutput) Drain() error {
	for o.delay.Load() != 0 {
		time.Sleep(time.Millisecond)
	}
	return nil
}
func (o *delayedOutput) Close() {}
func (o *delayedOutput) Format() localPCM {
	return localPCM{Format: "S32_LE", Rate: 1000, Channels: 2}
}

func TestNativeSeekStartsAtPosition(t *testing.T) {
	b, _, samples, events := nativeHarness(t, map[uint64][]byte{1: wavFile(1000, 4000, 0)}, []entry{{ID: 1}}, 0)
	s := b.Start(1, 1)
	<-events
	s.close()
	if len(*samples) != 2000 || (*samples)[0] != 2000 {
		t.Fatalf("%d samples from %d", len(*samples), (*samples)[0])
	}
}

func TestNativeOutputFailureKeepsPosition(t *testing.T) {
	b, _, _, _ := nativeHarness(t, map[uint64][]byte{1: wavFile(44100, 100000, 0)}, []entry{{ID: 1}}, 40000)
	s := b.Start(1, 0)
	select {
	case <-s.(*session).done:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
	if !s.failure() || !s.finished() || s.current() != 1 {
		t.Fatal("failed output was not marked for recovery")
	}
	if s.position() <= 0 {
		t.Fatalf("lost position: %f", s.position())
	}
}

func TestNativeRemembersUndecodableEntry(t *testing.T) {
	b, _, _, _ := nativeHarness(t, map[uint64][]byte{1: []byte("OggS and nothing else useful")}, []entry{{ID: 1, Codec: "flac"}}, 0)
	s := b.Start(1, 0)
	<-s.(*session).done
	if !s.failure() || b.handles(1) {
		t.Fatal("undecodable entry should fail and then route to the fallback")
	}
}

func TestNativeInputShowsCodec(t *testing.T) {
	if got := nativeInputPCM(format{44100, 2, 16}, "FLAC", false); got.Codec != "FLAC" || got.Format != "S16_LE" {
		t.Fatalf("%+v", got)
	}
	if got := nativeInputPCM(format{44100, 2, 24}, "MP3", true); got.Codec != "MP3" || got.Format != "" {
		t.Fatalf("lossy source reported a sample format: %+v", got)
	}
}

func TestNativePeaksAreEvenlySpacedDespiteBurstyWrites(t *testing.T) {
	type window struct {
		peak float64
		at   time.Time
	}
	var got []window
	p := nativePeaks{report: func(v float64, at time.Time) { got = append(got, window{v, at}) }}
	f := format{rate: 5000, channels: 2}
	samples := make([]int32, 2600) // Not a whole number of windows.
	samples[10] = -1 << 31
	start := time.Unix(1000, 0)
	// A burst of writes in one instant, as when a device buffer fills.
	for i := 0; i < 4; i++ {
		// The device's delay grows as each write adds 260 ms to its buffer.
		p.add(samples, f, time.Duration(i+1)*260*time.Millisecond, start)
	}
	if len(got) != 0 {
		t.Fatalf("released %d windows before they were heard", len(got))
	}
	p.release(start.Add(time.Hour))
	if len(got) < 4 || got[0].peak < 0.99 {
		t.Fatalf("%+v", got)
	}
	for i := 1; i < len(got); i++ {
		if d := got[i].at.Sub(got[i-1].at); d != 200*time.Millisecond {
			t.Fatalf("window %d spaced %v", i, d)
		}
	}
	// A real interruption moves the timeline rather than hiding the gap.
	before := got[len(got)-1].at
	p.add(samples, f, 0, start.Add(time.Minute))
	p.release(start.Add(time.Hour))
	if d := got[len(got)-1].at.Sub(before); d < 30*time.Second {
		t.Fatalf("gap hidden: %v", d)
	}
}

// An empty device accepts writes instantly. Reading a live stream at that speed
// would underflow it, and a PipeWire tunnel then replays its buffer.
func TestFreshDeviceIsFilledAtRealTime(t *testing.T) {
	var fill nativeFillPacer
	var next time.Time
	start := time.Now()
	for range 25 { // 500 ms of audio in 20 ms writes.
		next = fill.wait(next, 960, 48000, 0, time.Now())
	}
	if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
		t.Fatalf("500 ms of audio consumed in %v", elapsed)
	}
	// Later, a device running short may catch up instead of waiting.
	long := nativeFillPacer{started: time.Now().Add(-time.Minute)}
	before := time.Now()
	long.wait(time.Time{}, 48000, 48000, 0, time.Now())
	if time.Since(before) > 100*time.Millisecond {
		t.Fatal("a running device short of audio was held back")
	}
}

func TestQueueFillReportsDecodedAudio(t *testing.T) {
	b := &nativeBackend{}
	if q, c := b.queueFill(); q != 0 || c != 0 {
		t.Fatalf("no session: %d/%d", q, c)
	}
	b.samplesPerSec.Store(88200)
	b.queuedSamples.Store(44100)
	if q, c := b.queueFill(); q != 500 || c != nativeChunks*nativeChunkSamples*1000/88200 {
		t.Fatalf("%d/%d", q, c)
	}
}

func TestNativeRoutesByCodec(t *testing.T) {
	for _, tc := range []struct {
		e    entry
		want bool
	}{{entry{Codec: "flac"}, true}, {entry{Codec: "mp3"}, true}, {entry{Codec: "pcm_s24le"}, true}, {entry{Codec: "aac"}, false}, {entry{Codec: "flac", VideoCodec: "mjpeg"}, true}} {
		if nativeCodec(tc.e) != tc.want {
			t.Fatalf("%+v", tc.e)
		}
	}
}

func TestNativeFormatsNeverReducePrecision(t *testing.T) {
	if fmt.Sprint(nativeFormats(32)) != fmt.Sprint([]int{alsa.FormatS32LE}) {
		t.Fatal("32-bit source must not narrow")
	}
	if got := nativeFormats(24); len(got) != 3 {
		t.Fatalf("%v", got)
	}
	if got := packSamples(nil, []int32{0x12345600}, alsa.FormatS243LE); !bytes.Equal(got, []byte{0x56, 0x34, 0x12}) {
		t.Fatalf("%x", got)
	}
	if got := packSamples(nil, []int32{0x12345600}, alsa.FormatS24LE); !bytes.Equal(got, []byte{0x56, 0x34, 0x12, 0}) {
		t.Fatalf("%x", got)
	}
}

func TestNewNativeBackendRefusesPipeWire(t *testing.T) {
	if _, err := newNativeBackend(playbackServices{}, "pipewire:x", nil); err == nil {
		t.Fatal("PipeWire is not implemented by the native engine")
	}
}

func TestPackSamplesS32IsLittleEndian(t *testing.T) {
	got := packSamples(nil, []int32{0x01020304, -2}, alsa.FormatS32LE)
	want := []byte{4, 3, 2, 1, 0xfe, 0xff, 0xff, 0xff}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

func BenchmarkPackSamplesS32(b *testing.B) {
	samples := make([]int32, 3840) // 20 ms at 96 kHz stereo.
	dst := make([]byte, 0, len(samples)*4)
	b.SetBytes(int64(len(samples) * 4))
	for b.Loop() {
		dst = packSamples(dst[:0], samples, alsa.FormatS32LE)
	}
}

func TestPeakTimelineFollowsClockSteps(t *testing.T) {
	var got []time.Time
	p := nativePeaks{report: func(_ float64, at time.Time) { got = append(got, at) }}
	f := format{rate: 48000, channels: 2}
	samples := make([]int32, 2*48000/5)
	start := time.Now() // carries a monotonic reading, as real calls do
	p.add(samples, f, 0, start)
	for _, step := range []time.Duration{97 * time.Second, -time.Hour} {
		got = nil
		now := start.Add(step)
		p.add(samples, f, 0, now)
		if len(got) != 1 || got[0].Sub(now.Round(0)).Abs() > nativePeakResync {
			t.Fatalf("step %v: stamps %v, clock %v", step, got, now)
		}
	}
}
