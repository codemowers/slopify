package main

import (
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/codemowers/slopify/player/internal/rtp"
)

// Metrics answer where a glitch came from. Every audio series carries three
// bounded labels:
//
//   - stage: download (fetching an original), input (a sender's stream or the
//     decoded queue) or output (the device, or the multicast copy);
//   - transport: how audio arrives or leaves: https (or http) for the queue's
//     originals, multicast, pulse, airplay, esound, upnp, or cue for the
//     source-change sound;
//   - source: the server's hostname for the queue (transition for the cue),
//     otherwise the sender's IP address, with @group for multicast because one
//     host can send to several groups. It is configuration or the verified peer
//     address, never a sender-supplied name; a slot taken over by another sender
//     drops the previous one's series, so there are never more series than slots.
//
// So `sum by (stage, transport) (rate(slopify_audio_underruns_total[5m]))` says
// whether dropouts began on the network, in a sender's stream or at the device.
const (
	stageDownload = "download"
	stageInput    = "input"
	stageOutput   = "output"
)

var audioLabelNames = []string{"stage", "transport", "source"}

// Startup-only switch. Keep float histogram work out of soft-float routers by
// default; -metrics can explicitly enable it on any architecture.
var metricsEnabled = defaultMetrics(runtime.GOARCH)

func defaultMetrics(arch string) bool { return !strings.HasPrefix(arch, "mips") }

// queueIdentity labels the Slopify queue: set from SLOPIFY_SERVER at startup,
// before anything is recorded.
var queueIdentity = sourceIdentity{"https", "slopify"}

func setQueueSource(server string) {
	if u, err := url.Parse(server); err == nil && u.Hostname() != "" && (u.Scheme == "https" || u.Scheme == "http") {
		queueIdentity = sourceIdentity{u.Scheme, u.Hostname()}
	}
}

type sourceIdentity struct{ transport, label string }

// sourceIdentities maps LAN slots to their current sender's labels.
var sourceIdentities sync.Map

// noteSource records who occupies a slot: its protocol, peer IP and, for
// multicast, its group (with or without port). A peer that is not an IP
// address, such as a UPnP URL host, keeps the slot ID.
func noteSource(id, protocol, peer, group string) {
	if !metricsEnabled {
		return
	}
	if protocol == "rtp" {
		protocol = "multicast"
	}
	label := id
	if addr, err := netip.ParseAddr(peer); err == nil {
		label = addr.Unmap().String()
		if g, err := netip.ParseAddrPort(group); err == nil {
			label += "@" + g.Addr().String()
		} else if g, err := netip.ParseAddr(group); err == nil {
			label += "@" + g.String()
		}
	}
	next := sourceIdentity{protocol, label}
	old, loaded := sourceIdentities.Swap(id, next)
	if !loaded {
		forgetSource(id) // Anything recorded under the slot before it was known.
	} else if previous := old.(sourceIdentity); previous != next {
		forgetSource(previous.label)
	}
}

func identityOf(id string) sourceIdentity {
	switch id {
	case "", "slopify":
		return queueIdentity
	case "transition":
		return sourceIdentity{"cue", "transition"}
	}
	if v, ok := sourceIdentities.Load(id); ok {
		return v.(sourceIdentity)
	}
	return sourceIdentity{"unknown", id}
}

// audio returns the label values of a stage of a source slot.
func audio(stage, id string) []string {
	i := identityOf(id)
	return []string{stage, i.transport, i.label}
}

// outputRate records the device's rate under its owner, or no output series
// when rate is 0 because the device closed.
func outputRate(id string, rate int) {
	if !metricsEnabled {
		return
	}
	metricRate.DeletePartialMatch(prometheus.Labels{"stage": stageOutput})
	if rate > 0 {
		metricRate.WithLabelValues(audio(stageOutput, id)...).Set(float64(rate))
	}
}

// errorAt counts a failure at a stage of a source.
func errorAt(stage, id, reason string) {
	if !metricsEnabled {
		return
	}
	metricErrors.WithLabelValues(append(audio(stage, id), reason)...).Inc()
}

func forgetSource(label string) {
	if !metricsEnabled {
		return
	}
	match := prometheus.Labels{"source": label}
	for _, v := range []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{metricBuffer, metricWriteTime, metricReceiveGap, metricLoopGap, metricFetch, metricStartup,
		metricUnderruns, metricOverflows, metricErrors, metricStreamEvents, metricLostFrames, metricClipped, metricOpens, metricBytes, metricPackets,
		metricBufferBytes, metricBufferLimitBytes, metricSelected, metricRate} {
		v.DeletePartialMatch(match)
	}
}

const (
	helpOverflows = "Queue overflow events that dropped audio: a sender outpacing input, or a full multicast output queue."
	helpErrors    = "Failures, by reason. output: write, send (multicast); input: decode, unsupported, stream_url, or a download reason; download: network, status, range, changed, url."
	helpBytes     = "Bytes at a stage: download from the server, input taken from a sender's stream, output multicast."
	helpPackets   = "Packets at a stage: input blocks arriving from a sender (RTP packets for multicast), output RTP packets multicast."
)

var (
	nativeMetricsRegistry = prometheus.NewRegistry()

	bufferBuckets = []float64{0, .005, .01, .02, .04, .06, .08, .1, .15, .2, .3, .4, .5, .75, 1, 2, 4, 8}
	timingBuckets = []float64{.001, .002, .005, .01, .015, .02, .025, .03, .04, .05, .075, .1, .15, .25, .5, 1, 2.5}

	metricBuffer = audioHistogram("slopify_audio_buffer_seconds",
		"Audio queued at a stage, per output write: input is a sender's stream or decoded originals, output is written to the device but not yet heard.", bufferBuckets)
	metricWriteTime = audioHistogram("slopify_audio_write_seconds",
		"Time one output write blocked. Near the chunk length is healthy; much longer means a stall.", timingBuckets)
	metricReceiveGap = audioHistogram("slopify_audio_receive_gap_seconds",
		"Time between blocks arriving at input: network and sender jitter.", timingBuckets)
	metricLoopGap = audioHistogram("slopify_audio_loop_seconds",
		"Time between output loop passes for a source: scheduling and stalls here.", timingBuckets)
	metricFetch = audioHistogram("slopify_audio_fetch_seconds",
		"One bounded range request for an original.", []float64{.01, .025, .05, .1, .25, .5, 1, 2, 5, 10, 20})
	metricStartup = audioHistogram("slopify_audio_start_seconds",
		"From requesting an original to the first decoded audio, including any seek.", []float64{.05, .1, .25, .5, 1, 2, 4, 8, 16, 32})

	metricUnderruns = audioCounter("slopify_audio_underruns_total",
		"A stage ran out of audio. download: decoding waited for the network; input: a sender's stream or the decoded queue ran dry (a PipeWire sender rewinds and repeats audio); output: the device ran dry, an audible dropout.")
	metricOverflows    = audioCounter("slopify_audio_overflows_total", helpOverflows)
	metricStreamEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "slopify_audio_stream_events_total",
		Help: "Observed receiver stream events, not sender-internal clock corrections: cork, uncork, flush, seek, rewind and timing replies.",
	}, append(append([]string{}, audioLabelNames...), "event"))
	metricErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_audio_errors_total", Help: helpErrors},
		append(append([]string{}, audioLabelNames...), "reason"))
	metricLostFrames = audioCounter("slopify_audio_lost_frames_total",
		"Frames lost on the way from a multicast sender, played as silence.")
	metricClipped = audioCounter("slopify_audio_clipped_samples_total",
		"Float samples from a sender beyond full scale, cut off at conversion to integer.")
	metricBytes   = audioCounter("slopify_audio_bytes_total", helpBytes)
	metricPackets = audioCounter("slopify_audio_packets_total", helpPackets)
	metricOpens   = audioCounter("slopify_audio_opens_total",
		"Device opened for a source; each one is a gap.")

	metricBufferBytes = audioGauge("slopify_audio_buffer_bytes",
		"Originals downloaded and not yet decoded, current and next track together.")
	metricBufferLimitBytes = audioGauge("slopify_audio_buffer_limit_bytes",
		"Most originals held ahead of decoding: -read-ahead-mib, or half the memory limit.")

	metricSelectionChanges = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_audio_selection_changes_total",
		Help: "Selected source changed."})
	metricSelected = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "slopify_audio_source_selected",
		Help: "1 for the selected source."}, []string{"transport", "source"})
	metricRate = audioGauge("slopify_audio_rate_hertz",
		"Sample rate at a stage: input is what a connected source delivers, output what the device is open at for the source that owns it. Absent when not open.")
)

func audioHistogram(name, help string, buckets []float64) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, audioLabelNames)
}

func audioCounter(name, help string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, audioLabelNames)
}

func audioGauge(name, help string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, audioLabelNames)
}

func init() {
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "slopify_player_build_info",
		Help: "Player build metadata; timestamps are UTC RFC3339, unknown when unavailable. Value is always 1.",
		ConstLabels: prometheus.Labels{
			"version": playerBuild.Version, "revision": playerBuild.Revision,
			"modified": playerBuild.Modified, "build_time": playerBuild.BuildTime,
			"commit_time": playerBuild.CommitTime, "go_version": playerBuild.GoVersion,
		},
	})
	build.Set(1)
	nativeMetricsRegistry.MustRegister(build)
	nativeMetricsRegistry.MustRegister(metricBuffer, metricWriteTime, metricReceiveGap, metricLoopGap, metricFetch, metricStartup,
		metricUnderruns, metricOverflows, metricErrors, metricStreamEvents, metricLostFrames, metricClipped, metricOpens, metricBytes, metricPackets,
		metricBufferBytes, metricBufferLimitBytes, metricSelectionChanges, metricSelected, metricRate,
		// Garbage collection pauses and scheduling latency are prime glitch suspects.
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

var (
	descMulticastOut = func(name, help string, extra ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, append(append([]string{}, audioLabelNames...), extra...), nil)
	}
	descPackets   = descMulticastOut("slopify_audio_packets_total", helpPackets)
	descBytes     = descMulticastOut("slopify_audio_bytes_total", helpBytes)
	descOverflows = descMulticastOut("slopify_audio_overflows_total", helpOverflows)
	descErrors    = descMulticastOut("slopify_audio_errors_total", helpErrors, "reason")
)

// senderCollector reports the multicast copy of the queue as output stage,
// transport multicast. It is unchecked (describes nothing) because its overflows and
// errors share families with the labelled vectors.
type senderCollector struct{ status func() rtp.SenderStatus }

func (senderCollector) Describe(chan<- *prometheus.Desc) {}

func (c senderCollector) Collect(ch chan<- prometheus.Metric) {
	v := c.status()
	labels := []string{stageOutput, "multicast", identityOf("slopify").label}
	for _, m := range []struct {
		desc  *prometheus.Desc
		value uint64
		extra []string
	}{{descPackets, v.Packets, nil}, {descBytes, v.Bytes, nil}, {descOverflows, v.Discards, nil}, {descErrors, v.Errors, []string{"send"}}} {
		ch <- prometheus.MustNewConstMetric(m.desc, prometheus.CounterValue, float64(m.value), append(append([]string{}, labels...), m.extra...)...)
	}
}

func registerRTPSenderMetrics(s *rtp.Sender) {
	if !metricsEnabled {
		return
	}
	nativeMetricsRegistry.MustRegister(senderCollector{s.Status})
}
