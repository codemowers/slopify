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
	tagged        *prometheus.GaugeVec
	dirs          prometheus.Gauge
	sizes         map[string]map[fileLabels]int
	tagSizes      map[string]map[tagLabels]int
}

func newIndexMetrics() *indexMetrics {
	reg := prometheus.NewRegistry()
	m := &indexMetrics{registry: reg, sizes: map[string]map[fileLabels]int{}, tagSizes: map[string]map[tagLabels]int{}}
	m.filesIndexed = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_indexer_files_indexed_total", Help: "Files successfully indexed into a new or unchanged valid sidecar by MIME type and stream codecs; includes reindexing and path-based probe fallbacks."}, []string{"mimetype", "audio", "video"})
	m.dirsIndexed = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_indexer_directories_indexed_total", Help: "Successful directory indexing passes with a new or unchanged valid sidecar."})
	m.errors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_indexer_errors_total", Help: "Indexing errors by operation and reason; paths are only logged."}, []string{"operation", "reason"})
	m.probeFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "slopify_indexer_probe_failures_total", Help: "Failed ffprobe executions or invalid ffprobe JSON results."})
	m.scanDuration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "slopify_indexer_scan_duration_seconds", Help: "Completed tree scan durations, including scans with errors.", Buckets: []float64{.1, 1, 5, 15, 60, 300, 900, 3600}})
	m.scanning = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_scan_in_progress", Help: "Whether a tree scan is currently running."})
	m.lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_last_successful_scan_timestamp_seconds", Help: "Unix timestamp of the last complete library scan without indexing errors; zero until one succeeds."})
	m.watched = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_watched_directories", Help: "Number of directories currently watched for filesystem changes."})
	m.files = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "slopify_indexer_files", Help: "Files in successfully loaded or written sidecars by MIME type and stream codecs; rebuilt during initial scanning."}, []string{"mimetype", "audio", "video"})
	m.tagged = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "slopify_indexer_tagged_files", Help: "Files whose current probe found a well-known tag, by container MIME type and tag; rebuilt during initial scanning."}, []string{"mimetype", "tag"})
	m.dirs = prometheus.NewGauge(prometheus.GaugeOpts{Name: "slopify_indexer_directories", Help: "Directories with successfully loaded or written sidecars, including empty sidecars; rebuilt during initial scanning."})
	reg.MustRegister(m.filesIndexed, m.dirsIndexed, m.errors, m.probeFailures, m.scanDuration, m.scanning, m.lastSuccess, m.watched, m.files, m.tagged, m.dirs)
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

type tagLabels struct{ mime, tag string }

// countTags reports which well-known tags each file has. The tag label is a
// fixed list, so cardinality is bounded by MIME types times these names.
func countTags(entries []sidecar.Entry) map[tagLabels]int {
	counts := map[tagLabels]int{}
	for _, e := range entries {
		_, mime, ok := media.Classify(e.Name)
		if !ok {
			mime = "application/octet-stream"
		}
		var t sidecar.Info
		if e.Tags != nil {
			t = *e.Tags
		}
		var c sidecar.Credits
		if e.Credits != nil {
			c = *e.Credits
		}
		for _, tag := range [...]struct {
			name    string
			present bool
		}{
			{"title", t.Title != ""}, {"artist", t.Artist != ""}, {"album_artist", t.AlbumArtist != ""},
			{"album", t.Album != ""}, {"genre", t.Genre != ""}, {"year", t.Year != 0}, {"track", t.Track != 0}, {"disc", t.Disc != 0},
			{"composer", c.Composer != ""}, {"copyright", c.Copyright != ""}, {"license", c.License != ""}, {"label", c.Label != ""},
			{"isrc", c.ISRC != ""}, {"acoustid", c.AcoustID != ""},
			{"musicbrainz_recording", c.Recording != ""}, {"musicbrainz_track", c.ReleaseTrack != ""},
			{"musicbrainz_release", c.Release != ""}, {"musicbrainz_release_group", c.ReleaseGroup != ""},
			{"musicbrainz_work", c.Work != ""}, {"musicbrainz_artist", len(c.Artists) > 0},
			{"musicbrainz_album_artist", len(c.AlbumArtists) > 0},
		} {
			if tag.present {
				counts[tagLabels{mime, tag.name}]++
			}
		}
	}
	return counts
}

func (m *indexMetrics) adjust(counts map[fileLabels]int, tags map[tagLabels]int, sign float64) {
	for labels, n := range counts {
		m.files.WithLabelValues(labels.mime, labels.audio, labels.video).Add(sign * float64(n))
	}
	for labels, n := range tags {
		m.tagged.WithLabelValues(labels.mime, labels.tag).Add(sign * float64(n))
	}
}

func (m *indexMetrics) remember(dir string, entries []sidecar.Entry) {
	if m == nil {
		return
	}
	m.adjust(m.sizes[dir], m.tagSizes[dir], -1)
	counts, tags := countFiles(entries), countTags(entries)
	m.sizes[dir], m.tagSizes[dir] = counts, tags
	m.adjust(counts, tags, 1)
	m.dirs.Set(float64(len(m.sizes)))
}

func (m *indexMetrics) forgetSidecar(dir string) {
	if m == nil {
		return
	}
	m.adjust(m.sizes[dir], m.tagSizes[dir], -1)
	delete(m.sizes, dir)
	delete(m.tagSizes, dir)
	m.dirs.Set(float64(len(m.sizes)))
}

func (m *indexMetrics) forget(dir string) {
	if m == nil {
		return
	}
	for d, counts := range m.sizes {
		if d == dir || strings.HasPrefix(d, dir+"/") {
			m.adjust(counts, m.tagSizes[d], -1)
			delete(m.sizes, d)
			delete(m.tagSizes, d)
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
