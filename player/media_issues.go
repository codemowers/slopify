package main

// Damaged originals: one place logs and counts what the decoders worked around or
// could not get past, so that problem files can be found from speaker logs and
// metrics. Logs name the library path only, never the signed URL.

import (
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/codemowers/slopify/player/internal/audiodec"
	"github.com/prometheus/client_golang/prometheus"
)

// mediaUndecodable is the issue kind for an original the decoder stopped on.
const mediaUndecodable = "undecodable"

var metricMediaIssues = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_audio_media_issues_total",
	Help: "Damage found in originals while decoding: resync (data skipped, decoding resumed), checksum (frames failed their CRC, played as silence), truncated (file ends early) or undecodable (decoding stopped)."},
	append(append([]string{}, audioLabelNames...), "codec", "kind"))

func init() { nativeMetricsRegistry.MustRegister(metricMediaIssues) }

// mediaPath is the library path of a signed original URL, without its signature,
// expiry or anything else from the query.
func mediaPath(signed string) string {
	u, err := url.Parse(signed)
	if err != nil {
		return ""
	}
	return u.Query().Get("path")
}

// Retries and prefetching decode the same damage again: count each occurrence, but
// log it once while it is remembered. The memory is bounded.
var mediaIssueLog = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

const mediaIssueLogMemory = 1024

func firstSighting(key string) bool {
	mediaIssueLog.Lock()
	defer mediaIssueLog.Unlock()
	if _, ok := mediaIssueLog.seen[key]; ok {
		return false
	}
	if len(mediaIssueLog.seen) >= mediaIssueLogMemory {
		clear(mediaIssueLog.seen)
	}
	mediaIssueLog.seen[key] = struct{}{}
	return true
}

// metricCodec keeps the codec label to a small, known set.
func metricCodec(codec string) string {
	switch c := strings.ToUpper(codec); c {
	case "FLAC", "MP3", "WAV", "VORBIS", "ALAC":
		return c
	}
	return "other"
}

// reportMediaIssue records damage a decoder worked around in an entry's original.
func reportMediaIssue(entry uint64, path string, i audiodec.Issue) {
	if metricsEnabled {
		metricMediaIssues.WithLabelValues(append(audio(stageInput, "slopify"), metricCodec(i.Codec), i.Kind)...).Inc()
	}
	if !firstSighting(path + "\x00" + i.Kind + "\x00" + strconv.FormatInt(i.Offset, 10)) {
		return
	}
	attrs := []any{"entry", entry, "path", path, "codec", i.Codec, "kind", i.Kind,
		"at", roundMillis(i.At), "offset", i.Offset}
	if i.Bytes > 0 {
		attrs = append(attrs, "skipped_bytes", i.Bytes)
	}
	if i.Missing > 0 {
		attrs = append(attrs, "missing_seconds", roundMillis(i.Missing))
	}
	if i.Err != nil {
		attrs = append(attrs, "err", i.Err)
	}
	slog.Warn("damaged original", attrs...)
}

// reportMediaFailure records an original the decoder could not get past.
func reportMediaFailure(entry uint64, path, codec string, at float64, err error) {
	if metricsEnabled {
		metricMediaIssues.WithLabelValues(append(audio(stageInput, "slopify"), metricCodec(codec), mediaUndecodable)...).Inc()
	}
	if !firstSighting(path + "\x00" + mediaUndecodable + "\x00" + err.Error()) {
		return
	}
	slog.Warn("undecodable original", "entry", entry, "path", path, "codec", codec, "at", roundMillis(at), "err", err)
}

func roundMillis(seconds float64) float64 { return math.Round(seconds*1000) / 1000 }
