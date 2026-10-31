package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	"github.com/codemowers/slopify/internal/volume"
	"github.com/codemowers/slopify/player/internal/audiometer"
	"github.com/codemowers/slopify/player/internal/esound"
	"github.com/codemowers/slopify/player/internal/pulse"
	"github.com/codemowers/slopify/player/internal/upnp"
)

const (
	nativeSourcePeriod = 20 * time.Millisecond
	nativeSourceIdle   = 2 * time.Second
)

var errNotRouted = errors.New("source is not selected")

// nativeRouter owns the audio device. Only the selected source reaches it; every
// other source keeps running at its own real-time pace, so it stays metered and
// ready. No samples are mixed: a selection change closes the device and whatever
// it still held, and the next source opens it at that source's native format.
type nativeRouter struct {
	open     func(id string, f format) (nativeOutput, error)
	onDevice func(output *localPCM)

	mu       sync.Mutex
	selected string
	owner    string
	format   format
	device   *nativeDevice
}

// nativeDevice serialises a write in progress against closing the device.
type nativeDevice struct {
	mu     sync.Mutex
	output nativeOutput
}

func (d *nativeDevice) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.output != nil {
		d.output.Close()
		d.output = nil
	}
}

// selectSource closes the device if another source held it.
func (r *nativeRouter) selectSource(id string) {
	r.mu.Lock()
	var old *nativeDevice
	if r.selected != id {
		r.selected = id
		if r.owner != id {
			old, r.device, r.owner = r.device, nil, ""
		}
	}
	r.mu.Unlock()
	if old != nil {
		old.close()
		metricDeviceRate.Set(0)
		r.reportDevice(nil)
	}
}

func (r *nativeRouter) owns(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner == id && r.device != nil
}

func (r *nativeRouter) reportDevice(output *localPCM) {
	if r.onDevice != nil {
		r.onDevice(output)
	}
}

// write sends samples to the device if id is selected, reopening it only when the
// owner or format changes. It returns errNotRouted otherwise.
func (r *nativeRouter) write(id string, f format, samples []int32) error {
	r.mu.Lock()
	if r.selected != id {
		r.mu.Unlock()
		return errNotRouted
	}
	var old *nativeDevice
	if r.device == nil || r.owner != id || r.format != f {
		old, r.device, r.owner = r.device, nil, ""
	}
	device := r.device
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
	if device == nil {
		output, err := r.open(id, f)
		if err != nil {
			return err
		}
		device = &nativeDevice{output: output}
		r.mu.Lock()
		if r.selected != id { // Selection moved while opening.
			r.mu.Unlock()
			device.close()
			return errNotRouted
		}
		r.device, r.owner, r.format = device, id, f
		r.mu.Unlock()
		metricDeviceOpens.WithLabelValues(id).Inc()
		metricDeviceRate.Set(float64(f.rate))
		pcm := output.Format()
		r.reportDevice(&pcm)
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.output == nil {
		return errNotRouted // Closed by a selection change: the audio is withdrawn.
	}
	return device.output.Write(samples)
}

// release closes the device if id holds it, playing out what it wrote if asked.
func (r *nativeRouter) release(id string, drain bool) error {
	r.mu.Lock()
	if r.owner != id || r.device == nil {
		r.mu.Unlock()
		return nil
	}
	device := r.device
	r.device, r.owner = nil, ""
	r.mu.Unlock()
	var err error
	device.mu.Lock()
	if drain && device.output != nil {
		err = device.output.Drain()
	}
	device.mu.Unlock()
	device.close()
	metricDeviceRate.Set(0)
	r.reportDevice(nil)
	return err
}

// deviceFill returns frames queued in the device and its capacity, when id owns it.
func (r *nativeRouter) deviceFill(id string) (queued, capacity int) {
	r.mu.Lock()
	device, owner := r.device, r.owner
	r.mu.Unlock()
	if device == nil || owner != id {
		return 0, 0
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.output == nil {
		return 0, 0
	}
	if sized, ok := device.output.(interface{ BufferFrames() int }); ok {
		capacity = sized.BufferFrames()
	}
	return device.output.Delay(), capacity
}

func (r *nativeRouter) delay(id string) int {
	r.mu.Lock()
	device, owner := r.device, r.owner
	r.mu.Unlock()
	if device == nil || owner != id {
		return 0
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.output == nil {
		return 0
	}
	return device.output.Delay()
}

// routedOutput gives the Slopify session a device that may or may not be its own
// at any moment. While another source is selected it keeps real-time pace.
type routedOutput struct {
	router *nativeRouter
	id     string
	f      format
	next   time.Time
}

func (o *routedOutput) Write(samples []int32) error {
	err := o.router.write(o.id, o.f, samples)
	if err == nil {
		o.next = time.Time{}
		return nil
	}
	if !errors.Is(err, errNotRouted) {
		return err
	}
	o.next = nativePace(o.next, len(samples)/o.f.channels, o.f.rate)
	return nil
}

// nativeFillPacer stops a freshly opened device, whose empty buffer accepts writes
// instantly, from draining a live stream faster than real time. That would cause
// an underflow, and a PipeWire tunnel answers an underflow by rewinding its ring and
// resending, which is heard as the buffer looping. Once the device has been running
// a while its blocking writes pace the stream, on the DAC's clock.
type nativeFillPacer struct{ started time.Time }

const nativeFillTime = 2 * time.Second

func (p *nativeFillPacer) reset() { p.started = time.Time{} }

func (p *nativeFillPacer) wait(next time.Time, frames, rate int, delay time.Duration, now time.Time) time.Time {
	if p.started.IsZero() {
		p.started = now
	}
	// Past the fill, only a device running short may read ahead of the clock, so
	// that clock drift never starves it.
	if now.Sub(p.started) > nativeFillTime && delay < 100*time.Millisecond {
		return time.Time{}
	}
	return nativePace(next, frames, rate)
}

// nativePace sleeps so that discarded audio still takes its real duration.
func nativePace(next time.Time, frames, rate int) time.Time {
	now := time.Now()
	if next.IsZero() || now.After(next.Add(time.Second)) {
		next = now
	}
	next = next.Add(time.Duration(frames) * time.Second / time.Duration(rate))
	time.Sleep(time.Until(next))
	return next
}

func (o *routedOutput) Delay() int   { return o.router.delay(o.id) }
func (o *routedOutput) Drain() error { return o.router.release(o.id, true) }
func (o *routedOutput) Close()       { o.router.release(o.id, false) }
func (o *routedOutput) Format() localPCM {
	return localPCM{Format: "S32_LE", Rate: o.f.rate, Channels: o.f.channels}
}

// startNativeSources runs arbitration for the native engine and, when configured,
// the trusted-LAN Pulse receiver.
func (c *controller) startNativeSources(ctx context.Context, pulseListen, esoundListen string, pulseBufferMS, esoundBufferMS, esoundSocketMS int, upnpListen, sinkName string) error {
	backend, ok := c.backend.(*nativeBackend)
	if !ok {
		return errors.New("native sources require the native backend")
	}
	open, openLive := backend.open, backend.openLive
	router := &nativeRouter{onDevice: c.local.nativeDevice, open: func(id string, f format) (nativeOutput, error) {
		if id == "slopify" {
			return open(f) // Local files can afford a deep buffer.
		}
		return openLive(f)
	}}
	backend.open = func(f format) (nativeOutput, error) { return &routedOutput{router: router, id: "slopify", f: f}, nil }
	lan := &nativeLAN{c: c, router: router, consumers: map[string]*nativeConsumer{}}
	s := c.sources
	s.mu.Lock()
	router.selected = s.Selected
	s.mu.Unlock()
	go lan.arbitrate(ctx)
	if pulseListen != "" {
		listener, err := net.Listen("tcp", pulseListen)
		if err != nil {
			return err
		}
		server := pulse.NewServer(pulse.Config{Name: sinkName, Description: c.name, MinBufferMillis: pulseBufferMS,
			OnStream: func(stream *pulse.Stream) { lan.addStream(pulseLive{stream}) }, OnVolume: lan.setVolume})
		go func() {
			if err := server.Serve(ctx, listener); err != nil {
				slog.Error("pulse receiver", "err", err)
			}
		}()
		slog.Info("pulse receiver listening", "address", pulseListen, "sink", sinkName)
	}
	if esoundListen != "" {
		listener, err := net.Listen("tcp", esoundListen)
		if err != nil {
			return err
		}
		server := esound.NewServer(esound.Config{BufferMillis: esoundBufferMS, SocketMillis: esoundSocketMS, OnStream: func(stream *esound.Stream) bool { return lan.addStream(esoundLive{stream}) }})
		go func() {
			if err := server.Serve(ctx, listener); err != nil {
				slog.Error("esound receiver", "err", err)
			}
		}()
		slog.Info("esound receiver listening", "address", esoundListen)
	}
	if upnpListen != "" {
		listener, err := net.Listen("tcp4", upnpListen)
		if err != nil {
			return err
		}
		server := upnp.NewServer(upnp.Config{Name: c.name, Player: &upnpRenderer{lan: lan, client: newUPNPClient()}})
		go func() {
			if err := server.Serve(ctx, listener); err != nil {
				slog.Error("upnp renderer", "err", err)
			}
		}()
		slog.Info("upnp renderer listening", "address", listener.Addr().String())
	}
	return nil
}

// nativeLAN tracks the sender streams behind the source cards.
type nativeLAN struct {
	c      *controller
	router *nativeRouter

	mu        sync.Mutex
	consumers map[string]*nativeConsumer // by source key
}

type nativeConsumer struct {
	stream liveStream
	cancel context.CancelFunc
	done   chan struct{} // Closed once it can no longer touch the device.
	id     string
	key    string
	lastAt time.Time
}

// senderKey is the identity remembered source slots were saved under.
// It sorts cards; it is descriptive and not authentication.
func senderKey(props map[string]string, ip string) (key, name, host string) {
	name = props["application.name"]
	if name == "" {
		name = "Pulse sender"
	}
	host = props["application.process.host"]
	peer := host
	if peer == "" {
		peer = ip
	}
	sum := sha256.Sum256([]byte("pulse:" + peer + ":" + name))
	if host != "" {
		name = host
	}
	return hex.EncodeToString(sum[:]), name, host
}

// addStream admits a sender's stream; its newest session replaces an older one.
// It reports whether a source slot was free.
func (l *nativeLAN) addStream(stream liveStream) bool {
	props := stream.Props()
	key, name, host := senderKey(props, stream.Peer())
	s := l.c.sources
	s.mu.Lock()
	var source *speakerSource
	for _, p := range s.Sources {
		if p.Key == key {
			source = p
			break
		}
	}
	if source == nil {
		used := map[string]bool{}
		for _, p := range s.Sources {
			used[p.ID] = true
		}
		for i := 1; i <= 7 && source == nil; i++ {
			if id := fmt.Sprintf("lan_%d", i); !used[id] {
				source = &speakerSource{ID: id, Key: key, meter: audiometer.New()}
				s.Sources = append(s.Sources, source)
				s.Revision++
			}
		}
	}
	if source == nil {
		s.Error = "Remembered source limit reached; new sources are blocked"
		s.mu.Unlock()
		slog.Warn("pulse sender rejected: no free source slot", "peer", stream.Peer())
		return false
	}
	if source.meter == nil {
		source.meter = audiometer.New()
	}
	source.Name, source.Host, source.IP = name, host, stream.Peer()
	source.Protocol = stream.Protocol()
	source.Connected, source.LastSeen = true, time.Now()
	source.Format = stream.PCM().local()
	source.Metadata = sourceMetadata(props)
	id := source.ID
	s.save()
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	consumer := &nativeConsumer{stream: stream, cancel: cancel, done: make(chan struct{}), id: id, key: key}
	l.mu.Lock()
	previous := l.consumers[key]
	l.consumers[key] = consumer
	l.mu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	slog.Info("sender connected", "protocol", stream.Protocol(), "source", id, "name", name, "format", source.Format.Format, "rate", source.Format.Rate, "channels", source.Format.Channels)
	go func() {
		defer close(consumer.done)
		// A slot has one writer: two would fight over the device's format, and a
		// blocking open could wait on a device that nobody then closes.
		if previous != nil {
			<-previous.done
		}
		l.consume(ctx, consumer)
	}()
	return true
}

// consume moves one stream at the output's pace, which is what the sender's own
// flow control follows, so a selected sender runs on the DAC's clock.
func (l *nativeLAN) consume(ctx context.Context, consumer *nativeConsumer) {
	stream, id := consumer.stream, consumer.id
	pcm := stream.PCM()
	defer stream.Close()
	stream.ObserveReceive(func(gap time.Duration) { metricReceiveGap.WithLabelValues(id).Observe(millis(gap)) })
	var lastUnderflows, lastOverflows, lastClipped uint64
	lastPass := time.Now()
	defer func() {
		consumer.cancel()
		l.mu.Lock()
		current := l.consumers[consumer.key] == consumer
		if current {
			delete(l.consumers, consumer.key)
		}
		l.mu.Unlock()
		if !current { // A newer stream from this sender owns the slot now.
			return
		}
		l.router.release(id, false)
		l.c.sources.mu.Lock()
		if p := l.c.sources.source(id); p != nil {
			p.Connected, p.Active = false, false
			p.LastSeen = time.Now()
		}
		l.c.sources.mu.Unlock()
	}()
	frames := pcm.rate * int(nativeSourcePeriod) / int(time.Second)
	raw := make([]byte, frames*pcm.channels*pcm.bytes)
	samples := make([]int32, frames*pcm.channels)
	f := format{rate: pcm.rate, channels: pcm.channels, bits: pcm.bits}
	peaks := nativePeaks{report: func(v float64, at time.Time) { l.c.local.nativePeak(id, v, at) }}
	var next time.Time
	var fill nativeFillPacer
	lastAudio := time.Now()
	for ctx.Err() == nil {
		select {
		case <-stream.Done():
			return
		default:
		}
		n := stream.Read(raw)
		stats := stream.Stats()
		metricUnderflows.WithLabelValues(id).Add(float64(stats.Underflows - lastUnderflows))
		metricOverflows.WithLabelValues(id).Add(float64(stats.Overflows - lastOverflows))
		metricClipped.WithLabelValues(id).Add(float64(stats.Clipped - lastClipped))
		lastUnderflows, lastOverflows, lastClipped = stats.Underflows, stats.Overflows, stats.Clipped
		passAt := time.Now()
		metricLoopGap.WithLabelValues(id).Observe(millis(passAt.Sub(lastPass)))
		lastPass = passAt
		if n == 0 {
			metricEmptyReads.WithLabelValues(id).Inc()
			if time.Since(lastAudio) > nativeSourceIdle {
				l.router.release(id, false) // A silent tunnel does not hold the DAC.
			}
			peaks.release(time.Now())
			select {
			case <-ctx.Done():
				return
			case <-stream.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
			continue
		}
		lastAudio = time.Now()
		count := stream.Decode(samples, raw[:n])
		metricStreamBuffer.WithLabelValues(id).Observe(float64(stats.BufferedUsec) / 1000)
		writeAt := time.Now()
		err := l.router.write(id, f, samples[:count])
		delay := time.Duration(0)
		switch {
		case err == nil:
			metricWriteTime.WithLabelValues(id).Observe(millis(time.Since(writeAt)))
			delay = time.Duration(l.router.delay(id)) * time.Second / time.Duration(pcm.rate)
			metricDeviceDelay.WithLabelValues(id).Observe(millis(delay))
			stream.SetDeviceDelay(uint64(delay / time.Microsecond))
			next = fill.wait(next, count/pcm.channels, pcm.rate, delay, time.Now())
		case errors.Is(err, errNotRouted):
			fill.reset()
			stream.SetDeviceDelay(0)
			next = nativePace(next, count/pcm.channels, pcm.rate)
		default:
			metricOutputErrors.WithLabelValues(id).Inc()
			slog.Warn("pulse sender output", "source", id, "err", err)
			next = nativePace(next, count/pcm.channels, pcm.rate)
		}
		peaks.add(samples[:count], f, delay, time.Now())
	}
}

// setVolume maps a selected sender's volume to the hardware's configured range.
// Samples are never scaled: this is the same control as the knob.
func (l *nativeLAN) setVolume(stream *pulse.Stream, pulseVolume uint32) {
	mixer := l.c.mixer
	if mixer == nil {
		return
	}
	key, _, _ := senderKey(stream.Props(), stream.Peer)
	s := l.c.sources
	s.mu.Lock()
	selected := false
	if p := s.source(s.Selected); p != nil {
		selected = p.Key == key
	}
	s.mu.Unlock()
	if !selected {
		return
	}
	control := mixer.snapshot()
	level := senderLevel(pulseVolume, control)
	if err := mixer.setLevel(context.Background(), level); err != nil {
		slog.Warn("sender volume", "err", err)
		return
	}
	l.c.reportVolume(context.Background())
}

// bufferStatus reads a source's queues for display. The lock order is sources,
// then router, then stream; none of those paths take the sources lock.
func (l *nativeLAN) bufferStatus(p *speakerSource) *sourceBuffer {
	buffer := &sourceBuffer{}
	rate := 0
	if p.Format != nil {
		rate = p.Format.Rate
	}
	if queued, capacity := l.router.deviceFill(p.ID); rate > 0 {
		buffer.DeviceMS, buffer.DeviceMaxMS = queued*1000/rate, capacity*1000/rate
	}
	if backend, ok := l.c.backend.(*nativeBackend); ok && p.ID == "slopify" {
		buffer.StreamMS, buffer.StreamMaxMS = backend.queueFill()
	}
	l.mu.Lock()
	consumer := l.consumers[p.Key]
	l.mu.Unlock()
	if consumer != nil {
		p.Metadata = sourceMetadata(consumer.stream.Props()) // Senders retitle tracks.
		stats := consumer.stream.Stats()
		buffer.StreamMS, buffer.StreamMaxMS = int(stats.BufferedUsec/1000), int(stats.TargetUsec/1000)
		buffer.Underflows = stats.Underflows
	}
	if buffer.StreamMaxMS == 0 && buffer.DeviceMaxMS == 0 {
		return nil
	}
	return buffer
}

// senderLevel maps a Pulse volume onto the hardware's configured range. Pulse volumes
// are cubic, so the fraction is already perceptual: 0x10000 is 100%.
func senderLevel(pulseVolume uint32, c volume.Control) int {
	fraction := min(float64(pulseVolume)/0x10000, 1)
	return c.Min + int(math.Round(fraction*float64(c.Max-c.Min)))
}

// senderVolume is the inverse of senderLevel.
func senderVolume(level int, c volume.Control) uint32 {
	if c.Max <= c.Min {
		return 0x10000
	}
	return uint32(float64(level-c.Min) / float64(c.Max-c.Min) * 0x10000)
}

// syncSelectedSenderVolume moves the slider of only the sender connected to the amp.
// A sender already at this level is left alone, so a drag never fights its own echo.
func (l *nativeLAN) syncSelectedSenderVolume(selectedKey string) {
	mixer := l.c.mixer
	if mixer == nil || selectedKey == "" {
		return
	}
	l.mu.Lock()
	consumer := l.consumers[selectedKey]
	l.mu.Unlock()
	if consumer == nil {
		return
	}
	control := mixer.snapshot()
	if senderLevel(consumer.stream.Volume(), control) != control.Level {
		consumer.stream.SetVolume(senderVolume(control.Level, control))
	}
}

// arbitrate applies automatic selection and routes the selected source.
func (l *nativeLAN) arbitrate(ctx context.Context) {
	s := l.c.sources
	previous := ""
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		l.c.local.mu.Lock()
		playback := l.c.local.playback.State
		l.c.local.mu.Unlock()
		now := time.Now()
		s.mu.Lock()
		for _, p := range s.Sources {
			if p.ID == "slopify" && l.c.server != "" {
				p.Connected = true
				updateSourceStartup(p, playback, now)
			}
			if p.meter != nil {
				observeSourceSignal(p, p.meter.Snapshot(), now)
			}
			if !p.Connected && now.Sub(p.LastSeen) > 2*time.Second {
				p.Sounding = false
			}
			p.Active = l.router.owns(p.ID)
			p.Buffer = l.bufferStatus(p)
		}
		s.chooseSource(now)
		selected := s.Selected
		selectedKey := ""
		if p := s.source(selected); p != nil && p.ID != "slopify" {
			selectedKey = p.Key
		}
		for _, p := range s.Sources {
			value := 0.0
			if p.ID == selected {
				value = 1
			}
			metricSelected.WithLabelValues(p.ID).Set(value)
		}
		s.mu.Unlock()
		if selected != previous {
			if previous != "" {
				metricSelectionChanges.Inc()
			}
			previous = selected
		}
		l.router.selectSource(selected)
		l.syncSelectedSenderVolume(selectedKey)
	}
}
