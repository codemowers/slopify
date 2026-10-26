package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/sidecar"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Paths belong in logs, never metric labels. The per-directory map is private
// accounting for library gauges, and is only touched by the indexing goroutine.
type indexMetrics struct {
	registry      *prometheus.Registry
	filesIndexed  *prometheus.CounterVec
	dirsIndexed   prometheus.Counter
	errors        *prometheus.CounterVec
	probeFailures prometheus.Counter
	scanDuration  prometheus.Histogram
	scanning      prometheus.Gauge
	lastSuccess   prometheus.Gauge
	watched       prometheus.Gauge
	files         *prometheus.GaugeVec
	dirs          prometheus.Gauge
	sizes         map[string]map[fileLabels]int
}

func newIndexMetrics() *indexMetrics {
	reg := prometheus.NewRegistry()
	m := &indexMetrics{registry: reg, sizes: map[string]map[fileLabels]int{}}
	m.filesIndexed = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_indexer_files_indexed_total", Help: "Files successfully indexed into a new or unchanged valid sidecar by MIME type and stream codecs; includes reindexing and path-based probe fallbacks."}, []string{"mimetype", "audio", "video"})
	m.dirsIndexed = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_indexer_directories_indexed_total", Help: "Successful directory indexing passes with a new or unchanged valid sidecar."})
	m.errors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_indexer_errors_total", Help: "Indexing errors by operation and reason; paths are only logged."}, []string{"operation", "reason"})
	m.probeFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_indexer_probe_failures_total", Help: "Failed ffprobe executions or invalid ffprobe JSON results."})
	m.scanDuration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "slopify_indexer_scan_duration_seconds", Help: "Completed tree scan durations, including scans with errors.", Buckets: []float64{.1, 1, 5, 15, 60, 300, 900, 3600}})
	m.scanning = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_scan_in_progress", Help: "Whether a tree scan is currently running."})
	m.lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_last_successful_scan_timestamp_seconds", Help: "Unix timestamp of the last complete library scan without indexing errors; zero until one succeeds."})
	m.watched = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_watched_directories", Help: "Number of directories currently watched for filesystem changes."})
	m.files = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "slopify_indexer_files", Help: "Files in successfully loaded or written sidecars by MIME type and stream codecs; rebuilt during initial scanning."}, []string{"mimetype", "audio", "video"})
	m.dirs = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_directories", Help: "Directories with successfully loaded or written sidecars, including empty sidecars; rebuilt during initial scanning."})
	reg.MustRegister(m.filesIndexed, m.dirsIndexed, m.errors, m.probeFailures, m.scanDuration, m.scanning, m.lastSuccess, m.watched, m.files, m.dirs)
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

func (m *indexMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

type fileLabels struct{ mime, audio, video string }

func streamLabel(codecs []string, legacy string, known bool) string {
	if len(codecs) > 1 {
		return "multiple"
	}
	if len(codecs) == 1 {
		return codecs[0]
	}
	if legacy != "" && legacy != "none" {
		return legacy
	}
	if !known {
		return "unknown"
	}
	return ""
}

func countFiles(entries []sidecar.Entry) map[fileLabels]int {
	counts := map[fileLabels]int{}
	for _, e := range entries {
		_, mime, ok := media.Classify(e.Name)
		if !ok {
			mime = "application/octet-stream"
		}
		known := e.ProbeError == "" && e.ProbeVersion == fileProbeVersion(e.Kind)
		labels := fileLabels{mime,
			streamLabel(e.AudioCodecs, e.Codec, known),
			streamLabel(e.VideoCodecs, e.VideoCodec, known)}
		counts[labels]++
	}
	return counts
}

func (m *indexMetrics) adjust(counts map[fileLabels]int, sign float64) {
	for labels, n := range counts {
		m.files.WithLabelValues(labels.mime, labels.audio, labels.video).Add(sign * float64(n))
	}
}

func (m *indexMetrics) remember(dir string, entries []sidecar.Entry) {
	if m == nil {
		return
	}
	m.adjust(m.sizes[dir], -1)
	counts := countFiles(entries)
	m.sizes[dir] = counts
	m.adjust(counts, 1)
	m.dirs.Set(float64(len(m.sizes)))
}

func (m *indexMetrics) forgetSidecar(dir string) {
	if m == nil {
		return
	}
	m.adjust(m.sizes[dir], -1)
	delete(m.sizes, dir)
	m.dirs.Set(float64(len(m.sizes)))
}

func (m *indexMetrics) forget(dir string) {
	if m == nil {
		return
	}
	for d, counts := range m.sizes {
		if d == dir || strings.HasPrefix(d, dir+"/") {
			m.adjust(counts, -1)
			delete(m.sizes, d)
		}
	}
	m.dirs.Set(float64(len(m.sizes)))
}

func (m *indexMetrics) indexed(entries []sidecar.Entry) {
	if m == nil {
		return
	}
	for labels, n := range countFiles(entries) {
		m.filesIndexed.WithLabelValues(labels.mime, labels.audio, labels.video).Add(float64(n))
	}
	m.dirsIndexed.Inc()
}

func (m *indexMetrics) scanStart() func(bool) {
	if m == nil {
		return func(bool) {}
	}
	start := time.Now()
	m.scanning.Set(1)
	return func(success bool) {
		m.scanning.Set(0)
		m.scanDuration.Observe(time.Since(start).Seconds())
		if success {
			m.lastSuccess.SetToCurrentTime()
		}
	}
}
