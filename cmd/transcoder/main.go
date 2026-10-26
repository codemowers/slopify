// Command transcoder owns one named codec/bitrate profile and a bounded PVC
// cache. Originals arrive by signed HTTP, with no local media mount.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/profiles"
	"github.com/codemowers/slopify/internal/signedurl"
)

type transcoder struct {
	profile profiles.Profile
	signer  *signedurl.Signer
	source  string
	cache   *diskCache
	slots   chan struct{}
}

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()
	profile, ok := profiles.Lookup(httpx.Env("TRANSCODE_PROFILE"))
	if !ok {
		slog.Error("unsupported profile")
		os.Exit(1)
	}
	limit, err := strconv.ParseInt(httpx.EnvOr("TRANSCODE_CACHE_MAX_BYTES", "25000000000"), 10, 64)
	if err != nil {
		slog.Error("invalid cache limit")
		os.Exit(1)
	}
	var cache *diskCache
	if limit > 0 {
		cache, err = openCache(httpx.EnvOr("TRANSCODE_CACHE_ROOT", "/cache"), limit)
		if err != nil {
			slog.Error("opening cache", "err", err)
			os.Exit(1)
		}
		defer cache.root.Close()
	}
	slots, err := strconv.Atoi(httpx.EnvOr("TRANSCODE_CONCURRENCY", "2"))
	if err != nil || slots < 1 {
		slog.Error("invalid concurrency")
		os.Exit(1)
	}
	s := &transcoder{profile: profile, signer: signedurl.New(httpx.Env("STREAM_SIGNING_KEY")), source: strings.TrimRight(httpx.EnvOr("ORIGINALS_URL", "http://slopify-streamer"), "/"), cache: cache, slots: make(chan struct{}, slots)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET "+profiles.Path(profile.Name), s.stream)
	if profile.Name == "video-aac-192" {
		copyStream := *s
		copyStream.profile, _ = profiles.Lookup("video-copy")
		mux.HandleFunc("GET "+profiles.Path("video-copy"), copyStream.stream)
	}
	if err := httpx.Serve(ctx, mux); err != nil {
		slog.Error("serving", "err", err)
		os.Exit(1)
	}
}
func (s *transcoder) originalURL(path string) string {
	return s.source + profiles.Path("original") + "?" + s.signer.Query(path, "original", 12*time.Hour).Encode()
}
func (s *transcoder) sourceTag(ctx context.Context, u string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := http.DefaultClient.Do(request.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("original status %d", response.StatusCode)
	}
	tag := response.Header.Get("ETag")
	if tag == "" {
		return "", fmt.Errorf("original has no version tag")
	}
	return tag, nil
}
func (s *transcoder) stream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if s.signer.Verify(q) != nil || q.Get("format") != s.profile.Name {
		http.Error(w, "profile signature mismatch", http.StatusForbidden)
		return
	}
	path, err := media.CleanPath(q.Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if _, _, ok := media.Classify(path); !ok {
		http.Error(w, "unsupported media", http.StatusBadRequest)
		return
	}
	start := 0.0
	if value := q.Get("start"); value != "" {
		start, err = strconv.ParseFloat(value, 64)
		if err != nil || start < 0 || math.IsNaN(start) || math.IsInf(start, 0) {
			http.Error(w, "invalid start", http.StatusBadRequest)
			return
		}
	}
	original := s.originalURL(path)
	tag, err := s.sourceTag(r.Context(), original)
	if err != nil {
		http.Error(w, "original unavailable", http.StatusBadGateway)
		return
	}
	sum := sha256.Sum256([]byte("v1\x00" + s.profile.Name + "\x00" + strings.Join(s.profile.Args, "\x00") + "\x00" + path + "\x00" + tag))
	key := hex.EncodeToString(sum[:]) + ".cache"
	w.Header().Set("Content-Type", s.profile.MIME)
	if start == 0 && s.cache != nil {
		for {
			if f, release, ok := s.cache.open(key); ok {
				defer release()
				info, err := f.Stat()
				if err != nil {
					http.Error(w, "cache unavailable", 500)
					return
				}
				w.Header().Set("X-Slopify-Cache", "hit")
				w.Header().Set("Cache-Control", "private, max-age=86400")
				http.ServeContent(w, r, "", info.ModTime(), f)
				return
			}
			if r.Method == http.MethodHead {
				w.Header().Set("X-Slopify-Cache", "miss")
				w.WriteHeader(http.StatusOK)
				return
			}
			done, owner := s.cache.begin(key)
			if owner {
				defer s.cache.end(key)
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-done:
			}
		}
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Slopify-Cache", "miss")
	var cached *cacheWriter
	if start == 0 && s.cache != nil {
		cached, err = s.cache.writer(key)
		if err != nil {
			slog.Warn("cache bypass", "err", err)
		} else {
			defer cached.abort()
		}
	}
	args := []string{"-nostdin", "-loglevel", "error"}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
		if s.profile.Video {
			// Keep audio preroll with copied video; both start at the preceding keyframe.
			args = append(args, "-noaccurate_seek")
		}
	}
	args = append(args, "-i", original)
	if s.profile.Video {
		args = append(args, "-map", "0:v:0", "-map", "0:a:0?", "-avoid_negative_ts", "make_zero")
	} else {
		args = append(args, "-map", "0:a:0", "-vn")
	}
	args = append(args, "-map_metadata", "-1", "-threads", "1")
	args = append(args, s.profile.Args...)
	if s.profile.Video && q.Get("videoCodec") == "hevc" {
		args = append(args, "-tag:v", "hvc1")
	}
	args = append(args, "pipe:1")
	slog.Info("stream starting", "path", path, "profile", s.profile.Name, "start", start)
	cmd := exec.CommandContext(r.Context(), "ffmpeg", args...)
	stderr := &limitedLog{}
	cmd.Stderr = stderr
	var output io.Writer = flushWriter{w}
	if cached != nil {
		output = io.MultiWriter(output, cached)
	}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if r.Context().Err() == nil {
			slog.Error("transcoding failed", "path", path, "profile", s.profile.Name, "err", err, "stderr", strings.ReplaceAll(stderr.String(), original, "<signed original>"))
		}
		return
	}
	if cached != nil {
		current, err := s.sourceTag(r.Context(), original)
		if err != nil || current != tag {
			return
		}
		if err := cached.commit(); err != nil {
			slog.Warn("publishing cache", "err", err)
		}
	}
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}

type limitedLog struct{ bytes.Buffer }

func (b *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 2048 {
		b.Buffer.Write(p[:min(n, 2048-b.Len())])
	}
	return n, nil
}
