// Command player is the Slopify output for devices such as a Raspberry Pi
// with a HiFiBerry board: an HTTPS-only counterpart of squeezelite. It
// receives its output's state over Server-Sent Events, decodes the original
// files locally with ffmpeg, plays them gaplessly through ALSA and reports
// progress back with POSTs. The first start generates a token that pairs the
// device with the server; adopt the device in the web app to use it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/httpx"
)

const version = "0.1"

// state is the part of the output's state the player acts on.
type state struct {
	CurrentEntry *entry  `json:"currentEntry"`
	Adopted      bool    `json:"adopted"`
	Queue        []entry `json:"queue"`
	Current      uint64  `json:"current"`
	State        string  `json:"state"`
	Position     float64 `json:"position"`
	Version      uint64  `json:"version"`
}

type entry struct {
	Bits       int    `json:"bits"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
	ID         uint64 `json:"id"`
	Title      string `json:"title"`
}

func (s *state) entry(id uint64) *entry {
	for i := range s.Queue {
		if s.Queue[i].ID == id {
			return &s.Queue[i]
		}
	}
	if s.CurrentEntry != nil && s.CurrentEntry.ID == id {
		return s.CurrentEntry
	}
	return nil
}

func (s *state) playbackQueue() []entry {
	for _, e := range s.Queue {
		if e.ID == s.Current {
			return s.Queue
		}
	}
	if s.CurrentEntry != nil {
		return append([]entry{*s.CurrentEntry}, s.Queue...)
	}
	return s.Queue
}

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()

	server := strings.TrimRight(httpx.Env("SLOPIFY_SERVER"), "/")
	hostname, _ := os.Hostname()
	name := httpx.EnvOr("SLOPIFY_NAME", hostname)
	token, err := loadToken()
	if err != nil {
		slog.Error("loading device token", "err", err)
		os.Exit(1)
	}
	open, err := sinkFor(httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0"))
	if err != nil {
		slog.Error("invalid SLOPIFY_OUTPUT", "err", err)
		os.Exit(2)
	}
	c := &controller{
		server: server, token: token, open: open,
		ffmpeg:  httpx.EnvOr("SLOPIFY_FFMPEG", "ffmpeg"),
		ffprobe: httpx.EnvOr("SLOPIFY_FFPROBE", "ffprobe"),
		updates: make(chan *state, 16),
		events:  make(chan event, 16),
	}
	slog.Info("starting", "server", server, "name", name, "output", httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0"))
	go c.follow(ctx, name)
	c.run(ctx)
}

// loadToken reads the device token, generating it on first start.
func loadToken() (string, error) {
	if t := os.Getenv("SLOPIFY_TOKEN"); t != "" {
		return t, nil
	}
	file := httpx.EnvOr("SLOPIFY_TOKEN_FILE", "/var/lib/slopify-player/token")
	b, err := os.ReadFile(file)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	token := hex.EncodeToString(randomBytes(32))
	return token, os.WriteFile(file, []byte(token+"\n"), 0o600)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

type controller struct {
	server, token, ffmpeg, ffprobe string
	open                           func(format) (sink, error)
	updates                        chan *state
	events                         chan event

	desired *state // latest state, owned by run
	applied uint64 // version last acted on
	sess    *session
	queue   queueView
}

func (c *controller) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// follow keeps the event stream open, reconnecting with backoff.
func (c *controller) follow(ctx context.Context, name string) {
	q := url.Values{"name": {name}, "model": {"slopify-player " + version}}
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := c.stream(ctx, "/device/v1/events?"+q.Encode())
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		slog.Warn("event stream ended", "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *controller) stream(ctx context.Context, path string) (bool, error) {
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	slog.Info("connected")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20) // a large queue is one event
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if e, ok := strings.CutPrefix(line, "event: "); ok {
			event = e
		} else if data, ok := strings.CutPrefix(line, "data: "); ok && event == "player" {
			var s state
			if err := json.Unmarshal([]byte(data), &s); err != nil {
				return true, err
			}
			c.updates <- &s
		} else if line == "" {
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, errors.New("stream closed by server")
}

type event struct {
	ended bool
	entry uint64
}

func (c *controller) run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.stop()
			return
		case s := <-c.updates:
			c.reconcile(s)
		case e := <-c.events:
			if e.ended && c.desired != nil {
				c.report(map[string]any{"version": c.desired.Version, "entry": e.entry, "state": "playing", "ended": true})
			}
		case <-tick.C:
			if c.sess != nil && c.desired != nil && c.desired.State == "playing" {
				c.report(map[string]any{"version": c.desired.Version, "entry": c.sess.current(), "state": "playing", "position": c.sess.position()})
			}
		}
	}
}

// reconcile makes playback follow the output's state.
func (c *controller) reconcile(s *state) {
	versionChanged := c.desired == nil || s.Version != c.applied
	c.desired = s
	c.queue.set(s.playbackQueue())
	c.applied = s.Version
	e := s.entry(s.Current)
	if !s.Adopted || e == nil || s.State != "playing" {
		if c.sess != nil {
			pos, cur := c.sess.position(), c.sess.current()
			c.stop()
			// Report where playback actually stopped, so resuming starts there.
			if s.State == "paused" && cur == s.Current && versionChanged {
				c.report(map[string]any{"version": s.Version, "entry": cur, "state": "paused", "position": pos})
			}
		}
		return
	}
	switch {
	case c.sess == nil || c.sess.finished() || c.sess.current() != e.ID:
		c.start(e.ID, s.Position)
	case versionChanged && math.Abs(c.sess.position()-s.Position) > 2:
		c.start(e.ID, s.Position) // a seek
	}
}

func (c *controller) start(id uint64, at float64) {
	c.stop()
	slog.Info("playing", "entry", id, "at", at)
	c.sess = c.startSession(id, at)
}

func (c *controller) stop() {
	if c.sess != nil {
		c.sess.close()
		c.sess = nil
	}
}

func (c *controller) report(body map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.request(ctx, http.MethodPost, "/device/v1/report", body)
	if err != nil {
		slog.Warn("reporting", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("reporting", "status", resp.Status)
	}
}

// streamURL asks the server for a signed URL of an entry's original file.
func (c *controller) streamURL(ctx context.Context, id uint64) (string, error) {
	resp, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/device/v1/stream?entry=%d", id), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stream URL: %s", resp.Status)
	}
	var v struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return c.server + v.URL, nil
}
