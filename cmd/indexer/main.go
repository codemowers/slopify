// Command indexer is the only writer of library metadata. It scans the media
// tree, follows changes with inotify and keeps a sidecar in every directory
// that holds media, then tells readers which directories changed.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/codemowers/slopify/internal/events"
	"github.com/codemowers/slopify/internal/heuristics"
	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/sidecar"
)

const settle = time.Second

type indexer struct {
	probeWorkers int
	reprobe      bool
	metrics      *indexMetrics
	dir          string
	root         *os.Root
	watcher      *fsnotify.Watcher
	hub          *events.Hub[events.Event]
	watched      map[string]bool
	pending      map[string]time.Time
}

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()

	dir := httpx.EnvOr("MEDIA_ROOT", "/media")
	root, err := os.OpenRoot(dir)
	if err != nil {
		slog.Error("opening media root", "err", err)
		os.Exit(1)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("creating watcher", "err", err)
		os.Exit(1)
	}
	defer watcher.Close()

	ix := &indexer{
		dir: dir, root: root, watcher: watcher, hub: events.NewHub[events.Event](),
		watched: map[string]bool{}, pending: map[string]time.Time{}, metrics: newIndexMetrics(), probeWorkers: probeWorkerCount(), reprobe: os.Getenv("INDEXER_REPROBE") == "true",
	}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/events", ix.hub)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	// Serving events is ready independently of the background library scan.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {})

	// Metrics have their own internal listener, never the application router.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", ix.metrics.handler())
	errc := make(chan error, 3)
	go func() { errc <- httpx.Serve(ctx, mux) }()
	go func() { errc <- httpx.ServeAddr(ctx, httpx.EnvOr("METRICS_LISTEN_ADDR", ":9090"), metricsMux) }()
	go func() {
		// Individual directory failures are reported by the scan, not fatal to
		// HTTP/SSE or to indexing the remainder of the library.
		_ = ix.scanTree(".")
		errc <- ix.run(ctx)
	}()
	if err := <-errc; err != nil {
		slog.Error("indexer failed", "err", err)
		os.Exit(1)
	}
}

// scanTree keeps walking after unreadable directories or failed sidecar writes.
// Each directory is read once, so a permission failure has one log summary.
func (ix *indexer) scanTree(dir string) error {
	finish := ix.metrics.scanStart()
	start := time.Now()
	pending := []string{dir}
	failures, visited := 0, 0
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		children, err := ix.indexDirectory(current, true)
		visited++
		if err != nil {
			failures++
		}
		pending = append(pending, children...)
	}
	finish(failures == 0 && dir == ".")
	if failures > 0 {
		slog.Warn("scan completed with errors", "dir", dir, "directories", visited, "failed_directories", failures, "took", time.Since(start))
		return fmt.Errorf("scan of %q: %d directories had errors", dir, failures)
	}
	slog.Info("scan complete", "dir", dir, "directories", visited, "took", time.Since(start))
	return nil
}

func (ix *indexer) indexDir(dir string) error {
	_, err := ix.indexDirectory(dir, false)
	return err
}

func (ix *indexer) indexDirectory(dir string, watch bool) ([]string, error) {
	report := &directoryReport{dir: dir, metrics: ix.metrics}
	defer report.finish()
	if watch && !ix.watched[dir] {
		if err := ix.watcher.Add(filepath.Join(ix.dir, dir)); err != nil {
			report.add("watch", dir, err)
			report.failed = true
		} else {
			ix.watched[dir] = true
			if ix.metrics != nil {
				ix.metrics.watched.Set(float64(len(ix.watched)))
			}
		}
	}
	entries, err := fs.ReadDir(ix.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		ix.metrics.forget(dir)
		return nil, report.result()
	}
	if err != nil {
		report.add("read", dir, err)
		report.failed = true
		return nil, report.result()
	}
	var children []string
	for _, d := range entries {
		if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			children = append(children, path.Join(dir, d.Name()))
		}
	}
	old, err := sidecar.Read(ix.root, dir)
	hadSidecar := err == nil
	repairSidecar := false
	if hadSidecar {
		ix.metrics.remember(dir, old.Entries)
	} else if errors.Is(err, fs.ErrNotExist) {
		ix.metrics.forgetSidecar(dir)
		old = &sidecar.Sidecar{}
	} else {
		report.add("read", sidecar.Path(dir), err)
		var syntax *json.SyntaxError
		var shape *json.UnmarshalTypeError
		if !errors.As(err, &syntax) && !errors.As(err, &shape) {
			// Preserve the old sidecar on permission, I/O or other read errors.
			report.failed = true
			return children, report.result()
		}
		ix.metrics.forgetSidecar(dir)
		repairSidecar = true
		old = &sidecar.Sidecar{}
	}
	previous := map[string]sidecar.Entry{}
	for _, e := range old.Entries {
		previous[e.Name] = e
	}
	next := &sidecar.Sidecar{Version: sidecar.Version, Entries: []sidecar.Entry{}}
	incomplete := false
	// Only probing runs concurrently; sidecar writes, error summaries and
	// metric accounting stay on the indexing goroutine.
	files := []fs.DirEntry{}
	for _, d := range entries {
		_, _, ok := media.Classify(d.Name())
		if ok && d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".") {
			files = append(files, d)
		}
	}
	type result struct {
		entry      *sidecar.Entry
		err        error
		operation  string
		incomplete bool
	}
	results := make([]result, len(files))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(max(ix.probeWorkers, 1), max(len(files), 1)) {
		workers.Go(func() {
			for i := range jobs {
				d := files[i]
				rel := path.Join(dir, d.Name())
				info, err := d.Info()
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					results[i] = result{err: err, operation: "read", incomplete: true}
					continue
				}
				kind, _, _ := media.Classify(d.Name())
				e := sidecar.Entry{Name: d.Name(), Kind: kind, Size: info.Size(), ModTime: info.ModTime().UTC()}
				e.Guess, e.Confidence = heuristics.Guess(rel)
				if old, ok := previous[d.Name()]; ok && !ix.reprobe && old.Size == e.Size && old.ModTime.Equal(e.ModTime) && old.ProbeVersion == fileProbeVersion(kind) && (old.Tags == nil || !old.Tags.HasCorruptText() || old.TagEncodingVersion == 1) {
					e.Tags, e.Duration, e.Codec = old.Tags, old.Duration, old.Codec
					e.Bitrate, e.SampleRate, e.Channels, e.Bits = old.Bitrate, old.SampleRate, old.Channels, old.Bits
					e.ProbeVersion, e.ProbeError = old.ProbeVersion, old.ProbeError
					e.TagSource = old.TagSource
					e.TagEncodingVersion = old.TagEncodingVersion
					e.VideoCodec = old.VideoCodec
					e.AudioCodecs, e.VideoCodecs = old.AudioCodecs, old.VideoCodecs
				} else {
					metadata, probeErr := ix.probe(rel)
					e.Tags, e.Duration, e.Codec = metadata.Tags, metadata.Duration, metadata.Codec
					e.Bitrate, e.SampleRate, e.Channels, e.Bits = metadata.Bitrate, metadata.SampleRate, metadata.Channels, metadata.Bits
					e.ProbeVersion = fileProbeVersion(kind)
					e.TagSource = metadata.TagSource
					e.TagEncodingVersion = 1
					e.VideoCodec = metadata.VideoCodec
					e.AudioCodecs, e.VideoCodecs = metadata.AudioCodecs, metadata.VideoCodecs
					if probeErr != nil {
						e.ProbeError = probeErr.Error()
						operation := "probe"
						var pe *probeError
						if !errors.As(probeErr, &pe) {
							operation = "read"
						}
						results[i].err, results[i].operation = probeErr, operation
					}
				}
				results[i].entry = &e
			}
		})
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	for i, result := range results {
		if result.err != nil {
			report.add(result.operation, path.Join(dir, files[i].Name()), result.err)
		}
		incomplete = incomplete || result.incomplete
		if result.entry != nil {
			next.Entries = append(next.Entries, *result.entry)
		}
	}

	report.files = len(next.Entries)
	if incomplete {
		// A failed stat must not remove an existing entry from its sidecar.
		report.failed = true
		return children, report.result()
	}
	if len(next.Entries) == 0 && len(old.Entries) == 0 && !hadSidecar && !repairSidecar {
		return children, report.result()
	}
	old.Version = sidecar.Version
	if repairSidecar || !reflect.DeepEqual(old, next) {
		if err := sidecar.Write(ix.root, dir, next); err != nil {
			report.add("write", sidecar.Path(dir), err)
			report.failed = true
			return children, report.result()
		}
		report.updated = true
		ix.hub.Publish(events.Event{Type: events.Changed, Path: dir})
	}
	ix.metrics.remember(dir, next.Entries)
	ix.metrics.indexed(next.Entries)
	return children, report.result()
}

type probeResult struct {
	Format struct {
		Duration string            `json:"duration"`
		Bitrate  string            `json:"bit_rate"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType     string            `json:"codec_type"`
		CodecName     string            `json:"codec_name"`
		Bitrate       string            `json:"bit_rate"`
		SampleRate    string            `json:"sample_rate"`
		Channels      int               `json:"channels"`
		BitsPerRaw    string            `json:"bits_per_raw_sample"`
		BitsPerSample int               `json:"bits_per_sample"`
		SampleFormat  string            `json:"sample_fmt"`
		Tags          map[string]string `json:"tags"`
	} `json:"streams"`
}

// probe reads tags, duration and audio codec with ffprobe. Untagged or
// unparsable files get nil tags: they are still indexed by their path.
func (ix *indexer) probe(rel string) (probeMetadata, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// ffprobe reads the descriptor opened through os.Root; symlinks cannot
	// take probing outside the library. Stderr is retained, with a hard cap.
	out, err := ix.probeCommand(ctx, rel)
	if err != nil {
		return probeMetadata{}, err
	}
	var pr probeResult
	if err := json.Unmarshal(out, &pr); err != nil {
		return probeMetadata{}, &probeError{err: fmt.Errorf("invalid ffprobe JSON: %w", err)}
	}

	var metadata probeMetadata
	kind, _, _ := media.Classify(rel)
	if kind == media.Video {
		metadata.TagSource = "container"
	}
	metadata.Duration, _ = strconv.ParseFloat(pr.Format.Duration, 64)
	metadata.Bitrate, _ = strconv.ParseInt(pr.Format.Bitrate, 10, 64)
	// Containers keep tags at the format level (ID3, MP4) or on the stream
	// (Ogg); keys vary in case between formats.
	tags := map[string]string{}
	codec := ""
	for _, s := range pr.Streams {
		streamCodec := s.CodecName
		if streamCodec == "" {
			streamCodec = "unknown"
		}
		switch s.CodecType {
		case "audio":
			metadata.AudioCodecs = append(metadata.AudioCodecs, streamCodec)
		case "video":
			metadata.VideoCodecs = append(metadata.VideoCodecs, streamCodec)
		}
		if s.CodecType == "video" && metadata.VideoCodec == "" {
			metadata.VideoCodec = s.CodecName
		}
		if s.CodecType == "audio" && codec == "" {
			codec = s.CodecName
			if bitrate, _ := strconv.ParseInt(s.Bitrate, 10, 64); bitrate > 0 {
				metadata.Bitrate = bitrate
			}
			metadata.SampleRate, _ = strconv.Atoi(s.SampleRate)
			metadata.Channels = s.Channels
			raw, _ := strconv.Atoi(s.BitsPerRaw)
			if raw == 0 {
				raw = s.BitsPerSample
			}
			switch strings.TrimSuffix(s.SampleFormat, "p") {
			case "u8", "s16":
				metadata.Bits = 16
			case "s32", "s64":
				metadata.Bits = 32
				if raw > 0 && raw <= 24 {
					metadata.Bits = 24
				}
				if raw > 0 && raw <= 16 {
					metadata.Bits = 16
				}
			default:
				metadata.Bits = 24
			}
			if kind != media.Video {
				for k, v := range s.Tags {
					tags[strings.ToLower(k)] = v
				}
			}
		}
	}
	for k, v := range pr.Format.Tags {
		tags[strings.ToLower(k)] = v
	}
	if codec == "" {
		codec = "none"
	}
	clean := func(k string) string { return strings.TrimSpace(strings.Trim(tags[k], "\x00")) }
	leadingInt := func(k string) int {
		v := clean(k)
		end := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' })
		if end >= 0 {
			v = v[:end]
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	info := sidecar.Info{
		Artist: clean("artist"), AlbumArtist: cmp.Or(clean("album_artist"), clean("albumartist")),
		Album: clean("album"), Title: clean("title"), Genre: clean("genre"),
		Year: cmp.Or(leadingInt("date"), leadingInt("year")), Track: leadingInt("track"),
		Disc: cmp.Or(leadingInt("disc"), leadingInt("discnumber")),
	}
	metadata.Codec = codec
	if !info.Empty() {
		metadata.Tags = &info
	}
	return metadata, nil
}

func (ix *indexer) rel(abs string) (string, bool) {
	r, err := filepath.Rel(ix.dir, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, "../") {
		return "", false
	}
	r = filepath.ToSlash(r)
	for _, seg := range strings.Split(r, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return "", false // sidecars and other hidden files
		}
	}
	return r, true
}

func (ix *indexer) run(ctx context.Context) error {
	tick := time.NewTicker(settle / 4)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-ix.watcher.Errors:
			if !ok {
				return errors.New("watcher closed")
			}
			if ix.metrics != nil {
				ix.metrics.errors.WithLabelValues("watch", errorReason(err, "")).Inc()
			}
			if !errors.Is(err, fsnotify.ErrEventOverflow) {
				slog.Error("filesystem watcher error", "err", err)
				continue
			}
			slog.Warn("inotify queue overflowed, rescanning library")
			_ = ix.scanTree(".")
		case ev, ok := <-ix.watcher.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			if err := ix.handle(ev); err != nil {
				r := &directoryReport{dir: path.Dir(ev.Name), metrics: ix.metrics, failed: true}
				r.add("read", ev.Name, err)
				r.finish()
			}
		case now := <-tick.C:
			for dir, due := range ix.pending {
				if now.Before(due) {
					continue
				}
				delete(ix.pending, dir)
				_ = ix.indexDir(dir)
			}
		}
	}
}

func (ix *indexer) handle(ev fsnotify.Event) error {
	p, ok := ix.rel(ev.Name)
	if !ok || p == "." {
		return nil
	}
	parent := path.Dir(p)
	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		if ix.watched[p] {
			// A directory left: forget its subtree. A rename shows up again
			// as a Create at the destination.
			for d := range ix.watched {
				if d == p || strings.HasPrefix(d, p+"/") {
					delete(ix.watched, d)
					delete(ix.pending, d)
					_ = ix.watcher.Remove(filepath.Join(ix.dir, d))
				}
			}
			ix.metrics.forget(p)
			if ix.metrics != nil {
				ix.metrics.watched.Set(float64(len(ix.watched)))
			}
			ix.hub.Publish(events.Event{Type: events.Removed, Path: p})
			return nil
		}
	case ev.Has(fsnotify.Create):
		fi, err := ix.root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if fi.IsDir() {
			_ = ix.scanTree(p)
			return nil
		}
	}
	if ev.Has(fsnotify.Chmod) && ix.watched[p] {
		ix.pending[p] = time.Now().Add(settle)
		return nil
	}
	// Files settle before reindexing so a copy in progress is read once.
	ix.pending[parent] = time.Now().Add(settle)
	return nil
}

const probeVersion = 2

type probeMetadata struct {
	AudioCodecs, VideoCodecs   []string
	TagSource                  string
	Tags                       *sidecar.Info
	Duration                   float64
	Codec                      string
	VideoCodec                 string
	Bitrate                    int64
	SampleRate, Channels, Bits int
}

func probeWorkerCount() int {
	n, err := strconv.Atoi(httpx.EnvOr("INDEXER_PROBE_WORKERS", "2"))
	if err != nil {
		return 2
	}
	return min(max(n, 1), 16)
}

func fileProbeVersion(kind media.Kind) int {
	if kind == media.Video {
		return 4
	}
	return probeVersion
}
