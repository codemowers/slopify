// Command streamer serves original files with signed HTTP range requests.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/signedurl"
)

type streamer struct {
	root   *os.Root
	signer *signedurl.Signer
}

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()
	root, err := os.OpenRoot(httpx.EnvOr("MEDIA_ROOT", "/media"))
	if err != nil {
		slog.Error("opening media", "err", err)
		os.Exit(1)
	}
	defer root.Close()
	s := &streamer{root: root, signer: signedurl.New(httpx.Env("STREAM_SIGNING_KEY"))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /stream/original", s.stream)
	if err := httpx.Serve(ctx, mux); err != nil {
		slog.Error("serving", "err", err)
		os.Exit(1)
	}
}
func (s *streamer) stream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if s.signer.Verify(q) != nil || q.Get("format") != "original" {
		http.Error(w, "invalid original URL", http.StatusForbidden)
		return
	}
	p, err := media.CleanPath(q.Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	_, mime, ok := media.Classify(p)
	if !ok {
		http.Error(w, "unsupported media", http.StatusBadRequest)
		return
	}
	f, err := s.root.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "cannot open media", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat media", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))
	http.ServeContent(w, r, "", info.ModTime(), f)
}
