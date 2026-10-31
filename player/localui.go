package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/audiometer"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui/*
var localUIFiles embed.FS

type localPCM struct {
	Codec    string `json:"codec,omitempty"`
	Format   string `json:"format"`
	Rate     int    `json:"rate"`
	Channels int    `json:"channels"`
}
type localNode struct {
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	State        string    `json:"state"`
	Format       *localPCM `json:"format,omitempty"`
	GraphFormat  *localPCM `json:"graphFormat,omitempty"`
	Mode         string    `json:"mode,omitempty"`
	FormatOrigin string    `json:"formatOrigin,omitempty"`
	SourceIP     string    `json:"sourceIP,omitempty"`
	Title        string    `json:"title,omitempty"`
}

func playbackStarting(state string) bool {
	switch state {
	case "starting", "starting-mpv", "loading", "buffering":
		return true
	}
	return false
}

type localPlaybackStatus struct {
	State string    `json:"state"`
	Since time.Time `json:"since"`
}
type localStatus struct {
	queuePaused   bool
	queuePlaying  bool
	playback      localPlaybackStatus
	subscribers   atomic.Int32
	mu            sync.Mutex
	device        *alsa.Device
	output        string
	err           string
	decoded       localNode
	nativeOutput  localNode
	nativeSources *speakerSources // what the native engine reports its source state to
	trackTitle    string
	trackMetadata map[string]string
}

func (l *localStatus) decodedState(f format, state string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.playback.State != state {
		l.playback = localPlaybackStatus{State: state, Since: time.Now()}
	}
	if s := l.nativeSources; s != nil {
		s.mu.Lock()
		if p := s.source("slopify"); p != nil {
			p.State = state
			if s.Fixed {
				p.Active = state == "running"
			}
			if f.rate > 0 { // The fallback engine knows only its output format.
				p.Format = &localPCM{Format: f.pcmName(), Rate: f.rate, Channels: f.channels}
				p.Metadata = nil
			}
		}
		s.mu.Unlock()
	}
	if f.rate <= 0 {
		if l.decoded.Name != "" {
			l.decoded.State = state
		}
		if l.nativeOutput.Name != "" {
			l.nativeOutput.State = state
		}
		return
	}
	l.decoded = localNode{Name: "Slopify decoded audio", Kind: "Slopify source", State: state, Format: &localPCM{Format: f.pcmName(), Rate: f.rate, Channels: f.channels}}
}

// nativeFormats fills the Slopify card and the amplifier box.
func (l *localStatus) nativeFormats(input, output localPCM, metadata map[string]string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.decoded = localNode{Name: "Slopify decoded audio", Kind: "Slopify source", State: l.playback.State, Format: &input}
	l.nativeOutput = localNode{Name: "Amplifier", Kind: "Hardware output", State: l.playback.State, Format: &output}
	l.mu.Unlock()
	if s := l.nativeSources; s != nil {
		s.mu.Lock()
		if p := s.source("slopify"); p != nil {
			p.Format, p.Metadata = &input, metadata
		}
		s.mu.Unlock()
	}
}

// nativePeak records a window for a source, and for the output if it is selected.
func (l *localStatus) nativePeak(id string, peak float64, at time.Time) {
	s := l.nativeSources
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source := s.source(id)
	if source == nil || source.meter == nil {
		return
	}
	source.meter.Observe(peak, at)
	level := source.meter.Snapshot()
	rememberSourcePeak(source, level, time.Now())
	if s.Selected == id {
		s.Output = append(s.Output, sourceOutputWindow{SourceID: id, Window: level})
		if len(s.Output) > 150 {
			s.Output = s.Output[len(s.Output)-150:]
		}
	}
}

func (c *controller) localSnapshot() any {
	l := c.local
	l.mu.Lock()
	defer l.mu.Unlock()
	nodes := make([]localNode, 0, 2)
	if l.decoded.Name != "" {
		nodes = append(nodes, l.decoded)
	}
	if l.nativeOutput.Name != "" {
		nodes = append(nodes, l.nativeOutput)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		return nodes[i].Name < nodes[j].Name
	})
	var vol *localVolume
	if c.mixer != nil {
		vol = c.mixer.localVolume()
	}
	serverHost := ""
	if serverURL, err := url.Parse(c.server); err == nil {
		serverHost = serverURL.Hostname()
	}
	hostname, _ := os.Hostname()
	var ampStatus map[string]any
	if c.amp != nil {
		ampStatus = c.amp.status()
	}
	return struct {
		Pulse      bool                `json:"pulseEnabled"`
		AirPlay    bool                `json:"airplayEnabled"`
		EsoundPort int                 `json:"esoundPort,omitempty"`
		Amp        map[string]any      `json:"amp,omitempty"`
		ServerHost string              `json:"serverHost,omitempty"`
		Playback   localPlaybackStatus `json:"playback"`
		Now        time.Time           `json:"now"`
		Name       string              `json:"name"`
		Hostname   string              `json:"hostname,omitempty"`
		PulseSink  string              `json:"pulseSink,omitempty"`
		Output     string              `json:"output"`
		Device     *alsa.Device        `json:"outputDevice,omitempty"`
		Engine     string              `json:"engine"`
		Nodes      []localNode         `json:"nodes"`
		Volume     any                 `json:"volume"`
		Error      string              `json:"error,omitempty"`
		Sources    any                 `json:"sourceControl,omitempty"`
	}{c.localPulse, c.localAirPlay, c.localEsoundPort, ampStatus, serverHost, l.playback, time.Now(), c.name, hostname, httpx.EnvOr("SLOPIFY_PULSE_SINK", "slopify.hifiberry"), l.output, l.device, c.engineName, nodes, vol, l.err, c.sourceSnapshot()}
}
func (c *controller) localEvents(w http.ResponseWriter, r *http.Request) {
	if c.local.subscribers.Add(1) > 16 {
		c.local.subscribers.Add(-1)
		http.Error(w, "Too many status viewers", http.StatusTooManyRequests)
		return
	}
	defer c.local.subscribers.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	rc := http.NewResponseController(w)
	for {
		data, err := json.Marshal(c.localSnapshot())
		if err != nil {
			return
		}
		rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func (c *controller) serveLocal(ctx context.Context, listener net.Listener) {
	root, _ := fs.Sub(localUIFiles, "ui")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(root)))
	mux.HandleFunc("GET /api/events", c.localEvents)
	mux.HandleFunc("POST /api/volume", c.localVolumeCommand)
	mux.HandleFunc("POST /api/amp", c.localAmpCommand)
	mux.HandleFunc("POST /api/sources", c.localSourceCommand)
	if c.sourceList() != nil {
		mux.HandleFunc("GET /api/history", c.localHistory)
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(nativeMetricsRegistry, promhttp.HandlerOpts{MaxRequestsInFlight: 2, Timeout: 2 * time.Second}))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(c.localSnapshot())
	})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { <-ctx.Done(); server.Close() }()
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		slog.Error("local UI", "err", err)
	}
}
func (c *controller) startLocalUI(ctx context.Context, address, output string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	c.local = &localStatus{output: output}
	if spec, ok := strings.CutPrefix(output, "alsa:"); ok {
		if device, err := alsa.Describe(spec); err == nil {
			c.local.device = &device
		}
	}
	if c.engineName == "native" {
		if c.localSourceControl {
			c.sources = newSpeakerSources(c.localState)
			if c.server == "" {
				c.sources.withoutServer()
			}
			c.local.nativeSources = c.sources
			for _, p := range c.sources.Sources {
				if p.meter == nil {
					p.meter = audiometer.New()
				}
			}
		} else {
			c.local.nativeSources = newFixedSources()
		}
	}
	go c.serveLocal(ctx, listener)
	slog.Info("local speaker UI listening", "address", listener.Addr())
	return nil
}

// nativeDevice shows what the amplifier is receiving, if anything.
func (l *localStatus) nativeDevice(output *localPCM) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if output == nil {
		l.nativeOutput.State = "idle"
		return
	}
	l.nativeOutput = localNode{Name: "Amplifier", Kind: "Hardware output", State: "running", Format: output}
}
