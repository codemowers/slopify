package pulse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// consumer plays a stream at real-time speed, as an output would.
type consumer struct {
	mu       sync.Mutex
	received map[uint32]*bytes.Buffer
	streams  []*Stream
}

func startServer(t *testing.T) (string, *consumer) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &consumer{received: map[uint32]*bytes.Buffer{}}
	server := NewServer(Config{Name: "slopify.test", Description: "Slopify Test", Rate: 48000,
		OnStream: func(s *Stream) {
			buffer := &bytes.Buffer{}
			c.mu.Lock()
			c.received[s.ID] = buffer
			c.streams = append(c.streams, s)
			c.mu.Unlock()
			go func() {
				frame := s.Channels * BytesPerSample(s.Format)
				chunk := make([]byte, s.Rate/100*frame)
				for {
					select {
					case <-s.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
					n := s.Read(chunk)
					c.mu.Lock()
					buffer.Write(chunk[:n])
					c.mu.Unlock()
				}
			}()
		}})
	go server.Serve(ctx, listener)
	return "tcp:" + listener.Addr().String(), c
}

func requireTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skip(tool + " unavailable")
	}
}

func pulseCommand(ctx context.Context, server, tool string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, tool, args...)
	// A private home: nothing may reach the desktop's own audio server.
	cmd.Env = []string{"PULSE_SERVER=" + server, "HOME=" + os.TempDir(), "PATH=" + os.Getenv("PATH"), "PULSE_NO_SIMD=1"}
	return cmd
}

func TestPactlSeesTheSink(t *testing.T) {
	requireTool(t, "pactl")
	server, _ := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := pulseCommand(ctx, server, "pactl", "info").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, info)
	}
	if !strings.Contains(string(info), "slopify.test") {
		t.Fatalf("%s", info)
	}
	sinks, err := pulseCommand(ctx, server, "pactl", "list", "sinks").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, sinks)
	}
	for _, want := range []string{"slopify.test", "Slopify Test", "s32le 2ch 48000Hz"} {
		if !strings.Contains(string(sinks), want) {
			t.Fatalf("missing %q in %s", want, sinks)
		}
	}
}

// Real client, real wire: whatever pacat sends must arrive byte for byte.
func TestPacatBytesArriveUnchanged(t *testing.T) {
	requireTool(t, "pacat")
	for _, tc := range []struct {
		format      string
		rate, frame int
		pulseFormat int
	}{
		{"s16le", 44100, 4, FormatS16LE},
		{"s24le", 96000, 6, FormatS24LE},
		{"s32le", 48000, 8, FormatS32LE},
		{"s24-32le", 44100, 8, FormatS2432LE},
	} {
		t.Run(tc.format, func(t *testing.T) {
			server, c := startServer(t)
			sound := make([]byte, tc.rate/2*tc.frame) // Half a second of noise.
			rand.Read(sound)
			path := filepath.Join(t.TempDir(), "sound.raw")
			if err := os.WriteFile(path, sound, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := pulseCommand(ctx, server, "pacat", "--playback", "--raw", "--format="+tc.format,
				fmt.Sprintf("--rate=%d", tc.rate), "--channels=2", "--property=media.name=Probe", path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.streams) != 1 {
				t.Fatalf("%d streams", len(c.streams))
			}
			s := c.streams[0]
			if s.Format != tc.pulseFormat || s.Rate != tc.rate || s.Channels != 2 || s.Peer != "127.0.0.1" {
				t.Fatalf("%+v", s)
			}
			got := c.received[s.ID].Bytes()
			if !bytes.Equal(got, sound) {
				t.Fatalf("received %d bytes, sent %d; equal prefix %d", len(got), len(sound), commonPrefix(got, sound))
			}
			if s.Props()["media.name"] != "Probe" {
				t.Fatalf("%v", s.Props())
			}
			select {
			case <-s.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("stream not closed after pacat exit")
			}
		})
	}
}

func commonPrefix(a, b []byte) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// Senders that stream continuously must be paced by the consumer, not flooded.
func TestFlowControlFollowsTheConsumer(t *testing.T) {
	requireTool(t, "pacat")
	server, c := startServer(t)
	path := filepath.Join(t.TempDir(), "long.raw")
	if err := os.WriteFile(path, make([]byte, 48000*4*60), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := pulseCommand(ctx, server, "pacat", "--playback", "--raw", "--format=s16le", "--rate=48000", "--channels=2", path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	time.Sleep(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.streams) != 1 {
		t.Fatalf("%d streams", len(c.streams))
	}
	s := c.streams[0]
	s.mu.Lock()
	written, read := s.written, s.readIndex
	s.mu.Unlock()
	const bytesPerSecond = 48000 * 4
	if written > 3*bytesPerSecond+int64(s.attrs().maxLength) {
		t.Fatalf("sender ran ahead: wrote %d in 2 s", written)
	}
	if read < bytesPerSecond {
		t.Fatalf("only %d bytes consumed in 2 s", read)
	}
}

// A stream that underflows must be able to refill and resume, whatever the
// sender's request sizes: this deadlock stalled a real laptop tunnel.
func TestUnderflowCanAlwaysResume(t *testing.T) {
	for _, tc := range []struct{ tLength, prebuf, minReq uint32 }{
		{unset, unset, unset},
		{9600, 9600, 2400},
		{9600, 9000, 4800},
		{9600, 0, 960},
	} {
		c := &client{server: NewServer(Config{Name: "x"}), conn: discardConn{}, streams: map[uint32]*Stream{}, props: map[string]string{}}
		s := newStream(c, 0, 0, FormatS32LE, 2, 48000, map[string]string{})
		s.setAttr(unset, tc.tLength, tc.prebuf, tc.minReq)
		attr := s.attrs()
		if attr.prebuf > attr.tLength-attr.minReq+8 {
			t.Fatalf("%+v: threshold %d is unreachable", tc, attr.prebuf)
		}
		// A sender that sends exactly what it is granted, over many underflows.
		out := make([]byte, attr.tLength)
		granted := s.takeRequest()
		for round := 0; round < 200; round++ {
			if granted > 0 {
				s.receive(bytes.NewReader(make([]byte, granted)), granted, 0, 0)
				granted = 0
			}
			got := s.Read(out)
			s.mu.Lock()
			granted = s.pending
			pendingLen := s.len
			s.pending = granted
			s.mu.Unlock()
			if got == 0 && granted == 0 && pendingLen < int(attr.prebuf) {
				t.Fatalf("%+v: stalled in round %d with %d buffered and nothing requested", tc, round, pendingLen)
			}
		}
		if s.readIndex == 0 {
			t.Fatalf("%+v: nothing was ever read", tc)
		}
	}
}

// A PipeWire tunnel steers reported latency to its target: device delay would starve it.
func TestLatencyReplyLeavesOutDeviceDelay(t *testing.T) {
	c := &client{server: NewServer(Config{Name: "x"}), conn: discardConn{}, streams: map[uint32]*Stream{}, props: map[string]string{}}
	s := newStream(c, 0, 0, FormatS32LE, 2, 48000, map[string]string{})
	s.setAttr(unset, unset, unset, unset)
	s.SetDeviceDelay(180_000)
	r := &reader{b: s.latencyReply(&writer{}, 1, 2).b}
	if got := r.usec(); r.err != nil || got != 0 {
		t.Fatalf("sink latency %d, %v", got, r.err)
	}
}

// The real client sees the stream and its volume, and hears about a change.
func TestSetVolumeReachesSubscribers(t *testing.T) {
	requireTool(t, "pacat")
	requireTool(t, "pactl")
	server, c := startServer(t)
	path := filepath.Join(t.TempDir(), "long.raw")
	if err := os.WriteFile(path, make([]byte, 48000*4*30), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := pulseCommand(ctx, server, "pacat", "--playback", "--raw", "--format=s16le", "--rate=48000", "--channels=2", path)
	if err := sender.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); sender.Wait() }()
	var stream *Stream
	for deadline := time.Now().Add(5 * time.Second); stream == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		c.mu.Lock()
		if len(c.streams) > 0 {
			stream = c.streams[0]
		}
		c.mu.Unlock()
	}
	if stream == nil {
		t.Fatal("no stream")
	}
	subscribe := pulseCommand(ctx, server, "pactl", "subscribe")
	var events bytes.Buffer
	subscribe.Stdout = &events
	if err := subscribe.Start(); err != nil {
		t.Fatal(err)
	}
	defer subscribe.Wait()
	time.Sleep(300 * time.Millisecond)
	stream.SetVolume(0x8000)
	time.Sleep(300 * time.Millisecond)
	list, err := pulseCommand(ctx, server, "pactl", "list", "sink-inputs").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, list)
	}
	if !strings.Contains(string(list), "50%") {
		t.Fatalf("volume not visible: %s", list)
	}
	subscribe.Process.Kill()
	if !strings.Contains(events.String(), "'change' on sink-input") {
		t.Fatalf("no event: %q", events.String())
	}
	// Setting the same value again must not echo a change.
	stream.SetVolume(0x8000)
}

func TestMalformedControlIsContained(t *testing.T) {
	inputs := [][]byte{{}, {tagU32}, {tagU32, 0, 0, 0, 8, tagU32, 0, 0, 0, 1}, bytes.Repeat([]byte{tagString}, 64)}
	for _, in := range inputs {
		c := &client{server: NewServer(Config{Name: "x"}), conn: discardConn{}, streams: map[uint32]*Stream{}, props: map[string]string{}}
		c.control(in) // Must return an error or reply, and never panic.
	}
}

type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error)      { return len(b), nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

func FuzzControl(f *testing.F) {
	f.Add([]byte{tagU32, 0, 0, 0, 3, tagU32, 0, 0, 0, 1, tagSampleSpec, 3, 2, 0, 0, 0xac, 0x44})
	f.Fuzz(func(t *testing.T, data []byte) {
		c := &client{server: NewServer(Config{Name: "x", OnStream: func(*Stream) {}}), conn: discardConn{}, streams: map[uint32]*Stream{}, props: map[string]string{}, authed: true, version: protocolVersion}
		c.control(data)
	})
}

func TestMinBufferRaisesSmallRequests(t *testing.T) {
	c := &client{server: NewServer(Config{Name: "x", MinBufferMillis: 500}), conn: discardConn{}, streams: map[uint32]*Stream{}, props: map[string]string{}}
	s := newStream(c, 0, 0, FormatS32LE, 2, 48000, map[string]string{})
	s.setAttr(unset, 38400, unset, unset) // 100 ms
	if got, want := s.attrs().tLength, s.microsToBytes(500_000); got != want {
		t.Fatalf("target %d, want %d", got, want)
	}
}

func TestFloat32DecodesWithoutLosingBits(t *testing.T) {
	for _, tc := range []struct {
		v    float32
		want int32
	}{
		{0, 0}, {0.5, 1 << 30}, {-0.5, -(1 << 30)}, {0.25, 1 << 29}, {1, math.MaxInt32}, {-1, math.MinInt32},
		{2, math.MaxInt32}, {-2, math.MinInt32}, {float32(math.Inf(1)), math.MaxInt32}, {float32(math.NaN()), 0},
		{1.0 / (1 << 31), 1}, {1.0 / (1 << 40), 0},
		{0.1, int32(float64(float32(0.1)) * (1 << 31))},
	} {
		if got := float32BitsToS32(math.Float32bits(tc.v)); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.v, got, tc.want)
		}
	}
}

func TestDecodeCountsOnlyFloatBeyondFullScale(t *testing.T) {
	c := &client{server: NewServer(Config{Name: "x"}), conn: discardConn{}, streams: map[uint32]*Stream{}, props: map[string]string{}}
	s := newStream(c, 0, 0, FormatFloat32LE, 2, 48000, map[string]string{})
	s.setAttr(unset, unset, unset, unset)
	samples := []float32{0, 0.5, 1, -1, 1.5, -2, float32(math.Inf(-1)), float32(math.NaN())}
	raw := make([]byte, 0, 4*len(samples))
	for _, v := range samples {
		raw = binary.LittleEndian.AppendUint32(raw, math.Float32bits(v))
	}
	s.Decode(make([]int32, len(samples)), raw)
	if got := s.Stats().Clipped; got != 3 {
		t.Fatalf("clipped %d, want 3", got)
	}
}
