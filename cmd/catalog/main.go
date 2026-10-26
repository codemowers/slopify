// Command catalog serves browse and search over the library. It holds no
// state of its own: it loads the indexer's sidecars into memory at startup
// and applies the indexer's change events.
package main

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemowers/slopify/internal/auth"
	"github.com/codemowers/slopify/internal/events"
	"github.com/codemowers/slopify/internal/heuristics"
	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/sidecar"
)

type Item struct {
	Bitrate    int64 `json:"bitrate,omitempty"`
	SampleRate int   `json:"sampleRate,omitempty"`
	Channels   int   `json:"channels,omitempty"`
	Bits       int   `json:"bits,omitempty"`

	Path        string     `json:"path"`
	Dir         string     `json:"dir"`
	Name        string     `json:"name"`
	Kind        media.Kind `json:"kind"`
	Mime        string     `json:"mime"`
	Size        int64      `json:"size"`
	Duration    float64    `json:"duration,omitempty"`
	Codec       string     `json:"codec,omitempty"`
	VideoCodec  string     `json:"videoCodec,omitempty"`
	Title       string     `json:"title"`
	Artist      string     `json:"artist,omitempty"`
	AlbumArtist string     `json:"albumArtist,omitempty"`
	Album       string     `json:"album,omitempty"`
	Genre       string     `json:"genre,omitempty"`
	Year        int        `json:"year,omitempty"`
	Track       int        `json:"track,omitempty"`
	Disc        int        `json:"disc,omitempty"`
	Tagged      bool       `json:"tagged"`
	Confidence  float64    `json:"confidence"`

	haystack string
}

type Dir struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Artist struct {
	Name   string  `json:"name"`
	Albums []Album `json:"albums"`
}

type Album struct {
	Dir    string `json:"dir"`
	Album  string `json:"album"`
	Artist string `json:"artist"`
	Year   int    `json:"year,omitempty"`
	Tracks int    `json:"tracks"`
}

// index is an immutable snapshot; changes build a new one.
type index struct {
	items    map[string][]Item // by directory
	children map[string][]Dir
	all      []Item
	albums   []Album
	artists  []Artist
}

type catalog struct {
	dirty atomic.Bool
	root  *os.Root
	mu    sync.Mutex
	dirs  map[string]*sidecar.Sidecar
	index atomic.Pointer[index]
}

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()

	root, err := os.OpenRoot(httpx.EnvOr("MEDIA_ROOT", "/media"))
	if err != nil {
		slog.Error("opening media root", "err", err)
		os.Exit(1)
	}
	c := &catalog{root: root}
	start := time.Now()
	if err := c.load(); err != nil {
		slog.Error("loading sidecars", "err", err)
		os.Exit(1)
	}
	slog.Info("catalog loaded", "items", len(c.index.Load().all), "took", time.Since(start))

	go c.batch(ctx)
	if url := os.Getenv("INDEXER_EVENTS_URL"); url != "" {
		go c.follow(ctx, url)
	}

	authorizer, err := auth.New(ctx)
	if err != nil {
		slog.Error("OIDC discovery", "err", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	api := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	api.HandleFunc("GET /api/catalog/browse", c.browse)
	api.HandleFunc("GET /api/catalog/search", c.search)
	api.HandleFunc("GET /api/catalog/tracks", c.tracks)
	api.HandleFunc("GET /api/catalog/item", c.item)
	api.HandleFunc("GET /api/catalog/albums", func(w http.ResponseWriter, r *http.Request) {
		albums := c.index.Load().albums
		if albums == nil {
			albums = []Album{}
		}
		httpx.JSON(w, albums)
	})
	api.HandleFunc("GET /api/catalog/artists", func(w http.ResponseWriter, r *http.Request) {
		artists := c.index.Load().artists
		if artists == nil {
			artists = []Artist{}
		}
		httpx.JSON(w, artists)
	})
	api.HandleFunc("GET /api/catalog/videos", func(w http.ResponseWriter, r *http.Request) {
		videos := []Item{}
		for _, it := range c.index.Load().all {
			if it.Kind == media.Video {
				videos = append(videos, it)
			}
		}
		httpx.JSON(w, videos)
	})
	mux.Handle("/api/catalog/", authorizer.Require(auth.Scope("slopify:library:read"), api))
	if err := httpx.Serve(ctx, mux); err != nil {
		slog.Error("serving", "err", err)
		os.Exit(1)
	}
}

// load reads every sidecar below the root.
func (c *catalog) load() error {
	dirs := map[string]*sidecar.Sidecar{}
	err := fs.WalkDir(c.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		s, err := sidecar.Read(c.root, p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			// The indexer rewrites invalid sidecars; skip until it has.
			slog.Warn("skipping unreadable sidecar", "dir", p, "err", err)
			return nil
		}
		dirs[p] = s
		return nil
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirs = dirs
	c.rebuild()
	return nil
}

// follow applies indexer events, reloading everything whenever the
// subscription (re)connects because events may have been missed meanwhile.
func (c *catalog) follow(ctx context.Context, url string) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := events.Subscribe(ctx, url, func() {
			backoff = time.Second
			slog.Info("indexer subscription connected; resynchronizing sidecars")
			if err := c.load(); err != nil {
				slog.Error("reloading sidecars", "err", err)
			}
		}, c.apply)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("indexer subscription lost", "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (c *catalog) apply(e events.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e.Type {
	case events.Removed:
		for d := range c.dirs {
			if d == e.Path || strings.HasPrefix(d, e.Path+"/") {
				delete(c.dirs, d)
			}
		}
	case events.Changed:
		s, err := sidecar.Read(c.root, e.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("reading changed sidecar", "dir", e.Path, "err", err)
			}
			delete(c.dirs, e.Path)
		} else {
			c.dirs[e.Path] = s
		}
	}
	c.dirty.Store(true)
}

// Coalesce bursts of directory changes into at most four rebuilds per second.
func (c *catalog) batch(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if c.dirty.Swap(false) {
				c.mu.Lock()
				c.rebuild()
				c.mu.Unlock()
			}
		}
	}
}

// rebuild derives a new index from c.dirs; callers hold c.mu.
func (c *catalog) rebuild() {
	ix := &index{items: map[string][]Item{}, children: map[string][]Dir{}}
	seen := map[string]bool{".": true}
	for dir, s := range c.dirs {
		for p := dir; !seen[p]; p = path.Dir(p) {
			seen[p] = true
			parent := path.Dir(p)
			ix.children[parent] = append(ix.children[parent], Dir{Name: path.Base(p), Path: p})
		}
		items := make([]Item, 0, len(s.Entries))
		for _, e := range s.Entries {
			if e.Kind == media.Video || (e.Tags != nil && e.Tags.HasCorruptText()) {
				e.Guess, e.Confidence = heuristics.Guess(path.Join(dir, e.Name))
			}
			d := e.Display()
			_, mime, _ := media.Classify(e.Name)
			p := path.Join(dir, e.Name)
			it := Item{
				Path: p, Dir: dir, Name: e.Name, Kind: e.Kind, Mime: mime, Size: e.Size,
				Duration: e.Duration, Codec: e.Codec, VideoCodec: e.VideoCodec, Bitrate: e.Bitrate, SampleRate: e.SampleRate, Channels: e.Channels, Bits: e.Bits,
				Title: d.Title, Artist: d.Artist, AlbumArtist: d.AlbumArtist, Album: d.Album,
				Genre: d.Genre, Year: d.Year, Track: d.Track, Disc: d.Disc,
				Tagged: e.Tags != nil && (e.Kind != media.Video || (e.TagSource == "container" && e.Tags.Title != "" && !media.TechnicalVideoTitle(e.Tags.Title))), Confidence: e.Confidence,
			}
			if it.Tagged {
				it.Confidence = 1
			}
			it.haystack = strings.ToLower(strings.Join([]string{p, d.Title, d.Artist, d.AlbumArtist, d.Album, d.Genre}, "\x00"))
			items = append(items, it)
		}
		slices.SortFunc(items, func(a, b Item) int {
			return cmp.Or(cmp.Compare(a.Disc, b.Disc), cmp.Compare(a.Track, b.Track), strings.Compare(a.Name, b.Name))
		})
		ix.items[dir] = items
		ix.all = append(ix.all, items...)
		if album := summarize(dir, items); album != nil {
			ix.albums = append(ix.albums, *album)
		}
	}
	for _, ds := range ix.children {
		slices.SortFunc(ds, func(a, b Dir) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	}
	slices.SortFunc(ix.all, func(a, b Item) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(ix.albums, func(a, b Album) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Artist), strings.ToLower(b.Artist)), cmp.Compare(a.Year, b.Year), strings.Compare(a.Album, b.Album))
	})
	byArtist := map[string]int{}
	for _, a := range ix.albums {
		i, ok := byArtist[a.Artist]
		if !ok {
			i = len(ix.artists)
			byArtist[a.Artist] = i
			ix.artists = append(ix.artists, Artist{Name: a.Artist})
		}
		ix.artists[i].Albums = append(ix.artists[i].Albums, a)
	}
	c.index.Store(ix)
}

// summarize treats a directory of audio as an album, named by its most common album and artist.
func summarize(dir string, items []Item) *Album {
	albums, artists := map[string]int{}, map[string]int{}
	a := &Album{Dir: dir}
	for _, it := range items {
		if it.Kind != media.Audio {
			continue
		}
		a.Tracks++
		albums[it.Album]++
		artists[cmp.Or(it.AlbumArtist, it.Artist)]++
		a.Year = max(a.Year, it.Year)
	}
	if a.Tracks == 0 {
		return nil
	}
	a.Album = cmp.Or(top(albums), path.Base(dir))
	a.Artist = top(artists)
	if len(artists) > 1 && artists[a.Artist] < a.Tracks {
		a.Artist = "Various Artists"
	}
	return a
}

func top(counts map[string]int) string {
	best, n := "", 0
	for k, v := range counts {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best
}

func (c *catalog) browse(w http.ResponseWriter, r *http.Request) {
	p, err := media.CleanPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ix := c.index.Load()
	dirs, items := ix.children[p], ix.items[p]
	if p != "." && dirs == nil && items == nil {
		http.NotFound(w, r)
		return
	}
	if dirs == nil {
		dirs = []Dir{}
	}
	if items == nil {
		items = []Item{}
	}
	httpx.JSON(w, map[string]any{"path": p, "dirs": dirs, "items": items})
}

func (c *catalog) search(w http.ResponseWriter, r *http.Request) {
	terms := strings.Fields(strings.ToLower(r.URL.Query().Get("q")))
	limit, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("limit"), "200"))
	if err != nil || limit < 1 {
		http.Error(w, "invalid limit", http.StatusBadRequest)
		return
	}
	limit = min(limit, 1000)
	if r.URL.Query().Get("scope") == "folders" {
		results := []Dir{}
		if len(terms) > 0 {
			for _, dirs := range c.index.Load().children {
				for _, dir := range dirs {
					match := true
					for _, term := range terms {
						if !strings.Contains(strings.ToLower(dir.Path), term) {
							match = false
							break
						}
					}
					if match {
						results = append(results, dir)
					}
				}
			}
		}
		slices.SortFunc(results, func(a, b Dir) int { return strings.Compare(a.Path, b.Path) })
		if len(results) > limit {
			results = results[:limit]
		}
		httpx.JSON(w, results)
		return
	}
	results := []Item{}
	if len(terms) > 0 {
	items:
		for _, it := range c.index.Load().all {
			for _, t := range terms {
				if !strings.Contains(it.haystack, t) {
					continue items
				}
			}
			results = append(results, it)
			if len(results) == limit {
				break
			}
		}
	}
	httpx.JSON(w, results)
}

// tracks collects a folder recursively in one request with a hard bound. A
// folder containing thousands of files cannot flood the queue or browser.
func (c *catalog) tracks(w http.ResponseWriter, r *http.Request) {
	p, err := media.CleanPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit := 300
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = min(limit, 300)
	}
	ix := c.index.Load()
	if p != "." && ix.children[p] == nil && ix.items[p] == nil {
		http.NotFound(w, r)
		return
	}
	items := []Item{}
	truncated := false
	for _, it := range ix.all {
		if p != "." && it.Dir != p && !strings.HasPrefix(it.Dir, p+"/") {
			continue
		}
		if len(items) == limit {
			truncated = true
			break
		}
		items = append(items, it)
	}
	httpx.JSON(w, map[string]any{"items": items, "truncated": truncated, "limit": limit})
}

func (c *catalog) item(w http.ResponseWriter, r *http.Request) {
	p, err := media.CleanPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, it := range c.index.Load().items[path.Dir(p)] {
		if it.Path == p {
			httpx.JSON(w, it)
			return
		}
	}
	http.NotFound(w, r)
}
