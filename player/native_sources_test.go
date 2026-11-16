package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/codemowers/slopify/player/internal/pcmring"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codemowers/slopify/internal/volume"
	"github.com/codemowers/slopify/player/internal/pulse"
	"github.com/codemowers/slopify/player/internal/rtp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (r *nativeRouter) selectSource(id string) { r.selectSourceWithCue(id, false) }

// routerDevices records every device a router opens and what reaches it.
type routerDevices struct {
	mu     sync.Mutex
	opened []string
	closed int
	writes map[string]int
}

type routerDevice struct {
	devices *routerDevices
	id      string
	f       format
	drained *bool
}

func (d *routerDevice) Write(samples []int32) error {
	d.devices.mu.Lock()
	defer d.devices.mu.Unlock()
	d.devices.writes[d.id] += len(samples)
	return nil
}
func (d *routerDevice) Delay() int { return 7 }
func (d *routerDevice) Drain() error {
	*d.drained = true
	return nil
}
func (d *routerDevice) Close() {
	d.devices.mu.Lock()
	defer d.devices.mu.Unlock()
	d.devices.closed++
}
func (d *routerDevice) Format() localPCM {
	return localPCM{Format: "S32_LE", Rate: d.f.rate, Channels: d.f.channels}
}

func testRouter() (*nativeRouter, *routerDevices, *bool) {
	devices := &routerDevices{writes: map[string]int{}}
	drained := new(bool)
	router := &nativeRouter{open: func(id string, f format) (nativeOutput, error) {
		devices.mu.Lock()
		defer devices.mu.Unlock()
		devices.opened = append(devices.opened, fmt.Sprintf("%s@%d", id, f.rate))
		return &routerDevice{devices: devices, id: id, f: f, drained: drained}, nil
	}}
	return router, devices, drained
}

func TestRouterSendsOnlyTheSelectedSourceToTheDevice(t *testing.T) {
	router, devices, _ := testRouter()
	f := format{rate: 48000, channels: 2, bits: 32}
	router.selectSource("a")
	if err := router.write("b", f, make([]int32, 2)); !errors.Is(err, errNotRouted) {
		t.Fatalf("unselected source: %v", err)
	}
	if len(devices.opened) != 0 {
		t.Fatalf("unselected source opened %v", devices.opened)
	}
	for range 3 {
		if err := router.write("a", f, make([]int32, 2)); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(devices.opened) != "[a@48000]" || devices.writes["a"] != 6 {
		t.Fatalf("%v %v", devices.opened, devices.writes)
	}
	if !router.owns("a") || router.delay("a") != 7 || router.delay("b") != 0 {
		t.Fatal("wrong owner")
	}
}

// Each source gets the device at its own rate; nothing is ever converted.
func TestRouterReopensOnlyForAnotherSourceOrFormat(t *testing.T) {
	router, devices, _ := testRouter()
	a, b := format{rate: 44100, channels: 2, bits: 16}, format{rate: 96000, channels: 2, bits: 24}
	router.selectSource("a")
	router.write("a", a, make([]int32, 2))
	router.write("a", a, make([]int32, 2))
	router.write("a", b, make([]int32, 2))
	router.selectSource("b")
	if devices.closed != 2 {
		t.Fatalf("selection must withdraw the old device at once: %d closed", devices.closed)
	}
	if err := router.write("a", b, make([]int32, 2)); !errors.Is(err, errNotRouted) {
		t.Fatalf("deselected source still routed: %v", err)
	}
	router.write("b", a, make([]int32, 2))
	if fmt.Sprint(devices.opened) != "[a@44100 a@96000 b@44100]" {
		t.Fatalf("%v", devices.opened)
	}
}

func TestRouterReleaseOnlyAffectsTheOwner(t *testing.T) {
	router, devices, drained := testRouter()
	f := format{rate: 48000, channels: 2, bits: 32}
	router.selectSource("a")
	router.write("a", f, make([]int32, 2))
	router.release("b", true)
	if !router.owns("a") || *drained {
		t.Fatal("another source released the device")
	}
	router.release("a", true)
	if router.owns("a") || !*drained || devices.closed != 1 {
		t.Fatalf("owner release: %v %v %d", router.owns("a"), *drained, devices.closed)
	}
	// A later write by the still-selected source reopens it.
	router.write("a", f, make([]int32, 2))
	if len(devices.opened) != 2 {
		t.Fatalf("%v", devices.opened)
	}
}

// A deselected Slopify session keeps real-time pace, without touching the device.
func TestRoutedOutputPacesWhileNotSelected(t *testing.T) {
	router, devices, _ := testRouter()
	router.selectSource("other")
	output := &routedOutput{router: router, id: "slopify", f: format{rate: 48000, channels: 2, bits: 32}}
	start := time.Now()
	for range 10 {
		if err := output.Write(make([]int32, 960*2)); err != nil { // 20 ms each.
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 180*time.Millisecond {
		t.Fatalf("200 ms of audio took %v", elapsed)
	}
	if len(devices.opened) != 0 {
		t.Fatal("deselected source opened the device")
	}
}

func TestSenderKeyStableAndNamedByHost(t *testing.T) {
	key, name, host := senderKey(map[string]string{"application.name": "PipeWire", "application.process.host": "laptop"}, "192.0.2.1")
	if name != "laptop" || host != "laptop" || len(key) != 64 {
		t.Fatalf("%q %q %q", key, name, host)
	}
	moved, _, _ := senderKey(map[string]string{"application.name": "PipeWire", "application.process.host": "laptop"}, "192.0.2.9")
	if moved != key {
		t.Fatal("a sender's card must survive a new DHCP address")
	}
	if _, name, _ := senderKey(map[string]string{}, "192.0.2.1"); name != "Pulse sender" {
		t.Fatalf("%q", name)
	}
}

func TestMetricsAreBoundedAndServed(t *testing.T) {
	metricBuffer.WithLabelValues(stageInput, "pulse", "lan_1").Observe(80)
	metricUnderruns.WithLabelValues(audio(stageOutput, "slopify")...).Add(0)
	metricStreamEvents.WithLabelValues(stageInput, "pulse", "lan_1", "flush").Add(1)
	w := httptest.NewRecorder()
	promhttp.HandlerFor(nativeMetricsRegistry, promhttp.HandlerOpts{}).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{"slopify_audio_buffer_seconds_bucket", `stage="input"`, `transport="pulse"`, `source="lan_1"`, "slopify_audio_underruns_total", "slopify_audio_stream_events_total", `event="flush"`, "go_gc_duration_seconds"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

// Multicast counters share families with the labelled vectors; a mismatch would
// only show at scrape time, as a gather error.
func TestMulticastMetricsShareFamilies(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(metricOverflows, metricErrors, senderCollector{func() rtp.SenderStatus { return rtp.SenderStatus{Packets: 7, Discards: 2} }})
	metricOverflows.WithLabelValues(stageInput, "pulse", "lan_1").Add(1)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, f := range families {
		found[f.GetName()] = len(f.GetMetric())
	}
	if found["slopify_audio_overflows_total"] < 2 || found["slopify_audio_packets_total"] != 1 {
		t.Fatalf("families %v", found)
	}
}

func TestSenderVolumeRoundTripsEveryLevel(t *testing.T) {
	c := volume.Control{Min: 0, Max: 40}
	for level := c.Min; level <= c.Max; level++ {
		if got := senderLevel(senderVolume(level, c), c); got != level {
			t.Fatalf("level %d returned as %d, which would fight the slider", level, got)
		}
	}
	if senderLevel(0x10000, c) != 40 || senderLevel(0, c) != 0 || senderLevel(0x20000, c) != 40 {
		t.Fatal("wrong endpoints or over-100% volume exceeded the cap")
	}
}

// A sender may open a second stream, which takes over its slot. The first stream
// ending must not mark that slot disconnected, since the chart hides such sources.
func TestReplacedStreamLeavesSourceConnected(t *testing.T) {
	source := &speakerSource{ID: "lan_1", Key: "sender", Connected: true}
	lan := &nativeLAN{c: &controller{sources: &speakerSources{Sources: []*speakerSource{source}}},
		router: &nativeRouter{}, consumers: map[string]*nativeConsumer{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	replaced := &nativeConsumer{stream: pulseLive{&pulse.Stream{Format: pulse.FormatS32LE, Rate: 48000, Channels: 2}},
		cancel: cancel, id: "lan_1", key: "sender"}
	current := &nativeConsumer{key: "sender"}
	lan.consumers["sender"] = current
	lan.consume(ctx, replaced)
	if !source.Connected || lan.consumers["sender"] != current {
		t.Fatal("replaced stream took over the slot:", source.Connected)
	}
	lan.consumers["sender"] = replaced
	lan.consume(ctx, replaced)
	if source.Connected || lan.consumers["sender"] != nil {
		t.Fatal("final stream left the slot connected")
	}
}

type fakeLiveStream struct {
	pulseLive
	reads atomic.Int32
	audio bool
	ended chan struct{}
}

func (s *fakeLiveStream) Protocol() string { return "fake" }
func (s *fakeLiveStream) Peer() string     { return "192.0.2.1" }
func (s *fakeLiveStream) Props() map[string]string {
	return map[string]string{"application.name": "fake"}
}
func (s *fakeLiveStream) PCM() livePCM {
	return livePCM{name: "S16_LE", rate: 48000, channels: 2, bytes: 2, bits: 16}
}
func (s *fakeLiveStream) Decode(dst []int32, b []byte) int   { return len(b) / 2 }
func (s *fakeLiveStream) Done() <-chan struct{}              { return s.ended }
func (s *fakeLiveStream) Stats() pcmring.Stats               { return pcmring.Stats{} }
func (s *fakeLiveStream) ObserveReceive(func(time.Duration)) {}
func (s *fakeLiveStream) SetDeviceDelay(uint64)              {}
func (s *fakeLiveStream) Close()                             {}
func (s *fakeLiveStream) Read(p []byte) int {
	s.reads.Add(1)
	if !s.audio {
		return 0
	}
	return len(p)
}

// A stream replacing another must not reach the device while the first still can.
func TestReplacementWaitsForPreviousWriter(t *testing.T) {
	opening, release := make(chan struct{}), make(chan struct{})
	router := &nativeRouter{selected: "lan_1", open: func(string, format) (nativeOutput, error) {
		close(opening)
		<-release
		return nil, errors.New("no device")
	}}
	lan := &nativeLAN{c: &controller{local: &localStatus{}, sources: &speakerSources{}}, router: router,
		consumers: map[string]*nativeConsumer{}}
	first := &fakeLiveStream{audio: true, ended: make(chan struct{})}
	second := &fakeLiveStream{ended: make(chan struct{})}
	defer close(second.ended)
	if !lan.addStream(first) {
		t.Fatal("not admitted")
	}
	<-opening
	if !lan.addStream(second) {
		t.Fatal("not admitted")
	}
	time.Sleep(50 * time.Millisecond)
	if second.reads.Load() != 0 {
		t.Fatal("replacement ran while the first stream was opening the device")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for second.reads.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("replacement never started")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSourceLabelsUsePeerAddressAndStayBounded(t *testing.T) {
	noteSource("lan_91", "pulse", "10.0.0.5", "")
	if got := audio(stageInput, "lan_91"); fmt.Sprint(got) != "[input pulse 10.0.0.5]" {
		t.Fatal(got)
	}
	// One host on two groups stays two sources; the port is not part of identity.
	noteSource("lan_92", "rtp", "10.0.0.6", "239.192.1.1:5004")
	noteSource("lan_93", "rtp", "10.0.0.6", "239.192.1.2")
	if a, b := audio(stageInput, "lan_92")[2], audio(stageInput, "lan_93")[2]; a != "10.0.0.6@239.192.1.1" || b != "10.0.0.6@239.192.1.2" {
		t.Fatal(a, b)
	}
	if got := audio(stageInput, "lan_92")[1]; got != "multicast" {
		t.Fatal(got)
	}
	// A name rather than an address never becomes a label.
	noteSource("lan_94", "upnp", "evil.example\nname", "")
	if got := audio(stageInput, "lan_94")[2]; got != "lan_94" {
		t.Fatal(got)
	}
	// Another sender taking the slot drops the previous one's series.
	metricUnderruns.WithLabelValues(audio(stageInput, "lan_91")...).Inc()
	noteSource("lan_91", "pulse", "10.0.0.7", "")
	if n := metricUnderruns.DeletePartialMatch(prometheus.Labels{"source": "10.0.0.5"}); n != 0 {
		t.Fatal("previous sender's series survived the slot change")
	}
}

type sourceStatsStream struct {
	fakeLiveStream
	stats pcmring.Stats
}

func (s *sourceStatsStream) Stats() pcmring.Stats { return s.stats }
func (s *sourceStatsStream) Protocol() string     { return "pulse" }

func TestSourceDropCountersRemainVisibleWithoutPrometheus(t *testing.T) {
	previous := metricsEnabled
	metricsEnabled = false
	t.Cleanup(func() { metricsEnabled = previous })
	stream := &sourceStatsStream{stats: pcmring.Stats{TargetUsec: 100_000, Underflows: 2, Overflows: 3,
		Events: [pcmring.StreamEventCount]uint64{pcmring.EventFlush: 4}}}
	lan := &nativeLAN{c: &controller{}, router: &nativeRouter{}, consumers: map[string]*nativeConsumer{
		"sender": {stream: stream},
	}}
	buffer := lan.bufferStatus(&speakerSource{ID: "lan_1", Key: "sender"})
	if buffer == nil || buffer.Underflows != 2 || buffer.Overflows != 3 || buffer.Events["flush"] != 4 {
		t.Fatalf("missing source damage counters with Prometheus disabled: %+v", buffer)
	}
}
