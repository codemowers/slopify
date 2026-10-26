package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/auth"
)

// playlist exports a snapshot, with originals accessible without OIDC cookies.
// Use a configured origin so client-controlled Host headers cannot redirect links.
func (s *store) playlist(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimRight(os.Getenv("PUBLIC_ORIGIN"), "/")
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		http.Error(w, "playlist export requires PUBLIC_ORIGIN", http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	var entries []Entry
	if p := s.players[queueID(auth.Subject(r))]; p != nil {
		entries = slices.Clone(p.Queue)
	}
	s.mu.Unlock()
	if len(entries) == 0 {
		http.Error(w, "Your queue is empty", http.StatusConflict)
		return
	}
	pls := strings.HasSuffix(r.URL.Path, ".pls")
	ext, mime := "m3u", "audio/x-mpegurl"
	if pls {
		ext, mime = "pls", "audio/x-scpls"
	}
	w.Header().Set("Content-Type", mime+"; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"slopify."+ext+"\"")
	w.Header().Set("Cache-Control", "no-store")
	if pls {
		fmt.Fprintln(w, "[playlist]")
	} else {
		fmt.Fprintln(w, "#EXTM3U")
	}
	clean := strings.NewReplacer("\r", " ", "\n", " ")
	for i, e := range entries {
		title := e.Title
		if title == "" {
			title = e.Path
		}
		if e.Artist != "" {
			title = e.Artist + " - " + title
		}
		duration := e.Duration
		if duration <= 0 {
			duration = -1
		}
		link := origin + "/stream/original?" + s.signer.Query(e.Path, "original", 12*time.Hour).Encode()
		if pls {
			fmt.Fprintf(w, "File%d=%s\nTitle%d=%s\nLength%d=%.0f\n", i+1, link, i+1, clean.Replace(title), i+1, duration)
		} else {
			fmt.Fprintf(w, "#EXTINF:%.0f,%s\n%s\n", duration, clean.Replace(title), link)
		}
	}
	if pls {
		fmt.Fprintf(w, "NumberOfEntries=%d\nVersion=2\n", len(entries))
	}
}
