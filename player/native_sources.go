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
	"net/netip"
	"sync"
	"time"

	"github.com/codemowers/slopify/internal/volume"
	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/audiometer"
	"github.com/codemowers/slopify/player/internal/esound"
	"github.com/codemowers/slopify/player/internal/pcmring"
	"github.com/codemowers/slopify/player/internal/pulse"
	"github.com/codemowers/slopify/player/internal/rtp"
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
	switchMu   sync.Mutex
	transition chan struct{}
	open       func(id string, f format) (nativeOutput, error)
	onDevice   func(output *localPCM)

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

// selectSourceWithCue closes the device if another source held it.
func (r *nativeRouter) selectSourceWithCue(id string, sound bool) {
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	r.mu.Lock()
	if r.selected == id {
		r.mu.Unlock()
		return
	}
	cue := sound && r.selected != "" && id != ""
	f := r.format
	old := r.device
	done := make(chan struct{})
	r.transition = done
	r.selected = id
	r.device = nil
	r.owner = ""
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.transition = nil; close(done); r.mu.Unlock() }()
	if old != nil {
		old.close()
		outputRate("", 0)
		r.reportDevice(nil)
	}
	if cue {
		if err := r.playTransitionSound(f); err != nil {
			slog.Warn("transition sound unavailable", "err", err)
		}
	}
}

func (r *nativeRouter) owns(id string) bool {
	return r.deviceFor(id) != nil
}

func (r *nativeRouter) deviceFor(id string) *nativeDevice {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != id {
		return nil
	}
	return r.device
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
	for r.transition != nil && r.selected == id {
		done := r.transition
		r.mu.Unlock()
		<-done
		r.mu.Lock()
	}
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
		if metricsEnabled {
			metricOpens.WithLabelValues(audio(stageOutput, id)...).Inc()
		}
		outputRate(id, f.rate)
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
	outputRate("", 0)
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
		// A source switch or reopen can leave the old deadline behind now.
		// Catching up on that deadline would drain the sender in a burst.
		next = now
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

// BufferFrames is the device buffer while this output holds the device, else zero.
func (o *routedOutput) BufferFrames() int {
	_, capacity := o.router.deviceFill(o.id)
	return capacity
}

// startNativeSources routes playback and, when configured,
// the trusted-LAN Pulse receiver.
func (c *controller) startNativeSources(ctx context.Context, backend *nativeBackend, pulseListen, esoundListen string, pulseBufferMS, esoundBufferMS, esoundSocketMS int, upnpListen, sinkName string, rtpConfig *rtp.Config) error {
	deep, live := backend.open, backend.openLive
	router := &nativeRouter{onDevice: c.local.nativeDevice, open: func(id string, f format) (nativeOutput, error) {
		open := live
		if id == "slopify" {
			open = deep // Local files can afford a deep buffer.
		}
		output, err := open(f)
		if a, ok := output.(*alsaOutput); ok {
			a.id = id
		}
		return output, err
	}}
	backend.open = func(f format) (nativeOutput, error) { return &routedOutput{router: router, id: "slopify", f: f}, nil }
	backend.pauseCue = router.playPauseSound
	backend.startCue = func(f format) error { return router.playControlSound(f, transitionSound) }
	// The transition sounds setting covers pause, seek and stall sounds too.
	backend.cues = func() bool {
		sources := c.local.nativeSources
		if sources == nil {
			return true
		}
		sources.mu.Lock()
		defer sources.mu.Unlock()
		return sources.TransitionSounds
	}
	lan := &nativeLAN{c: c, router: router, consumers: map[string]*nativeConsumer{}}
	c.nativeLAN = lan
	c.remoteCommands = make(chan remoteCommand, 1)
	go c.runRemoteCommands(ctx)
	c.senderVolumes = make(chan senderVolumeRequest, 1)
	go c.runSenderVolumes(ctx)
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
		sinkFormat := pulseSinkFormat(backend.alsaDevice, alsa.Formats)
		server := pulse.NewServer(pulse.Config{Name: sinkName, Description: c.name + " via PulseAudio", MinBufferMillis: pulseBufferMS, Format: sinkFormat,
			OnStream: func(stream *pulse.Stream) { lan.addStream(pulseLive{stream}) }, OnVolume: lan.setVolume})
		go func() {
			if err := server.Serve(ctx, listener); err != nil {
				slog.Error("pulse receiver", "err", err)
			}
		}()
		advertise(ctx, "_pulse-sink._tcp", listener, []string{"device=" + sinkName, "description=" + c.name + " via PulseAudio", "format=" + pulse.FormatName(sinkFormat)},
			func(collision int) string { return collisionName(c.name, collision) + " via PulseAudio" })
		slog.Info("pulse receiver listening", "address", pulseListen, "sink", sinkName, "format", pulse.FormatName(sinkFormat))
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
	if rtpConfig != nil {
		rtpConfig.Name = c.name
		rtpConfig.Control = c.link.controlPacket
		rtpConfig.OnControl = c.link.receiveControl
		rtpConfig.OnStream = func(stream *rtp.Stream) bool { return lan.addStream(rtpLive{stream}) }
		go retryRTPReceiver(ctx, rtp.NewReceiver(*rtpConfig).Start, time.Second)
	}
	return nil
}

// retryRTPReceiver starts the receiver once the network allows it. Joining a
// multicast group fails until an interface is up, which at boot can be after
// the player starts, and that must not cost the player its other sources.
func retryRTPReceiver(ctx context.Context, start func(context.Context) error, delay time.Duration) {
	for ctx.Err() == nil {
		err := start(ctx)
		if err == nil {
			slog.Info("rtp receiver listening for announcements")
			return
		}
		slog.Warn("rtp receiver unavailable", "err", err, "retry", delay)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
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

// RTP session identity is source IP plus group, independent of display name,
// port and SSRC. A same-group collision between devices must never merge cards.
func liveSenderKey(stream liveStream) (key, name, host string) {
	switch s := stream.(type) {
	case rtpLive:
		sum := sha256.Sum256([]byte("rtp:" + s.Peer() + ":" + s.Group.String()))
		return hex.EncodeToString(sum[:]), s.Name + " · " + s.Group.String(), ""
	case raopLive:
		// Its own namespace: a laptop sending Pulse and AirPlay as "PipeWire" keeps two cards.
		sum := sha256.Sum256([]byte("airplay:" + s.Peer() + ":" + s.Name))
		return hex.EncodeToString(sum[:]), s.Name, ""
	}
	return senderKey(stream.Props(), stream.Peer())
}

// addStream admits a sender's stream; its newest session replaces an older one.
// It reports whether a source slot was free.
func (l *nativeLAN) addStream(stream liveStream) bool {
	props := stream.Props()
	key, name, host := liveSenderKey(stream)
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
		for i := 1; i <= maxLANSources && source == nil; i++ {
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
		slog.Warn("sender rejected: no free source slot", "protocol", stream.Protocol(), "peer", stream.Peer())
		return false
	}
	if source.meter == nil {
		source.meter = audiometer.New()
	}
	source.Name, source.Host, source.IP = name, host, stream.Peer()
	source.Protocol = stream.Protocol()
	if rtpStream, ok := stream.(rtpLive); ok {
		source.RTPGroup = netip.AddrPortFrom(rtpStream.Group, rtpStream.Port).String()
		source.RTPInstance = rtpStream.Metadata().Instance
	}
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
	group := ""
	if rtpStream, ok := stream.(rtpLive); ok {
		group = rtpStream.Group.String()
	}
	noteSource(id, stream.Protocol(), stream.Peer(), group)
	input := audio(stageInput, id)
	if metricsEnabled {
		metricRate.WithLabelValues(input...).Set(float64(pcm.rate))
		for event := range pcmring.StreamEventCount {
			metricStreamEvents.WithLabelValues(append(audio(stageInput, id), event.String())...).Add(0)
		}
		defer metricRate.DeleteLabelValues(input...)
	}
	if metricsEnabled {
		stream.ObserveReceive(func(gap time.Duration) {
			metricReceiveGap.WithLabelValues(audio(stageInput, id)...).Observe(gap.Seconds())
			metricPackets.WithLabelValues(audio(stageInput, id)...).Inc()
		})
	}
	var lastUnderflows, lastOverflows, lastClipped, lastLost uint64
	var lastEvents [pcmring.StreamEventCount]uint64
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
	frames := nativeSourceFrames(pcm.rate)
	raw := make([]byte, frames*pcm.channels*pcm.bytes)
	samples := make([]int32, frames*pcm.channels)
	f := format{rate: pcm.rate, channels: pcm.channels, bits: pcm.bits}
	peaks := nativePeaks{report: func(v uint32, at time.Time) {
		l.c.local.nativePeak(id, v, at)
		if l.c.link.source() == id {
			l.c.local.nativePeakFrom("slopify", id, v, at)
		}
	}}
	var next time.Time
	var fill nativeFillPacer
	var device *nativeDevice
	lastAudio := time.Now()
	for ctx.Err() == nil {
		select {
		case <-stream.Done():
			return
		default:
		}
		n := stream.Read(raw)
		stats := stream.Stats()
		if metricsEnabled {
			for event, count := range stats.Events {
				if delta := count - lastEvents[event]; delta > 0 {
					metricStreamEvents.WithLabelValues(append(audio(stageInput, id), pcmring.StreamEvent(event).String())...).Add(float64(delta))
				}
			}
			metricUnderruns.WithLabelValues(input...).Add(float64(stats.Underflows - lastUnderflows))
			metricOverflows.WithLabelValues(input...).Add(float64(stats.Overflows - lastOverflows))
			metricClipped.WithLabelValues(input...).Add(float64(stats.Clipped - lastClipped))
			metricLostFrames.WithLabelValues(input...).Add(float64(stats.Lost - lastLost))
		}
		lastUnderflows, lastOverflows, lastClipped, lastLost = stats.Underflows, stats.Overflows, stats.Clipped, stats.Lost
		lastEvents = stats.Events
		passAt := time.Now()
		if metricsEnabled {
			metricLoopGap.WithLabelValues(audio(stageOutput, id)...).Observe(passAt.Sub(lastPass).Seconds())
		}
		lastPass = passAt
		if n == 0 {
			if time.Since(lastAudio) > nativeSourceIdle {
				l.router.release(id, false) // A silent tunnel does not hold the DAC.
				stream.SetDeviceDelay(0)
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
		if metricsEnabled {
			metricBytes.WithLabelValues(input...).Add(float64(n))
		}
		count := stream.Decode(samples, raw[:n])
		if metricsEnabled {
			metricBuffer.WithLabelValues(input...).Observe(float64(stats.BufferedUsec) / 1e6)
		}
		writeAt := time.Now()
		err := l.router.write(id, f, samples[:count])
		delay := time.Duration(0)
		switch {
		case err == nil:
			if current := l.router.deviceFor(id); current != device {
				device = current
				fill.reset()
			}
			if metricsEnabled {
				metricWriteTime.WithLabelValues(audio(stageOutput, id)...).Observe(time.Since(writeAt).Seconds())
			}
			delay = time.Duration(l.router.delay(id)) * time.Second / time.Duration(pcm.rate)
			if metricsEnabled {
				metricBuffer.WithLabelValues(audio(stageOutput, id)...).Observe(delay.Seconds())
			}
			stream.SetDeviceDelay(uint64(delay / time.Microsecond))
			next = fill.wait(next, count/pcm.channels, pcm.rate, delay, time.Now())
		case errors.Is(err, errNotRouted):
			fill.reset()
			stream.SetDeviceDelay(0)
			next = nativePace(next, count/pcm.channels, pcm.rate)
		default:
			errorAt(stageOutput, id, "write")
			slog.Warn("pulse sender output", "source", id, "err", err)
			next = nativePace(next, count/pcm.channels, pcm.rate)
		}
		peaks.add(samples[:count], f, delay, time.Now())
	}
}

// setVolume maps a selected sender's volume to the hardware's configured range.
// Samples are never scaled: this is the same control as the knob.
func (l *nativeLAN) setVolume(stream *pulse.Stream, pulseVolume uint32) {
	key, _, _ := senderKey(stream.Props(), stream.Peer)
	l.c.requestSenderVolume(key, pulseSenderLevel(pulseVolume))
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
		if stream, ok := consumer.stream.(rtpLive); ok {
			p.RTPInstance = stream.Metadata().Instance
			p.RTPGroup = netip.AddrPortFrom(stream.Group, stream.Port).String()
		}
		p.Metadata = sourceMetadata(consumer.stream.Props()) // Senders retitle tracks.
		if stream, ok := consumer.stream.(remoteSource); ok {
			p.Remote = stream.Controllable()
		}
		stats := consumer.stream.Stats()
		buffer.StreamMS, buffer.StreamMaxMS = int(stats.BufferedUsec/1000), int(stats.TargetUsec/1000)
		buffer.Underflows = stats.Underflows
		buffer.Overflows = stats.Overflows
		if consumer.stream.Protocol() == "pulse" {
			buffer.Events = make(map[string]uint64, pcmring.StreamEventCount)
			for event, count := range stats.Events {
				buffer.Events[pcmring.StreamEvent(event).String()] = count
			}
		}
		if consumer.stream.Protocol() == "pulse" && buffer.DeviceMaxMS > 0 {
			offset := buffer.StreamMS + buffer.DeviceMS
			buffer.SyncOffsetMS = &offset
		}
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
	if mixer == nil || selectedKey == "" || l.c.senderVolumeRecent() {
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
		followSource := l.c.link.source()
		unavailable := l.c.link.unavailableSenders(now)
		s.mu.Lock()
		for _, p := range s.Sources {
			p.QueueInput = p.ID == followSource && s.Selected == "slopify"
			p.SenderUnavailable = p.Protocol == "rtp" && unavailable[p.IP]
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
		// The logical source stays Slopify. Mirror its multicast input's live
		// signal so automatic routing and the UI retain the queue's identity.
		if input := s.source(followSource); input != nil && followSource != "" {
			if queue := s.source("slopify"); queue != nil {
				queue.Starting = false
				queue.State = "running"
				queue.Format, queue.Buffer = input.Format, input.Buffer
				queue.Sounding, queue.SignalAt = input.Sounding, input.SignalAt
				queue.AudibleSince, queue.LastAudible = input.AudibleSince, input.LastAudible
				queue.LastSound = input.LastSound
				queue.Active = input.Active
			}
		}
		s.chooseSource(now)
		selected := s.Selected
		transitionSounds := s.TransitionSounds
		selectedKey := ""
		if p := s.source(selected); p != nil && p.ID != "slopify" {
			selectedKey = p.Key
		}
		for _, p := range s.Sources {
			value := 0.0
			if p.ID == selected {
				value = 1
			}
			if p.Protocol != "" {
				noteSource(p.ID, p.Protocol, p.IP, p.RTPGroup)
			}
			i := identityOf(p.ID)
			if metricsEnabled {
				metricSelected.WithLabelValues(i.transport, i.label).Set(value)
			}
		}
		s.mu.Unlock()
		logicalChanged := selected != previous
		if logicalChanged {
			if previous != "" {
				if metricsEnabled {
					metricSelectionChanges.Inc()
				}
			}
			previous = selected
		}
		routed := selected
		if selected == "slopify" && followSource != "" {
			routed = followSource
		}
		if l.c.rtpSender != nil {
			l.c.rtpSender.SetSourceEligible(multicastSourceEligible(selected, followSource))
		}
		l.router.selectSourceWithCue(routed, transitionSounds && logicalChanged)
		l.syncSelectedSenderVolume(selectedKey)
	}
}

// Only locally decoded queue audio may supply multicast. Future queue backends
// can join this allowlist when they implement the sender path; LAN inputs cannot.
func multicastSourceEligible(selected, upstream string) bool {
	return multicastSources[selected] && upstream == ""
}

// multicastSources may be retransmitted over multicast: Slopify queue playback and,
// once its backend exists and feeds the sender, Spotify. LAN inputs (Pulse, ESound,
// UPnP, multicast, AirPlay, Chromecast) never are; they are not ours to rebroadcast.
var multicastSources = map[string]bool{"slopify": true, "spotify": true}

// Multiply in 64 bits: rate * nanoseconds overflows an int on 32-bit ARM.
func nativeSourceFrames(rate int) int {
	return int(int64(rate) * int64(nativeSourcePeriod) / int64(time.Second))
}
