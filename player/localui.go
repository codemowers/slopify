package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/audiometer"
	"github.com/codemowers/slopify/player/internal/discovery"
	"github.com/codemowers/slopify/player/internal/rtp"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed ui/*
var localUIFiles embed.FS

type localPCM struct {
	DecodedBits int    `json:"decodedBits,omitempty"`
	Codec       string `json:"codec,omitempty"`
	Format      string `json:"format"`
	Rate        int    `json:"rate"`
	Channels    int    `json:"channels"`
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
	case "starting", "loading", "buffering":
		return true
	}
	return false
}

type localPlaybackStatus struct {
	State string    `json:"state"`
	Since time.Time `json:"since"`
}
type localStatus struct {
	siblings      *discovery.Browser
	queuePaused   bool
	queuePlaying  bool
	playback      localPlaybackStatus
	eventsMu      sync.Mutex
	viewers       []*localViewer
	mu            sync.Mutex
	device        *alsa.Device
	output        string
	hardware      string
	err           string
	decoded       localNode
	nativeOutput  localNode
	nativeSources *speakerSources // receives playback source state
	trackTitle    string
	trackMetadata map[string]string
}

func (l *localStatus) decodedState(state string) {
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
		}
		s.mu.Unlock()
	}
	if l.decoded.Name != "" {
		l.decoded.State = state
	}
	if l.nativeOutput.Name != "" {
		l.nativeOutput.State = state
	}
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
func (l *localStatus) nativePeak(id string, peak uint32, at time.Time) {
	l.nativePeakFrom(id, id, peak, at)
}

// Keep the logical source's meter while recording the physical input colour
// on the output timeline, including across later routing changes.
func (l *localStatus) nativePeakFrom(id, inputID string, peak uint32, at time.Time) {
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
		s.Output = append(s.Output, sourceOutputWindow{SourceID: inputID, Window: level})
		if len(s.Output) > 150 {
			s.Output = s.Output[len(s.Output)-150:]
		}
	}
}

func (c *controller) localSnapshot(history ...*outputHistoryCursor) any {
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
	serverHost, serverOrigin := "", ""
	if serverURL, err := url.Parse(c.server); err == nil {
		serverHost = serverURL.Hostname()
		if serverURL.Scheme == "https" || serverURL.Scheme == "http" {
			serverOrigin = (&url.URL{Scheme: serverURL.Scheme, Host: serverURL.Host}).String()
		}
	}
	hostname, _ := os.Hostname()
	var multicast *rtp.SenderStatus
	if c.rtpSender != nil {
		status := c.rtpSender.Status()
		multicast = &status
	}
	return struct {
		Pulse        bool                `json:"pulseEnabled"`
		RTP          bool                `json:"rtpEnabled"`
		AirPlay      bool                `json:"airplayEnabled"`
		AirPlayRSA   bool                `json:"airplayEncrypted"`
		EsoundPort   int                 `json:"esoundPort,omitempty"`
		Siblings     []discovery.Peer    `json:"siblings"`
		SettingsURL  string              `json:"settingsUrl,omitempty"`
		QueueLink    *queueLinkStatus    `json:"queueLink,omitempty"`
		SettingsPort int                 `json:"settingsPort,omitempty"`
		ServerOrigin string              `json:"serverOrigin,omitempty"`
		ServerHost   string              `json:"serverHost,omitempty"`
		Playback     localPlaybackStatus `json:"playback"`
		Now          time.Time           `json:"now"`
		Name         string              `json:"name"`
		Hardware     string              `json:"hardware,omitempty"`
		Hostname     string              `json:"hostname,omitempty"`
		PulseSink    string              `json:"pulseSink,omitempty"`
		Output       string              `json:"output"`
		Device       *alsa.Device        `json:"outputDevice,omitempty"`
		Nodes        []localNode         `json:"nodes"`
		Volume       any                 `json:"volume"`
		Error        string              `json:"error,omitempty"`
		Sources      any                 `json:"sourceControl,omitempty"`
		Multicast    *rtp.SenderStatus   `json:"multicast,omitempty"`
	}{c.localPulse, c.localRTP, c.localAirPlay, c.raopEncrypted.Load(), c.localEsoundPort, l.siblings.Snapshot(), c.localSettingsURL, c.link.snapshot(), c.localSettingsPort, serverOrigin, serverHost, l.playback, time.Now(), c.name, l.hardware, hostname, httpx.EnvOr("SLOPIFY_PULSE_SINK", "slopify.hifiberry"), l.output, l.device, nodes, vol, l.err, c.sourceSnapshot(history...), multicast}
}
func (c *controller) localEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	viewer := &localViewer{evicted: make(chan struct{})}
	viewer.stop = func() {
		close(viewer.evicted)
		rc.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	}
	c.local.addViewer(viewer)
	defer c.local.removeViewer(viewer)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	interval := c.localSSEInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var history outputHistoryCursor
	var previous any
	evicted := func() {
		rc.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		fmt.Fprint(w, "event: evicted\ndata: {}\n\n")
		rc.Flush()
	}
	for {
		select {
		case <-viewer.evicted:
			evicted()
			return
		default:
		}
		current := statusValue(reflect.ValueOf(c.localSnapshot(&history)))
		var message any
		if previous == nil {
			message = map[string]any{"snapshot": current}
		} else {
			changes := make([][]any, 0)
			statusDiff(previous, current, "", &changes)
			message = map[string]any{"patch": changes}
		}
		data, err := json.Marshal(message)
		if err != nil {
			return
		}
		previous = current
		rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		select {
		case <-viewer.evicted:
			evicted()
			return
		default:
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
		select {
		case <-viewer.evicted:
			evicted()
			return
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
	mux.HandleFunc("GET /multicast.sdp", c.localMulticastSDP)
	mux.HandleFunc("POST /api/volume", c.localVolumeCommand)
	mux.HandleFunc("GET /api/playback", c.localPlaybackControl)
	mux.HandleFunc("POST /api/playback", c.localPlaybackControl)
	mux.HandleFunc("POST /api/sources", c.localSourceCommand)
	mux.HandleFunc("GET /api/queue-link", c.queueGossip)
	if c.sourceList() != nil {
		mux.HandleFunc("GET /api/history", c.localHistory)
	}
	if metricsEnabled {
		mux.Handle("GET /metrics", promhttp.HandlerFor(nativeMetricsRegistry, promhttp.HandlerOpts{MaxRequestsInFlight: 2, Timeout: 2 * time.Second}))
	}
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
func (c *controller) localMulticastSDP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c.rtpSender == nil {
		http.Error(w, "Multicast sender is disabled", http.StatusNotFound)
		return
	}
	sdp := c.rtpSender.SessionDescription()
	if sdp == "" {
		http.Error(w, "Start Slopify playback to establish the multicast format", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Content-Disposition", `inline; filename="slopify-multicast.sdp"`)
	fmt.Fprint(w, sdp)
}
func (c *controller) startLocalUI(ctx context.Context, address, output string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	c.local = &localStatus{output: output, hardware: hardwareDescription()}
	if spec, ok := strings.CutPrefix(output, "alsa:"); ok {
		if device, err := alsa.Describe(spec); err == nil {
			c.local.device = &device
		}
	}
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
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		c.local.siblings = discovery.Start(ctx, c.name, tcp.Port)
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
