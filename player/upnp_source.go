package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/codemowers/slopify/player/internal/audiodec"
	"github.com/codemowers/slopify/player/internal/pcmring"
	"github.com/codemowers/slopify/player/internal/pulse"
)

const (
	upnpBufferMillis = 500
	upnpMaxURI       = 2048
)

// checkUPNPURI accepts plain HTTP addresses. Where they may connect is enforced at
// dial time, since a name can resolve to anything.
func checkUPNPURI(uri string) error {
	if len(uri) == 0 || len(uri) > upnpMaxURI {
		return errors.New("Invalid stream address")
	}
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil {
		return errors.New("Only plain http stream addresses are accepted")
	}
	return nil
}

// lanOnlyControl refuses connections outside the local network: a LAN client can
// choose this address, and the speaker must not become its way out or into itself.
func lanOnlyControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return fmt.Errorf("stream address %s is outside the local network", host)
	}
	return nil
}

func newUPNPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: lanOnlyControl}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dialer.DialContext, ResponseHeaderTimeout: 10 * time.Second, DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// upnpRenderer plays one stream at a time for the UPnP server.
type upnpRenderer struct {
	lan    *nativeLAN
	client *http.Client

	mu      sync.Mutex
	current *upnpStream
}

func (r *upnpRenderer) CheckURI(uri string) error { return checkUPNPURI(uri) }

func (r *upnpRenderer) Play(uri string) error {
	if err := checkUPNPURI(uri); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		if r.current.uri == uri && !r.current.ended() {
			return nil
		}
		r.current.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &upnpStream{uri: uri, cancel: cancel}
	r.current = stream
	go r.fetch(ctx, stream)
	return nil
}

func (r *upnpRenderer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		r.current.Close()
		r.current = nil
	}
}

func (r *upnpRenderer) Playing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil && !r.current.ended()
}

func (r *upnpRenderer) selected() bool {
	key := ""
	r.mu.Lock()
	if r.current != nil {
		key, _, _ = senderKey(r.current.Props(), r.current.Peer())
	}
	r.mu.Unlock()
	s := r.lan.c.sources
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.source(s.Selected)
	return key != "" && p != nil && p.Key == key
}

func (r *upnpRenderer) Volume() (int, bool, bool) {
	mixer := r.lan.c.mixer
	if mixer == nil {
		return 0, false, false
	}
	control := mixer.snapshot()
	return int(senderVolume(control.Level, control)) * 100 / 0x10000, control.Muted, true
}

// SetVolume only moves the hardware while this stream is what the amp is playing.
func (r *upnpRenderer) SetVolume(level int) error {
	mixer := r.lan.c.mixer
	if mixer == nil {
		return errors.New("No hardware volume")
	}
	if !r.selected() {
		return nil
	}
	control := mixer.snapshot()
	if err := mixer.setLevel(context.Background(), senderLevel(uint32(level)*0x10000/100, control)); err != nil {
		return errors.New("Setting volume failed")
	}
	r.lan.c.reportVolume(context.Background())
	return nil
}

func (r *upnpRenderer) fetch(ctx context.Context, stream *upnpStream) {
	defer stream.Close()
	if err := r.run(ctx, stream); err != nil && ctx.Err() == nil {
		// The address can hold a controller's secrets, so only its host is logged.
		slog.Warn("upnp stream ended", "host", stream.hostOnly(), "err", err)
	}
}

func (r *upnpRenderer) run(ctx context.Context, stream *upnpStream) error {
	req, err := http.NewRequestWithContext(ctx, "GET", stream.uri, nil)
	if err != nil {
		return errors.New("invalid stream address")
	}
	req.Header.Set("User-Agent", "Slopify speaker")
	res, err := r.client.Do(req)
	if err != nil {
		return errors.New("connecting failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	dec, err := audiodec.OpenStream(res.Body, res.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	format := dec.Format()
	if err := stream.prepare(format); err != nil {
		return err
	}
	if !r.lan.addStream(stream) {
		return errors.New("no free source slot")
	}
	samples := make([]int32, 2048*format.Channels)
	raw := make([]byte, len(samples)*4)
	for {
		n, err := dec.Read(samples)
		if n > 0 {
			for i, v := range samples[:n] {
				binary.LittleEndian.PutUint32(raw[i*4:], uint32(v))
			}
			if !stream.ring.Write(raw[:n*4]) {
				return nil
			}
		}
		if err != nil {
			stream.ring.Finish()
			<-stream.ring.Done()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// upnpStream is decoded audio on its way to a source card, held as S32LE.
type upnpStream struct {
	uri    string
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	format audiodec.Format
	ring   *pcmring.Ring
}

func (s *upnpStream) prepare(format audiodec.Format) error {
	if format.Rate < 8000 || format.Rate > 384000 || format.Channels < 1 || format.Channels > 8 {
		return errors.New("unsupported stream format")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stream stopped")
	}
	s.format = format
	frame := format.Channels * 4
	size := format.Rate * upnpBufferMillis / 1000 * frame
	s.ring = pcmring.New(size, size/2/frame*frame, frame, s.cancel)
	return nil
}

func (s *upnpStream) hostOnly() string {
	if parsed, err := url.Parse(s.uri); err == nil {
		return parsed.Hostname()
	}
	return ""
}

func (s *upnpStream) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ring == nil {
		return s.closed
	}
	select {
	case <-s.ring.Done():
		return true
	default:
		return false
	}
}

// Close also works before the stream's format is known.
func (s *upnpStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cancel()
	if s.ring != nil {
		s.ring.Close()
	}
}

func (s *upnpStream) Protocol() string { return "upnp" }
func (s *upnpStream) Peer() string     { return s.hostOnly() }
func (s *upnpStream) Props() map[string]string {
	return map[string]string{"application.name": "UPnP stream", "application.process.host": s.hostOnly()}
}

func (s *upnpStream) PCM() livePCM {
	name := map[int]string{16: "S16_LE", 24: "S24_LE", 32: "S32_LE"}[s.format.Bits]
	if name == "" {
		name = "S32_LE"
	}
	return livePCM{name: name, rate: s.format.Rate, channels: s.format.Channels, bytes: 4, bits: s.format.Bits}
}

func (s *upnpStream) Read(p []byte) int { return s.ring.Read(p) }

func (s *upnpStream) Decode(dst []int32, raw []byte) int {
	n := min(len(dst), len(raw)/4)
	for i := range n {
		dst[i] = int32(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return n
}

func (s *upnpStream) Done() <-chan struct{} { return s.ring.Done() }

func (s *upnpStream) Stats() pulse.Stats {
	size, underflows := s.ring.Stats()
	rate := uint64(s.format.Rate) * uint64(s.format.Channels) * 4
	return pulse.Stats{BufferedUsec: uint64(size) * 1_000_000 / rate, TargetUsec: upnpBufferMillis * 1000, Underflows: underflows}
}

func (s *upnpStream) ObserveReceive(observe func(time.Duration)) { s.ring.Observe(observe) }

func (s *upnpStream) SetDeviceDelay(uint64) {}
func (s *upnpStream) Volume() uint32        { return 0 }
func (s *upnpStream) SetVolume(uint32)      {}
