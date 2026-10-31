package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics answer where a glitch came from: the network, the stream queue, the
// device queue or this process's scheduling. Labels are bounded source slots
// (slopify, lan_1..lan_7), never sender-supplied names or addresses.
var (
	nativeMetricsRegistry = prometheus.NewRegistry()

	bufferBucketsMS = []float64{0, 5, 10, 20, 40, 60, 80, 100, 150, 200, 300, 400, 500, 750, 1000, 2000}
	timingBucketsMS = []float64{1, 2, 5, 10, 15, 20, 25, 30, 40, 50, 75, 100, 150, 250, 500, 1000, 2500}

	metricStreamBuffer = sourceHistogram("slopify_audio_stream_buffer_ms",
		"Sender audio queued in the receiver, per output write.", bufferBucketsMS)
	metricDeviceDelay = sourceHistogram("slopify_audio_device_delay_ms",
		"Audio written to the device but not yet heard, per output write.", bufferBucketsMS)
	metricWriteTime = sourceHistogram("slopify_audio_device_write_ms",
		"Time one device write blocked. Near the chunk length is healthy; much longer means a stall.", timingBucketsMS)
	metricReceiveGap = sourceHistogram("slopify_audio_receive_gap_ms",
		"Time between blocks arriving from a sender: network and sender jitter.", timingBucketsMS)
	metricLoopGap = sourceHistogram("slopify_audio_output_loop_ms",
		"Time between output loop passes for a source: scheduling and stalls here.", timingBucketsMS)

	metricUnderflows = sourceCounter("slopify_audio_stream_underflows_total",
		"Sender stream ran empty. A PipeWire sender rewinds and repeats audio on each one.")
	metricOverflows = sourceCounter("slopify_audio_stream_overflows_total",
		"Sender sent more than the queue holds, and audio was dropped.")
	metricClipped = sourceCounter("slopify_audio_stream_clipped_samples_total",
		"Float samples from a sender beyond full scale, cut off at conversion to integer.")
	metricEmptyReads = sourceCounter("slopify_audio_stream_empty_reads_total",
		"Output found no sender audio to write: silence on the wire or a dry queue.")
	metricDeviceOpens = sourceCounter("slopify_audio_device_opens_total",
		"Device opened for a source; each one is a gap.")
	metricOutputErrors = sourceCounter("slopify_audio_output_errors_total",
		"Writes that failed for a reason other than the source not being selected.")

	metricDeviceUnderruns = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_audio_device_underruns_total",
		Help: "Device ran out of audio: an audible dropout."})
	metricSelectionChanges = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_audio_selection_changes_total",
		Help: "Selected source changed."})
	metricSelected = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "slopify_audio_source_selected",
		Help: "1 for the selected source."}, []string{"source"})
	metricDeviceRate = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_audio_device_rate_hertz",
		Help: "Sample rate the device is open at; 0 when closed."})

	metricDecodeQueue = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "slopify_audio_decode_queue_chunks",
		Help:    "Decoded chunks waiting for the device, per write; near zero means decoding or the network is too slow.",
		Buckets: []float64{0, 1, 2, 4, 8, 16, 24, 32}})
	metricStartup = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "slopify_audio_decode_start_seconds",
		Help:    "From requesting an original to the first decoded audio, including any seek.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 4, 8, 16, 32}})
	metricRangeFetch = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "slopify_audio_source_fetch_seconds",
		Help:    "One bounded range request for an original.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2, 5, 10, 20}})
	metricDecodeFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_audio_decode_failures_total",
		Help: "Originals the native engine stopped decoding."})
)

func sourceHistogram(name, help string, buckets []float64) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, []string{"source"})
}

func sourceCounter(name, help string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, []string{"source"})
}

func init() {
	nativeMetricsRegistry.MustRegister(metricStreamBuffer, metricDeviceDelay, metricWriteTime, metricReceiveGap, metricLoopGap,
		metricUnderflows, metricOverflows, metricClipped, metricEmptyReads, metricDeviceOpens, metricOutputErrors,
		metricDeviceUnderruns, metricSelectionChanges, metricSelected, metricDeviceRate,
		metricDecodeQueue, metricStartup, metricRangeFetch, metricDecodeFailures,
		// Garbage collection pauses and scheduling latency are prime glitch suspects.
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
