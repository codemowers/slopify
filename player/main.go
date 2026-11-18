// Command player is the Slopify output for devices such as a Raspberry Pi
// with a HiFiBerry board: an HTTPS-only counterpart of squeezelite. It
// receives its output's state over Server-Sent Events, decodes the original
// files locally in Go, plays them gaplessly through ALSA and reports
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
	"flag"
	"fmt"
	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/esound"
	"github.com/codemowers/slopify/player/internal/raop"
	"github.com/codemowers/slopify/player/internal/receiverconfig"
	"github.com/codemowers/slopify/player/internal/rtp"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/volume"
)

const version = "0.1"

// state is the part of the output's state the player acts on.
type state struct {
	Seek         *seekEvent      `json:"seek,omitempty"`
	QueueGroup   string          `json:"queueGroup"`
	QueueCommand string          `json:"queueCommand"`
	Volume       *volume.Control `json:"volume,omitempty"`
	CurrentEntry *entry          `json:"currentEntry"`
	Adopted      bool            `json:"adopted"`
	Queue        []entry         `json:"queue"`
	Current      uint64          `json:"current"`
	State        string          `json:"state"`
	Position     float64         `json:"position"`
	Version      uint64          `json:"version"`
}

type seekEvent struct {
	ID       string  `json:"id"`
	Entry    uint64  `json:"entry"`
	Position float64 `json:"position"`
}

type entry struct {
	Artist     string  `json:"artist,omitempty"`
	Album      string  `json:"album,omitempty"`
	Kind       string  `json:"kind,omitempty"`
	Size       int64   `json:"size,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
	Bitrate    int64   `json:"bitrate,omitempty"`
	Codec      string  `json:"codec,omitempty"`
	VideoCodec string  `json:"videoCodec,omitempty"`
	Bits       int     `json:"bits"`
	SampleRate int     `json:"sampleRate"`
	Channels   int     `json:"channels"`
	ID         uint64  `json:"id"`
	Title      string  `json:"title"`
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
	if status, ok := fileCommand(os.Args[1:]); ok {
		os.Exit(status)
	}
	receivers, configErr := receiverconfig.FromEnv()
	if configErr != nil {
		slog.Error("receiver configuration", "err", configErr)
		os.Exit(1)
	}
	volumeControl := flag.String("volume-control", "", "opt-in ALSA mixer control (for example Digital); empty leaves volume untouched")
	volumeCard := flag.String("volume-card", "", "ALSA mixer card ID")
	volumeMin := flag.Int("volume-min", 0, "minimum alsamixer displayed volume (0..100)")
	volumeMax := flag.Int("volume-max", 40, "maximum alsamixer displayed volume (0..100)")
	volumeInitial := flag.Int("volume-initial", 10, "alsamixer displayed volume set once at player startup")
	rotaryInput := flag.String("rotary-input", "", "relative-axis rotary device or kernel name for volume; separate from media keys")
	var inputSpecs []string
	flag.Func("input", "selected input event device or kernel name; repeat for multiple devices; standard media keys only", func(value string) error {
		inputSpecs = append(inputSpecs, value)
		return nil
	})
	localSourceControl := flag.Bool("local-source-control", false, "enable source pinning, arbitration and peak meters")
	localState := flag.String("local-state", "/var/lib/slopify-player/local-audio.json", "local source settings file; empty keeps settings in memory")
	localPulse := flag.Bool("local-pulse", receivers.Pulse, "show the PulseAudio/PipeWire receiver in local sender setup")
	pulseListen := flag.String("pulse-listen", "", "trusted-LAN PulseAudio/PipeWire receiver address, for example 192.0.2.10:4713; no authentication")
	esoundListen := flag.String("esound-listen", "", "trusted-LAN ESound receiver address, for example 192.0.2.10:16001; no authentication")
	pulseBufferMS := flag.Int("pulse-buffer-ms", 0, "least PulseAudio/PipeWire stream buffer in milliseconds, 0 to 2000; 0 honours the sender, and a PipeWire tunnel ticks above about 100")
	var tuning tcpTuning
	flag.DurationVar(&tuning.DialTimeout, "tcp-dial-timeout", 8*time.Second, "time allowed to open an outbound connection; 0 for no limit")
	flag.DurationVar(&tuning.UserTimeout, "tcp-user-timeout", 20*time.Second, "drop an outbound connection whose sent data stays unacknowledged this long (TCP_USER_TIMEOUT, Linux); 0 for the system default of about 15 minutes")
	flag.DurationVar(&tuning.KeepAliveIdle, "tcp-keepalive-idle", 15*time.Second, "idle time before TCP keepalive probes on outbound connections; 0 for the system default")
	flag.DurationVar(&tuning.KeepAliveInterval, "tcp-keepalive-interval", 5*time.Second, "time between TCP keepalive probes; 0 for the system default")
	flag.IntVar(&tuning.KeepAliveCount, "tcp-keepalive-count", 3, "unanswered TCP keepalive probes before dropping the connection; 0 for the system default")
	readAheadMiB := flag.Int("read-ahead-mib", -1, "memory for downloading originals ahead of decoding, the current track to its end and then the next; -1 for half the memory limit (at most 1024), 0 to fetch only as decoding needs")
	flag.BoolVar(&allowOutputTruncate, "allow-truncate", false, "ALSA: permit truncating to 16-bit output if no precision-preserving format works")
	flag.IntVar(&outputBufferMS, "output-buffer-ms", 0, outputBufferHelp)
	esoundBufferMS := flag.Int("esound-buffer-ms", esound.DefaultBufferMillis, "ESound stream buffer in milliseconds, 20 to 2000")
	esoundSocketMS := flag.Int("esound-socket-ms", 0, "cap ESound's kernel receive queue, in milliseconds of audio; 0 leaves the system default, which adds delay")
	raopListen := flag.String("raop-listen", "", "trusted-LAN AirPlay 1 (RAOP) receiver address, for example :5000; advertised over mDNS, no authentication")
	raopBufferMS := flag.Int("raop-buffer-ms", raop.DefaultBufferMillis, "RAOP playout delay in milliseconds, 20 to 2000")
	raopKey := flag.String("raop-key", "", "enable RAOP encryption, which Apple devices require, with this RSA key file; it must be the RAOP key")
	raopKeyCheck := flag.Bool("raop-key-check", false, "verify the -raop-key file and exit")
	rtpReceive := flag.Bool("rtp-receive", false, "trusted-LAN receiver for multicast RTP announced over SAP (L16 and L24); no authentication")
	queueShare := flag.Bool("queue-share", false, "deduplicate same-queue original downloads over trusted-LAN multicast; requires local source control and RTP reception")
	rtpInterface := flag.String("rtp-interface", "", "network interface for rtp-receive and rtp-send; empty lets the system choose")
	rtpSend := flag.Bool("rtp-send", false, "multicast Slopify audio over RTP, announced over SAP with track titles in clear text; no authentication or encryption")
	rtpSendGroup := flag.String("rtp-send-group", "", "multicast ADDRESS or ADDRESS:PORT for rtp-send; empty picks a random one at each start")
	rtpBufferMS := flag.Int("rtp-buffer-ms", rtp.DefaultBufferMillis, "RTP stream delay in milliseconds, 20 to 1000; it holds twice this to absorb clock drift")
	upnpListen := flag.String("upnp-listen", "", "trusted-LAN UPnP media renderer HTTP address, for example 192.0.2.10:49494; also answers SSDP on UDP 1900; no authentication")
	localListen := flag.String("local-listen", "", "optional trusted-LAN status, hardware volume and setup web UI listen address")
	flag.BoolVar(&metricsEnabled, "metrics", metricsEnabled, "enable Prometheus collection and local /metrics; default off on MIPS")
	localSSEInterval := flag.Duration("local-sse-interval", 200*time.Millisecond, "local UI status update interval, 100ms to 10s; slower updates reduce CPU use")
	localSettingsURL := flag.String("local-settings-url", "", "device settings link in local UI (HTTP(S) URL or relative path)")
	localSettingsPort := flag.Int("local-settings-port", 0, "override port for the local settings link (0 keeps URL port)")
	listAudio := flag.Bool("list-audio", false, "print sound cards and their volume controls as JSON, then exit")
	flag.IntVar(&cueLevelDB, "cue-level-db", -6, "sound cue peak in dBFS (-60 to 0); buffering drone is 6 dB lower")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags]\n       %s play [flags] FILE...\n       %s validate [flags] FILE...\n\nMedia keys and rotary controls use Linux input devices through -input.\nUse input-info to list input device names.\n\n", os.Args[0], os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if cueLevelDB < -60 || cueLevelDB > 0 {
		slog.Error("cue-level-db must be -60 to 0 dBFS")
		os.Exit(2)
	}
	if *raopKeyCheck {
		if _, err := raop.LoadKey(*raopKey); err != nil {
			slog.Error("raop key", "err", err)
			os.Exit(1)
		}
		return
	}
	if *listAudio {
		cards, err := alsa.Cards()
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(cards)
		}
		if err != nil {
			slog.Error("listing sound cards", "err", err)
			os.Exit(1)
		}
		return
	}
	sourceOutput := httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0")
	if *localSSEInterval < 100*time.Millisecond || *localSSEInterval > 10*time.Second {
		slog.Error("local-sse-interval must be 100ms–10s")
		os.Exit(2)
	}
	if *queueShare && (!*rtpReceive || !*localSourceControl) {
		slog.Error("queue-share requires local source control and rtp-receive")
		os.Exit(2)
	}
	if *localSettingsPort < 0 || *localSettingsPort > 65535 {
		slog.Error("local-settings-port must be 0–65535")
		os.Exit(2)
	}
	if *localSourceControl && *localListen == "" {
		slog.Error("local-source-control requires local-listen")
		os.Exit(2)
	}
	if *pulseListen != "" && !*localSourceControl {
		slog.Error("pulse-listen requires local-source-control")
		os.Exit(2)
	}
	if *esoundListen != "" && !*localSourceControl {
		slog.Error("esound-listen requires local-source-control")
		os.Exit(2)
	}
	if *raopListen != "" && (!*localSourceControl || *raopBufferMS < 20 || *raopBufferMS > 2000) {
		slog.Error("raop-listen requires local-source-control, and raop-buffer-ms must be 20 to 2000")
		os.Exit(2)
	}
	if *pulseBufferMS < 0 || *pulseBufferMS > 2000 || !validOutputBuffer(outputBufferMS) {
		slog.Error("pulse-buffer-ms must be 0 to 2000, output-buffer-ms 0 or 40 to 10000")
		os.Exit(2)
	}
	switch {
	case *readAheadMiB < -1:
		slog.Error("read-ahead-mib must be -1 (automatic), 0 (off) or a size in MiB")
		os.Exit(2)
	case *readAheadMiB == -1:
		readAheadBytes = autoReadAhead()
	default:
		readAheadBytes = int64(*readAheadMiB) << 20
	}
	if readAheadBytes > 0 {
		if before, after := raiseMemoryLimit(readAheadBytes); after != before {
			slog.Info("read-ahead", "mib", readAheadBytes>>20, "gomemlimitMiB", after>>20, "wasMiB", before>>20)
		} else {
			slog.Info("read-ahead", "mib", readAheadBytes>>20)
		}
	}
	if err := tuning.validate(); err != nil {
		slog.Error("invalid tcp settings", "err", err)
		os.Exit(2)
	}
	if *esoundBufferMS < 20 || *esoundBufferMS > 2000 || *esoundSocketMS < 0 || *esoundSocketMS > 2000 {
		slog.Error("esound-buffer-ms must be 20 to 2000 and esound-socket-ms 0 to 2000")
		os.Exit(2)
	}
	var rtpInterfaceConfig *net.Interface
	if *rtpInterface != "" {
		iface, err := net.InterfaceByName(*rtpInterface)
		if err != nil {
			slog.Error("rtp-interface", "err", err)
			os.Exit(2)
		}
		rtpInterfaceConfig = iface
	}
	rtpInstance := rtp.NewInstance()
	var rtpConfig *rtp.Config
	if *rtpReceive {
		if !*localSourceControl {
			slog.Error("rtp-receive requires local-source-control")
			os.Exit(2)
		}
		if *rtpBufferMS < 20 || *rtpBufferMS > 1000 {
			slog.Error("rtp-buffer-ms must be 20 to 1000")
			os.Exit(2)
		}
		rtpConfig = &rtp.Config{BufferMillis: *rtpBufferMS, Interface: rtpInterfaceConfig, Instance: rtpInstance}
	}
	var rtpGroup netip.AddrPort
	if *rtpSend || *queueShare {
		var err error
		if *rtpSendGroup != "" {
			rtpGroup, err = rtp.ParseGroup(*rtpSendGroup)
		} else {
			rtpGroup, err = rtp.RandomGroup()
		}
		if err != nil {
			slog.Error("rtp-send-group", "err", err)
			os.Exit(2)
		}
	}
	if *upnpListen != "" && !*localSourceControl {
		slog.Error("upnp-listen requires local-source-control")
		os.Exit(2)
	}

	esoundPort := 0
	if *esoundListen != "" {
		_, port, err := net.SplitHostPort(*esoundListen)
		if err == nil {
			esoundPort, err = strconv.Atoi(port)
		}
		if err != nil || esoundPort < 1 || esoundPort > 65535 {
			slog.Error("esound-listen needs a host:port address with a fixed port")
			os.Exit(2)
		}
	}
	configureOutboundHTTP(tuning)
	ctx, stop := httpx.SignalContext()
	defer stop()

	if *rotaryInput != "" {
		inputSpecs = append(inputSpecs, *rotaryInput)
	}
	inputs, err := inputDevices(inputSpecs)
	if err != nil {
		slog.Error("configuring inputs", "err", err)
		os.Exit(2)
	}
	if *rotaryInput != "" {
		inputs[*rotaryInput].rotary = true
	}
	// Without a server the player only receives LAN sources.
	server := strings.TrimRight(os.Getenv("SLOPIFY_SERVER"), "/")
	if server == "" && *localListen == "" {
		slog.Error("SLOPIFY_SERVER or -local-listen is required")
		os.Exit(2)
	}
	setQueueSource(server)
	hostname, _ := os.Hostname()
	name := httpx.EnvOr("SLOPIFY_NAME", hostname)
	token := ""
	if server != "" {
		if token, err = loadToken(); err != nil {
			slog.Error("loading device token", "err", err)
			os.Exit(1)
		}
	}
	c := &controller{
		server: server, token: token, name: name, localSettingsURL: *localSettingsURL, localSettingsPort: *localSettingsPort, localSSEInterval: *localSSEInterval, localEsoundPort: esoundPort, localState: *localState, localSourceControl: *localSourceControl, localAirPlay: *raopListen != "", localPulse: *localPulse, localRTP: *rtpReceive,
		updates:               make(chan *state, 16),
		events:                make(chan event, 16),
		knob:                  make(chan knobEvent, 16),
		buttons:               make(chan string, 8),
		localPlaybackRequests: make(chan localPlaybackRequest),
		volumeWake:            make(chan struct{}, 1),
	}
	if *queueShare {
		c.link = newQueueLink(rtpInstance, *rtpSend)
		if host, port, err := net.SplitHostPort(*localListen); err == nil {
			ip := net.ParseIP(host)
			if host == "" || (ip != nil && !ip.IsLoopback()) {
				n, _ := strconv.ParseUint(port, 10, 16)
				c.link.self.ControlPort = uint16(n)
			}
		}
	}
	services := playbackServices{queue: &c.queue, streamURL: c.streamURL, events: c.events, decodedState: func(state string) { c.local.decodedState(state) }, formats: func(id uint64, input, output localPCM) {
		e := c.queue.entry(id)
		c.local.nativeFormats(input, output, entryMetadata(&e))
	}, inputStats: func(input sourceInput) {
		if c.local != nil && c.local.nativeSources != nil {
			s := c.local.nativeSources
			s.mu.Lock()
			if p := s.source("slopify"); p != nil {
				p.Input = &input
			}
			s.mu.Unlock()
		}
	}, peak: func(v uint32, at time.Time) {
		if c.local != nil {
			c.local.nativePeak("slopify", v, at)
		}
	}}
	backend, err := newNativeBackend(services, sourceOutput)
	if err != nil {
		slog.Error("configuring audio output", "err", err)
		os.Exit(2)
	}
	c.backend = backend
	if *volumeControl != "" {
		c.mixer, err = newMixer(ctx, *volumeCard, *volumeControl, *volumeMin, *volumeMax, *volumeInitial)
		if err != nil {
			slog.Error("configuring volume", "err", err)
			os.Exit(2)
		}
	}
	slog.Info("starting", "server", server, "name", name, "output", httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0"))

	resolvedInputs := map[string]*inputDevice{}
	for path, device := range inputs {
		resolved := inputPath("/sys/class/input", path)
		if resolved == "" {
			resolved = path
		}
		if previous := resolvedInputs[resolved]; previous != nil {
			previous.rotary = previous.rotary || device.rotary
		} else {
			resolvedInputs[resolved] = device
		}
	}
	for path, device := range resolvedInputs {
		go followInput(ctx, path, device, c.knob)
	}
	if *localListen != "" {
		if err := c.startLocalUI(ctx, *localListen, httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0")); err != nil {
			slog.Error("starting local UI", "err", err)
			os.Exit(2)
		}
	}

	if *rtpSend || *queueShare {
		c.rtpSender = rtp.NewSender(rtp.SenderConfig{Name: name, Group: rtpGroup, Interface: rtpInterfaceConfig, Instance: rtpInstance, Control: c.link.controlPacket, OnControl: c.link.receiveControl})
		backend.sender = c.rtpSender
		if c.sources != nil {
			c.rtpSender.SetSourceEligible(false)
		}
		if *queueShare && !*rtpSend {
			c.rtpSender.SetEnabled(false)
		}
		registerRTPSenderMetrics(c.rtpSender)
		c.rtpSender.Start(ctx)
		slog.Info("rtp sender", "group", rtpGroup)
	}
	if c.sources != nil {
		if err := c.startNativeSources(ctx, backend, *pulseListen, *esoundListen, *pulseBufferMS, *esoundBufferMS, *esoundSocketMS, *upnpListen, httpx.EnvOr("SLOPIFY_PULSE_SINK", "slopify.hifiberry"), rtpConfig); err != nil {
			slog.Error("starting native sources", "err", err)
			os.Exit(2)
		}
		if *raopListen != "" {
			if err := c.startRAOP(ctx, *raopListen, *raopBufferMS, *raopKey); err != nil {
				slog.Error("starting raop receiver", "err", err)
				os.Exit(2)
			}
		}
	}
	if c.link != nil {
		go c.pollQueuePeers(ctx)
	}
	if server != "" {
		go c.follow(ctx, name)
	}
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
	localPlaybackRequests chan localPlaybackRequest
	link                  *queueLink
	sourcePause           chan sourcePauseIntent
	localState            string
	localPulse            bool
	localRTP              bool
	localEsoundPort       int
	localSettingsURL      string
	localSettingsPort     int
	localSSEInterval      time.Duration
	rtpSender             *rtp.Sender
	localAirPlay          bool
	nativeLAN             *nativeLAN
	raopEncrypted         atomic.Bool
	remoteCommands        chan remoteCommand
	senderVolumes         chan senderVolumeRequest
	senderVolumeAt        atomic.Int64
	localSourceControl    bool
	sources               *speakerSources
	name                  string
	local                 *localStatus
	mixer                 *mixer
	knob                  chan knobEvent
	buttons               chan string
	volumeWake            chan struct{}
	server, token         string
	backend               playbackBackend
	updates               chan *state
	events                chan event

	outputRetryAt       time.Time
	outputRetryDelay    time.Duration
	sourcePauseSelected string // last observed selection, owned by run
	desired             *state // latest state, owned by run
	applied             uint64 // version last acted on
	sess                playbackSession
	queue               queueView
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
		if c.mixer != nil {
			q.Set("volume", c.mixer.registration())
		}
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

// The server pings every event stream every 20 seconds, so a longer silence
// means the connection is dead, however long the kernel would keep retrying it.
var eventStreamIdleTimeout = 60 * time.Second

var errEventStreamIdle = errors.New("event stream silent, assuming a dead connection")

// idleWatchReader pushes back the stream's deadline whenever bytes arrive.
type idleWatchReader struct {
	r     io.Reader
	timer *time.Timer
}

func (w idleWatchReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.timer.Reset(eventStreamIdleTimeout)
	}
	return n, err
}

func (c *controller) stream(parent context.Context, path string) (bool, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	watchdog := time.AfterFunc(eventStreamIdleTimeout, func() { cancel(errEventStreamIdle) })
	defer watchdog.Stop()
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		if errors.Is(context.Cause(ctx), errEventStreamIdle) {
			return false, errEventStreamIdle
		}
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	slog.Info("connected")
	c.reportVolume(ctx)
	sc := bufio.NewScanner(idleWatchReader{r: resp.Body, timer: watchdog})
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
		if errors.Is(context.Cause(ctx), errEventStreamIdle) {
			return true, errEventStreamIdle
		}
		return true, err
	}
	return true, errors.New("stream closed by server")
}

type event struct {
	ended   bool
	entry   uint64
	session playbackSession
}

func (c *controller) run(ctx context.Context) {
	if c.sources != nil {
		c.sourcePause = make(chan sourcePauseIntent, 1)
		go c.runSourcePause(ctx)
	}
	go c.runKnob(ctx)
	if c.mixer != nil && c.server != "" {
		go c.volumeReports(ctx)
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	volumeTick := time.NewTicker(time.Second)
	defer volumeTick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.stop()
			return
		case s := <-c.updates:
			if c.mixer != nil && s.Volume != nil {
				previous := c.mixer.snapshot().Revision
				if err := c.mixer.apply(ctx, s.Volume); err != nil {
					slog.Warn("setting volume", "err", err)
				}
				if previous != c.mixer.snapshot().Revision {
					c.reportVolume(ctx)
				}
			}
			c.reconcile(s)
			c.requestSourcePause()
		case <-volumeTick.C:
			c.updateQueueLink()
			c.requestSourcePause()
			c.recoverOutput(time.Now())
			if c.mixer != nil {
				changed, err := c.mixer.read(ctx)
				if err != nil {
					slog.Warn("reading volume", "err", err)
				} else if changed || time.Now().Unix()%5 == 0 {
					c.reportVolume(ctx)
				}
			}
		case req := <-c.localPlaybackRequests:
			c.handleLocalPlayback(req)
		case op := <-c.buttons:
			if c.desired != nil && c.desired.Adopted {
				if op == "play" || op == "pause" {
					if (op == "play") == (c.desired.State == "playing") {
						continue
					}
					op = "toggle"
				}
				reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				resp, err := c.request(reqCtx, "POST", "/device/v1/control", map[string]any{"op": op, "version": c.desired.Version, "entry": c.desired.Current})
				if err != nil {
					slog.Warn("button command failed", "op", op)
				} else {
					resp.Body.Close()
					if resp.StatusCode != 204 {
						slog.Warn("button command rejected", "op", op, "status", resp.StatusCode)
					}
				}
				cancel()
			}

		case e := <-c.events:
			if c.link != nil && e.ended && e.session == c.sess {
				c.link.ended(e.entry)
			}
			if e.ended && c.desired != nil && c.sess != nil && e.session == c.sess {
				c.report(map[string]any{"version": c.desired.Version, "entry": e.entry, "state": "playing", "ended": true})
			}
		case <-tick.C:
			c.reportQueueFollower()
			if c.sess != nil && c.desired != nil && c.desired.State == "playing" {
				c.report(map[string]any{"version": c.desired.Version, "entry": c.sess.current(), "state": "playing", "position": c.sess.position()})
			}
		}
	}
}

// reconcile makes playback follow the output's state.
func (c *controller) reconcile(s *state) {
	announceStart := s.State == "playing" && (c.desired == nil || c.desired.State != "playing" || c.desired.Current != s.Current)
	paused := c.desired != nil && c.desired.State == "playing" && s.State == "paused" && s.Adopted && c.desired.Current == s.Current
	versionChanged := c.desired == nil || s.Version != c.applied
	seekChanged := s.Seek != nil && s.Seek.ID != "" && s.Seek.Entry == s.Current &&
		(c.desired == nil || c.desired.Seek == nil || c.desired.Seek.ID != s.Seek.ID)
	startPosition := s.Position
	if seekChanged {
		startPosition = s.Seek.Position
	}
	if c.desired == nil || versionChanged || s.State != c.desired.State || s.Current != c.desired.Current || s.Adopted != c.desired.Adopted {
		slog.Info("playback command", "state", s.State, "entry", s.Current, "version", s.Version, "adopted", s.Adopted)
	}
	c.desired = s
	c.queue.set(s.playbackQueue())
	c.applied = s.Version
	e := s.entry(s.Current)
	if c.local != nil {
		c.local.mu.Lock()
		c.local.queuePaused = s.State == "paused"
		c.local.queuePlaying = s.State == "playing"
		if native := c.local.nativeSources; native != nil {
			native.mu.Lock()
			if p := native.source("slopify"); p != nil {
				p.Paused, p.Playing = c.local.queuePaused, c.local.queuePlaying
				p.Detached = s.Adopted && s.QueueGroup == ""
			}
			native.mu.Unlock()
		}
		c.local.trackTitle = ""
		c.local.trackMetadata = nil
		if e != nil {
			c.local.trackTitle = e.Title
			c.local.trackMetadata = entryMetadata(e)
		}
		c.local.mu.Unlock()
	}
	following := c.keepQueueFollower(s)
	if !s.Adopted || e == nil || s.State != "playing" {
		if c.sess != nil {
			pos, cur := c.sess.position(), c.sess.current()
			announce := paused && cur == s.Current && !c.sess.finished()
			c.stop()
			if announcer, ok := c.backend.(interface{ PauseSound() }); ok && announce {
				announcer.PauseSound()
			}
			// Report where playback actually stopped, so resuming starts there.
			if s.State == "paused" && cur == s.Current && versionChanged {
				c.report(map[string]any{"version": s.Version, "entry": cur, "state": "paused", "position": pos})
			}
		}
		return
	}
	if following {
		return
	}
	if c.sess != nil && c.sess.finished() && c.sess.failure() && c.sess.current() == e.ID && !versionChanged {
		c.recoverOutput(time.Now())
		return
	}
	switch {
	case c.sess == nil || c.sess.finished() || c.sess.current() != e.ID:
		c.outputRetryAt = time.Time{}
		c.outputRetryDelay = 0
		if announcer, ok := c.backend.(interface {
			StartAnnounced(uint64, float64) playbackSession
		}); ok && announceStart {
			c.stop()
			c.sess = announcer.StartAnnounced(e.ID, startPosition)
		} else {
			c.start(e.ID, startPosition)
		}
	// Progress snapshots acknowledge playback; only an explicit event seeks.
	case seekChanged:
		c.startSeek(e.ID, s.Seek.Position)
	}
}

// Recover a failed output from its last submitted position without needing a
// cloud state change. Polling and backoff stay on the controller goroutine.
func (c *controller) recoverOutput(now time.Time) {
	if c.desired == nil || !c.desired.Adopted || c.desired.State != "playing" || c.sess == nil {
		return
	}
	if !c.sess.failure() || !c.sess.finished() {
		if now.Sub(c.sess.started()) > 5*time.Second && !c.sess.finished() {
			c.outputRetryDelay = 0
			c.outputRetryAt = time.Time{}
		}
		return
	}
	if c.sess.current() != c.desired.Current {
		return
	}
	if c.outputRetryAt.IsZero() {
		delay := c.outputRetryDelay
		if delay == 0 {
			delay = time.Second
		}
		c.outputRetryAt = now.Add(delay)
		c.outputRetryDelay = min(delay*2, 10*time.Second)
		return
	}
	if now.Before(c.outputRetryAt) {
		return
	}
	id, at := c.sess.current(), c.sess.position()
	c.outputRetryAt = time.Time{}
	c.start(id, at)
}

func (c *controller) start(id uint64, at float64) {
	c.stop()
	slog.Info("playing", "entry", id, "at", at)
	c.sess = c.backend.Start(id, at)
}

// startSeek is start for a seek within the playing entry, which a backend may announce.
func (c *controller) startSeek(id uint64, at float64) {
	seeker, ok := c.backend.(interface {
		StartSeek(uint64, float64) playbackSession
	})
	if !ok {
		c.start(id, at)
		return
	}
	c.stop()
	slog.Info("seeking", "entry", id, "at", at)
	c.sess = seeker.StartSeek(id, at)
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
