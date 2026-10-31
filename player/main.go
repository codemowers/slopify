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
	"flag"
	"fmt"
	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/esound"
	"github.com/codemowers/slopify/player/internal/lirc"
	"github.com/codemowers/slopify/player/internal/receiverconfig"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/volume"
)

const version = "0.1"

// state is the part of the output's state the player acts on.
type state struct {
	Volume       *volume.Control `json:"volume,omitempty"`
	CurrentEntry *entry          `json:"currentEntry"`
	Adopted      bool            `json:"adopted"`
	Queue        []entry         `json:"queue"`
	Current      uint64          `json:"current"`
	State        string          `json:"state"`
	Position     float64         `json:"position"`
	Version      uint64          `json:"version"`
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
	receivers, configErr := receiverconfig.FromEnv()
	if configErr != nil {
		slog.Error("receiver configuration", "err", configErr)
		os.Exit(1)
	}
	volumeControl := flag.String("volume-control", "", "opt-in ALSA mixer control (for example Digital); empty leaves volume untouched")
	volumeCard := flag.String("volume-card", "", "ALSA mixer card ID")
	volumeMin := flag.Int("volume-min", 0, "minimum alsamixer displayed volume (0..100)")
	volumeMax := flag.Int("volume-max", 40, "maximum alsamixer displayed volume (0..100)")
	volumeInitial := flag.Int("volume-initial", 20, "alsamixer displayed volume set once at player startup")
	rotaryInput := flag.String("rotary-input", "", "relative-axis input device, a /dev path or kernel device name such as rotary@17 (optional)")
	buttonInput := flag.String("button-input", "", "play/pause input device, path or kernel name[:keycode], default key KEY_PLAYPAUSE (optional)")
	previousInput := flag.String("previous-input", "", "previous-track input device, path or kernel name[:keycode], default key KEY_PREVIOUSSONG (optional)")
	nextInput := flag.String("next-input", "", "next-track input device, path or kernel name[:keycode], default key KEY_NEXTSONG (optional)")
	muteInput := flag.String("mute-input", "", "mute input device, path or kernel name[:keycode], default key KEY_MUTE (optional)")
	engine := flag.String("playback-engine", "native", "playback engine: native (Go decoding, direct ALSA, mpv fallback when installed) or mpv")
	localSourceControl := flag.Bool("local-source-control", false, "enable source pinning, arbitration and peak meters")
	localState := flag.String("local-state", "/var/lib/slopify-player/local-audio.json", "local source settings file; empty keeps settings in memory")
	localPulse := flag.Bool("local-pulse", receivers.Pulse, "advertise configured Pulse receiver in local sender setup")
	airplayMetadataListen := flag.String("airplay-metadata-listen", os.Getenv("SLOPIFY_AIRPLAY_METADATA_LISTEN"), "loopback UDP endpoint for Shairport sender metadata and hardware volume")
	localAirPlay := flag.Bool("local-airplay", receivers.AirPlay, "advertise configured AirPlay receiver in local sender setup")
	pulseListen := flag.String("pulse-listen", "", "native engine: trusted-LAN Pulse receiver address, for example 192.0.2.10:4713; no authentication")
	esoundListen := flag.String("esound-listen", "", "native engine: trusted-LAN ESound receiver address, for example 192.0.2.10:16001; no authentication")
	pulseBufferMS := flag.Int("pulse-buffer-ms", 0, "least Pulse stream buffer in milliseconds, 0 to 2000; 0 honours the sender, and a PipeWire tunnel ticks above about 100")
	liveDeviceMS := flag.Int("live-device-buffer-ms", 0, "ALSA buffer for LAN sources in milliseconds, 40 to 2000; 0 lets the device choose within 150 to 250")
	esoundBufferMS := flag.Int("esound-buffer-ms", esound.DefaultBufferMillis, "ESound stream buffer in milliseconds, 20 to 2000")
	esoundSocketMS := flag.Int("esound-socket-ms", 0, "cap ESound's kernel receive queue, in milliseconds of audio; 0 leaves the system default, which adds delay")
	irDevice := flag.String("ir-device", os.Getenv("SLOPIFY_IR_DEVICE"), "opt-in LIRC device driving an amplifier's motorised volume knob, for example /dev/lirc0")
	irAddress := flag.String("ir-address", httpx.EnvOr("SLOPIFY_IR_ADDRESS", "0x877C"), "NEC extended address of the amplifier")
	irVolumeUp := flag.String("ir-volume-up", httpx.EnvOr("SLOPIFY_IR_VOLUME_UP", "0x88"), "IR command for volume up")
	irVolumeDown := flag.String("ir-volume-down", httpx.EnvOr("SLOPIFY_IR_VOLUME_DOWN", "0x8C"), "IR command for volume down")
	irPowerOn := flag.String("ir-power-on", httpx.EnvOr("SLOPIFY_IR_POWER_ON", "0x25"), "IR command for power on")
	irPowerOff := flag.String("ir-power-off", httpx.EnvOr("SLOPIFY_IR_POWER_OFF", "0xC8"), "IR command for power off")
	irSweep := flag.Duration("ir-sweep", envDuration("SLOPIFY_IR_SWEEP", 6*time.Second), "time to hold volume down, longer than the knob's full travel")
	irIdleOff := flag.Duration("ir-idle-off", envDuration("SLOPIFY_IR_IDLE_OFF", 5*time.Minute), "silence before sweeping to zero and powering off")
	irWake := flag.Duration("ir-wake", envDuration("SLOPIFY_IR_WAKE", 3*time.Second), "wait after power on before volume commands")
	irRepeat := flag.Duration("ir-repeat", envDuration("SLOPIFY_IR_REPEAT", 110*time.Millisecond), "time between IR commands")
	irInitialSteps := flag.Int("ir-initial-steps", envInt("SLOPIFY_IR_INITIAL_STEPS", 0), "volume steps up from zero until a level has been saved")
	irMaxSteps := flag.Int("ir-max-steps", envInt("SLOPIFY_IR_MAX_STEPS", 60), "most volume steps up from zero, so that nothing can drive the knob to full")
	irState := flag.String("ir-state", httpx.EnvOr("SLOPIFY_IR_STATE", "/var/lib/slopify-player/ir-amp.json"), "file remembering the amp's volume steps; empty keeps them in memory")
	upnpListen := flag.String("upnp-listen", "", "native engine: trusted-LAN UPnP media renderer HTTP address, for example 192.0.2.10:49494; also answers SSDP on UDP 1900; no authentication")
	localListen := flag.String("local-listen", "", "optional trusted-LAN status, hardware volume and setup web UI listen address")
	listAudio := flag.Bool("list-audio", false, "print sound cards and their volume controls as JSON, then exit")
	flag.Parse()
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
	if *localSourceControl && (*engine != "native" || *localListen == "") {
		slog.Error("local-source-control requires the native engine and local-listen")
		os.Exit(2)
	}
	if *pulseListen != "" && (*engine != "native" || !*localSourceControl) {
		slog.Error("pulse-listen requires the native engine and local-source-control")
		os.Exit(2)
	}
	if *esoundListen != "" && (*engine != "native" || !*localSourceControl) {
		slog.Error("esound-listen requires the native engine and local-source-control")
		os.Exit(2)
	}
	if *pulseBufferMS < 0 || *pulseBufferMS > 2000 || *liveDeviceMS != 0 && (*liveDeviceMS < 40 || *liveDeviceMS > 2000) {
		slog.Error("pulse-buffer-ms must be 0 to 2000 and live-device-buffer-ms 0 or 40 to 2000")
		os.Exit(2)
	}
	liveDeviceBufferMS = *liveDeviceMS
	if *esoundBufferMS < 20 || *esoundBufferMS > 2000 || *esoundSocketMS < 0 || *esoundSocketMS > 2000 {
		slog.Error("esound-buffer-ms must be 20 to 2000 and esound-socket-ms 0 to 2000")
		os.Exit(2)
	}
	if *upnpListen != "" && (*engine != "native" || !*localSourceControl) {
		slog.Error("upnp-listen requires the native engine and local-source-control")
		os.Exit(2)
	}
	var irConfig irAmpConfig
	var irAddressValue uint16
	if *irDevice != "" {
		if *volumeControl != "" {
			slog.Error("ir-device and volume-control are alternatives")
			os.Exit(2)
		}
		if *localListen == "" {
			slog.Error("ir-device requires local-listen, which tracks whether audio is playing")
			os.Exit(2)
		}
		address, err := strconv.ParseUint(*irAddress, 0, 16)
		if err != nil {
			slog.Error("ir-address must be a 16-bit number")
			os.Exit(2)
		}
		irAddressValue = uint16(address)
		irConfig = irAmpConfig{Sweep: *irSweep, IdleOff: *irIdleOff, Wake: *irWake, Repeat: *irRepeat,
			InitialSteps: *irInitialSteps, MaxSteps: *irMaxSteps, StateFile: *irState}
		for _, command := range []struct {
			name, text string
			value      *uint8
		}{{"ir-volume-up", *irVolumeUp, &irConfig.VolumeUp}, {"ir-volume-down", *irVolumeDown, &irConfig.VolumeDown},
			{"ir-power-on", *irPowerOn, &irConfig.PowerOn}, {"ir-power-off", *irPowerOff, &irConfig.PowerOff}} {
			value, err := strconv.ParseUint(command.text, 0, 8)
			if err != nil {
				slog.Error("IR command must be an 8-bit number", "flag", command.name)
				os.Exit(2)
			}
			*command.value = uint8(value)
		}
		if *irRepeat < 80*time.Millisecond || *irSweep <= 0 || *irIdleOff < time.Minute || *irWake < 0 || *irMaxSteps < 0 || *irInitialSteps < 0 {
			slog.Error("invalid IR timing or steps: ir-repeat must be at least 80ms and ir-idle-off at least 1m")
			os.Exit(2)
		}
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
	ctx, stop := httpx.SignalContext()
	defer stop()

	inputs, err := inputDevices(*rotaryInput, []inputBinding{
		{"button-input", *buttonInput, actionPlayPause, keyPlayPause},
		{"previous-input", *previousInput, actionPrevious, keyPreviousSong},
		{"next-input", *nextInput, actionNext, keyNextSong},
		{"mute-input", *muteInput, actionMute, keyMute},
	})
	if err != nil {
		slog.Error("configuring inputs", "err", err)
		os.Exit(2)
	}
	// Without a server the player only receives LAN sources.
	server := strings.TrimRight(os.Getenv("SLOPIFY_SERVER"), "/")
	if server == "" && *localListen == "" {
		slog.Error("SLOPIFY_SERVER or -local-listen is required")
		os.Exit(2)
	}
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
		server: server, token: token, name: name, engineName: *engine, localEsoundPort: esoundPort, localState: *localState, localSourceControl: *localSourceControl, localAirPlay: *localAirPlay, localPulse: *localPulse,
		updates:    make(chan *state, 16),
		events:     make(chan event, 16),
		knob:       make(chan knobEvent, 16),
		buttons:    make(chan string, 8),
		volumeWake: make(chan struct{}, 1),
	}
	services := playbackServices{queue: &c.queue, streamURL: c.streamURL, events: c.events, decodedState: func(f format, state string) { c.local.decodedState(f, state) }, formats: func(id uint64, input, output localPCM) {
		e := c.queue.entry(id)
		c.local.nativeFormats(input, output, entryMetadata(&e))
	}, peak: func(v float64, at time.Time) {
		if c.local != nil {
			c.local.nativePeak("slopify", v, at)
		}
	}}
	switch *engine {
	case "mpv":
		c.backend, err = newMPVBackend(services, httpx.EnvOr("SLOPIFY_MPV", "mpv"), sourceOutput)
	case "native":
		// mpv stays optional here: without it, other codecs fail visibly.
		var fallback playbackBackend
		if mpv, mpvErr := newMPVBackend(services, httpx.EnvOr("SLOPIFY_MPV", "mpv"), sourceOutput); mpvErr == nil {
			fallback = mpv
		} else {
			slog.Info("native playback without mpv fallback", "reason", mpvErr)
		}
		c.backend, err = newNativeBackend(services, sourceOutput, fallback)
	default:
		err = fmt.Errorf("unknown playback engine %q", *engine)
	}
	if err != nil {
		slog.Error("configuring playback engine", "err", err)
		os.Exit(2)
	}
	defer c.backend.Close()
	if *volumeControl != "" {
		c.mixer, err = newMixer(ctx, *volumeCard, *volumeControl, *volumeMin, *volumeMax, *volumeInitial)
		if err != nil {
			slog.Error("configuring volume", "err", err)
			os.Exit(2)
		}
	}
	slog.Info("starting", "server", server, "name", name, "output", httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0"))
	if *irDevice != "" {
		c.amp = newIRAmp(irConfig, &lirc.Transmitter{Path: *irDevice, Address: irAddressValue}, c.audioActive)
		go c.amp.run(ctx)
	}
	for path, device := range inputs {
		go followInput(ctx, path, device, c.knob)
	}
	if *localListen != "" {
		if err := c.startLocalUI(ctx, *localListen, httpx.EnvOr("SLOPIFY_OUTPUT", "aplay:hw:0,0")); err != nil {
			slog.Error("starting local UI", "err", err)
			os.Exit(2)
		}
	}

	if *airplayMetadataListen != "" {
		if err := c.startAirPlayMetadata(ctx, *airplayMetadataListen); err != nil {
			slog.Error("AirPlay metadata", "err", err)
			os.Exit(2)
		}
	}
	if c.engineName == "native" && c.sources != nil {
		if err := c.startNativeSources(ctx, *pulseListen, *esoundListen, *pulseBufferMS, *esoundBufferMS, *esoundSocketMS, *upnpListen, httpx.EnvOr("SLOPIFY_PULSE_SINK", "slopify.hifiberry")); err != nil {
			slog.Error("starting native sources", "err", err)
			os.Exit(2)
		}
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
	engineName         string
	sourcePause        chan sourcePauseIntent
	localState         string
	localPulse         bool
	localEsoundPort    int
	airplay            airplayMetadata
	localAirPlay       bool
	localSourceControl bool
	sources            *speakerSources
	name               string
	local              *localStatus
	mixer              *mixer
	amp                *irAmp
	knob               chan knobEvent
	buttons            chan string
	volumeWake         chan struct{}
	server, token      string
	backend            playbackBackend
	updates            chan *state
	events             chan event

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
	c.reportVolume(ctx)
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
		case op := <-c.buttons:
			if c.desired != nil && c.desired.Adopted {
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
			if e.ended && c.desired != nil && c.sess != nil && e.session == c.sess {
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
	if c.sess != nil && c.sess.finished() && c.sess.failure() && c.sess.current() == e.ID && !versionChanged {
		c.recoverOutput(time.Now())
		return
	}
	switch {
	case c.sess == nil || c.sess.finished() || c.sess.current() != e.ID:
		c.outputRetryAt = time.Time{}
		c.outputRetryDelay = 0
		c.start(e.ID, s.Position)
	case versionChanged && math.Abs(c.sess.position()-s.Position) > 2:
		c.start(e.ID, s.Position) // a seek
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

func envDuration(name string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return value
	}
	return fallback
}
