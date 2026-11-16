// Package rtp is a playback-only receiver for multicast RTP audio announced over
// SAP, as sent by PulseAudio's module-rtp-send and PipeWire's module-rtp-sink. It
// accepts uncompressed L16 and L24, plus the non-standard L32 of Slopify's own sender. Like the other LAN receivers it has no
// authentication: announcements and packets are believed by source address alone, so
// it belongs on a trusted LAN. Multicast cannot be held back and nothing here resamples,
// so clock drift between sender and DAC is absorbed by the buffer until it overflows or
// runs dry, which is an occasional audible jump.
package rtp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/codemowers/slopify/player/internal/cleantext"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemowers/slopify/player/internal/pcmring"
)

const (
	sapPort     = 9875
	sapMaxBytes = 4096
	sapMime     = "application/sdp\x00"
	nameMax     = 96

	maxSessions = 8
	maxPacket   = 9000
	minRate     = 8000
	maxRate     = 192000
	maxChannels = 8

	idleAfter   = 3 * time.Second  // No packets: the stream has ended.
	expireAfter = 90 * time.Second // No announcements: the session has gone.
	retryAfter  = 5 * time.Second  // After the source slots were full.
	ssrcHold    = time.Second      // Before a new sender identity may take over.
)

// DefaultBufferMillis is the delay a stream starts at. It holds twice this, leaving
// equal room for drift in either direction.
const DefaultBufferMillis = 200

// SDP attributes of Slopify's own sender. Other receivers ignore them.
const (
	attrTitle    = "a=x-slopify-title:"
	attrArtist   = "a=x-slopify-artist:"
	attrAlbum    = "a=x-slopify-album:"
	attrInstance = "a=x-slopify-instance:"
)

// markerTrim is how far above its intended delay a stream may be at a track change
// before it is brought back.
const markerTrim = 50 * time.Millisecond

var sapGroup = netip.AddrPortFrom(netip.MustParseAddr("224.0.0.56"), sapPort)

// Sample encodings.
const (
	EncodingL16 = "L16"
	EncodingL24 = "L24"
	EncodingL32 = "L32" // Not a registered format: only this receiver understands it.
)

// Description is what a session announces about itself.
type Description struct {
	Name           string
	Group          netip.Addr
	Port           uint16
	Payload        uint8
	Encoding       string
	Rate, Channels int
}

func (d Description) sampleBytes() int {
	switch d.Encoding {
	case EncodingL24:
		return 3
	case EncodingL32:
		return 4
	}
	return 2
}

// Metadata may change during a session without disturbing its audio.
type Metadata struct {
	Title, Artist, Album string
	// Instance identifies a Slopify sender, so that a player ignores itself.
	Instance string
}

func (d Description) groupPort() netip.AddrPort { return netip.AddrPortFrom(d.Group, d.Port) }

// parseSAP returns the SDP text of an announcement and whether it withdraws the session.
func parseSAP(packet []byte) (sdp []byte, deletion bool, err error) {
	if len(packet) < 4 || len(packet) > sapMaxBytes {
		return nil, false, errors.New("bad announcement size")
	}
	flags := packet[0]
	if flags>>5 != 1 {
		return nil, false, errors.New("unsupported SAP version")
	}
	if flags&0x03 != 0 {
		return nil, false, errors.New("encrypted or compressed announcement")
	}
	header := 4 + int(packet[1])*4 + 4
	if flags&0x10 != 0 {
		header += 12
	}
	if len(packet) < header {
		return nil, false, errors.New("short announcement")
	}
	body := packet[header:]
	if bytes.HasPrefix(body, []byte(sapMime)) {
		body = body[len(sapMime):]
	} else if !bytes.HasPrefix(body, []byte("v=0")) {
		return nil, false, errors.New("announcement is not SDP")
	}
	return bytes.TrimRight(body, "\x00"), flags&0x04 != 0, nil
}

// parseSDP accepts one IPv4 multicast audio stream in a format this receiver can play.
func parseSDP(text []byte) (Description, Metadata, error) {
	var d Description
	var meta Metadata
	var rtpmap string
	haveMedia := false
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "s="):
			d.Name = cleanName(line[2:])
		case strings.HasPrefix(line, attrTitle):
			meta.Title = cleanName(line[len(attrTitle):])
		case strings.HasPrefix(line, attrArtist):
			meta.Artist = cleanName(line[len(attrArtist):])
		case strings.HasPrefix(line, attrAlbum):
			meta.Album = cleanName(line[len(attrAlbum):])
		case strings.HasPrefix(line, attrInstance):
			meta.Instance = cleanName(line[len(attrInstance):])
		case strings.HasPrefix(line, "c=IN IP4 "):
			address, _, _ := strings.Cut(line[len("c=IN IP4 "):], "/")
			if group, err := netip.ParseAddr(address); err == nil {
				d.Group = group
			}
		case strings.HasPrefix(line, "m="):
			if haveMedia {
				continue // Only the first stream is played.
			}
			fields := strings.Fields(line[2:])
			if len(fields) != 4 || fields[0] != "audio" || fields[2] != "RTP/AVP" {
				return d, meta, errors.New("not an RTP audio stream")
			}
			port, err := strconv.ParseUint(fields[1], 10, 16)
			if err != nil {
				return d, meta, errors.New("bad port")
			}
			payload, err := strconv.ParseUint(fields[3], 10, 7)
			if err != nil {
				return d, meta, errors.New("bad payload type")
			}
			d.Port, d.Payload, haveMedia = uint16(port), uint8(payload), true
		case strings.HasPrefix(line, "a=rtpmap:") && haveMedia && rtpmap == "":
			id, value, _ := strings.Cut(line[len("a=rtpmap:"):], " ")
			if id == strconv.Itoa(int(d.Payload)) {
				rtpmap = value
			}
		}
	}
	if !haveMedia || rtpmap == "" {
		return d, meta, errors.New("no media description")
	}
	parts := strings.Split(rtpmap, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return d, meta, errors.New("bad rtpmap")
	}
	d.Encoding = parts[0]
	if d.Encoding != EncodingL16 && d.Encoding != EncodingL24 && d.Encoding != EncodingL32 {
		return d, meta, fmt.Errorf("unsupported encoding %q", cleanName(d.Encoding))
	}
	var err error
	if d.Rate, err = strconv.Atoi(parts[1]); err != nil || d.Rate < minRate || d.Rate > maxRate {
		return d, meta, errors.New("unsupported rate")
	}
	d.Channels = 1
	if len(parts) == 3 {
		if d.Channels, err = strconv.Atoi(parts[2]); err != nil || d.Channels < 1 || d.Channels > maxChannels {
			return d, meta, errors.New("unsupported channel count")
		}
	}
	if !d.Group.Is4() || !d.Group.IsMulticast() {
		return d, meta, errors.New("address is not IPv4 multicast")
	}
	if d.Port < 1024 || d.Port == sapPort {
		return d, meta, errors.New("unusable port")
	}
	if d.Name == "" {
		d.Name = "RTP sender"
	}
	return d, meta, nil
}

func cleanName(name string) string { return cleantext.Line(name, nameMax) }

// rtpPayload returns a packet's fields and audio, rejecting anything malformed.
func rtpPayload(packet []byte) (payloadType uint8, timestamp, ssrc uint32, audio []byte, ok bool) {
	if len(packet) < 12 || packet[0]>>6 != 2 {
		return 0, 0, 0, nil, false
	}
	end := len(packet)
	if packet[0]&0x20 != 0 {
		padding := int(packet[end-1])
		if padding == 0 || padding > end-12 {
			return 0, 0, 0, nil, false
		}
		end -= padding
	}
	start := 12 + int(packet[0]&0x0f)*4
	if packet[0]&0x10 != 0 {
		if start+4 > end {
			return 0, 0, 0, nil, false
		}
		start += 4 + int(binary.BigEndian.Uint16(packet[start+2:]))*4
	}
	if start >= end {
		return 0, 0, 0, nil, false
	}
	return packet[1] & 0x7f, binary.BigEndian.Uint32(packet[4:]), binary.BigEndian.Uint32(packet[8:]), packet[start:end], true
}

// Stream is one sender's audio on its way to a source card.
type Stream struct {
	Description
	Peer string

	ring      *pcmring.Ring
	target    int // Bytes.
	lost      atomic.Uint64
	discarded atomic.Uint64
	metadata  atomic.Pointer[Metadata]
}

// Metadata is what the sender last announced about its track.
func (s *Stream) Metadata() Metadata {
	if m := s.metadata.Load(); m != nil {
		return *m
	}
	return Metadata{}
}

func (s *Stream) frameBytes() int { return s.sampleBytes() * s.Channels }

// Read returns whole frames without blocking, and 0 while buffering.
func (s *Stream) Read(p []byte) int { return s.ring.Read(p) }

// Done is closed once the stream has ended.
func (s *Stream) Done() <-chan struct{} { return s.ring.Done() }

// Close ends the stream.
func (s *Stream) Close() { s.ring.Close() }

// ObserveReceive reports the time between packets.
func (s *Stream) ObserveReceive(observe func(time.Duration)) { s.ring.Observe(observe) }

// Stats reports what is queued and what was lost.
func (s *Stream) Stats() pcmring.Stats {
	size, underflows := s.ring.Stats()
	rate := uint64(s.Rate) * uint64(s.frameBytes())
	return pcmring.Stats{BufferedUsec: uint64(size) * 1_000_000 / rate, TargetUsec: uint64(s.target) * 1_000_000 / rate,
		Underflows: underflows, Overflows: s.discarded.Load(), Lost: s.lost.Load()}
}

// Decode widens big-endian samples into the top of int32, which is lossless.
func (s *Stream) Decode(dst []int32, raw []byte) int {
	width := s.sampleBytes()
	n := min(len(dst), len(raw)/width)
	for i := range n {
		switch width {
		case 4:
			dst[i] = int32(binary.BigEndian.Uint32(raw[i*4:]))
		case 3:
			dst[i] = int32(raw[i*3])<<24 | int32(raw[i*3+1])<<16 | int32(raw[i*3+2])<<8
		default:
			dst[i] = int32(binary.BigEndian.Uint16(raw[i*2:])) << 16
		}
	}
	return n
}

// Config describes the receiver.
type Config struct {
	Control   func() ([]byte, uint64)
	OnControl func([]byte, netip.Addr, time.Time)
	// Name is the human-readable RTCP receiver identity.
	Name string
	// Interface joins groups there; nil lets the system choose.
	Interface *net.Interface
	// BufferMillis is the delay a stream starts at; zero means DefaultBufferMillis.
	BufferMillis int
	// OnStream receives each new stream and reports whether it was admitted.
	OnStream func(*Stream) bool
	// Instance is this player's own sender, whose looped-back sessions are ignored.
	Instance string
}

type packetConn interface {
	io.Closer
	ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error)
}

// sessionKey is a card's identity: one sender on one group. The port is left out
// because PulseAudio picks a new random one each time its module loads.
type sessionKey struct{ source, group netip.Addr }

type session struct {
	reporter    *receiverReporter
	config      Config
	description Description
	source      netip.Addr
	metadata    atomic.Pointer[Metadata]

	mu           sync.Mutex
	announced    time.Time
	lastPacket   time.Time
	lastAccepted time.Time
	retryAt      time.Time
	stream       *Stream
	ssrc         uint32
	next         uint32
	synced       bool
	silence      []byte
}

// Receiver follows announcements and joins the groups they name.
type Receiver struct {
	ctx    context.Context
	config Config
	listen func(netip.AddrPort) (packetConn, error)

	mu       sync.Mutex
	stopped  bool
	sessions map[sessionKey]*session
	sockets  map[netip.AddrPort]*groupSocket
}

type groupSocket struct {
	conn packetConn
	refs int
}

// NewReceiver returns a receiver.
func NewReceiver(config Config) *Receiver {
	if config.BufferMillis <= 0 {
		config.BufferMillis = DefaultBufferMillis
	}
	r := &Receiver{config: config, sessions: map[sessionKey]*session{}, sockets: map[netip.AddrPort]*groupSocket{}}
	r.listen = func(group netip.AddrPort) (packetConn, error) {
		return net.ListenMulticastUDP("udp4", config.Interface, net.UDPAddrFromAddrPort(group))
	}
	return r
}

// Start joins the announcement group and runs until ctx ends.
func (r *Receiver) Start(ctx context.Context) error {
	conn, err := r.listen(sapGroup)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	go func() {
		buf := make([]byte, sapMaxBytes+1)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			r.announce(from.Addr().Unmap(), buf[:n], time.Now())
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				r.stop()
				return
			case now := <-ticker.C:
				r.sweep(now)
			}
		}
	}()
	return nil
}

func (r *Receiver) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	for key, s := range r.sessions {
		r.dropLocked(key, s)
	}
}

func (r *Receiver) acquireLocked(group netip.AddrPort) error {
	if socket := r.sockets[group]; socket != nil {
		socket.refs++
		return nil
	}
	conn, err := r.listen(group)
	if err != nil {
		return err
	}
	r.sockets[group] = &groupSocket{conn: conn, refs: 1}
	go r.read(group, conn)
	return nil
}

func (r *Receiver) releaseLocked(group netip.AddrPort) {
	socket := r.sockets[group]
	if socket == nil {
		return
	}
	if socket.refs--; socket.refs == 0 {
		delete(r.sockets, group)
		socket.conn.Close()
	}
}

func (r *Receiver) dropLocked(key sessionKey, s *session) {
	delete(r.sessions, key)
	r.releaseLocked(s.description.groupPort())
	s.finish()
}

func (r *Receiver) read(group netip.AddrPort, conn packetConn) {
	buf := make([]byte, maxPacket+1)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if n <= maxPacket {
			r.packet(group, from.Addr().Unmap(), buf[:n], time.Now())
		}
	}
}

// announce applies one SAP packet.
func (r *Receiver) announce(source netip.Addr, packet []byte, now time.Time) {
	if !source.Is4() || !(source.IsPrivate() || source.IsLinkLocalUnicast()) {
		return
	}
	text, deletion, err := parseSAP(packet)
	if err != nil {
		slog.Debug("rtp announcement ignored", "source", source, "err", err)
		return
	}
	d, meta, err := parseSDP(text)
	if err != nil {
		slog.Debug("rtp session ignored", "source", source, "err", err)
		return
	}
	if r.config.Instance != "" && meta.Instance == r.config.Instance {
		return
	}
	key := sessionKey{source, d.Group}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	existing := r.sessions[key]
	if deletion {
		if existing != nil && existing.description.Port == d.Port {
			r.dropLocked(key, existing)
		}
		return
	}
	if existing != nil {
		if existing.description == d {
			existing.touch(now, meta)
			return
		}
		// A second stream from this sender on this group must not fight a playing one.
		if existing.description.Port != d.Port && existing.recent(now) {
			return
		}
		r.dropLocked(key, existing)
	}
	if len(r.sessions) >= maxSessions {
		slog.Warn("rtp session ignored: too many sessions", "source", source)
		return
	}
	if err := r.acquireLocked(d.groupPort()); err != nil {
		slog.Warn("joining rtp group", "group", d.Group, "port", d.Port, "err", err)
		return
	}
	created := &session{config: r.config, description: d, source: source, announced: now}
	if r.ctx != nil {
		ctx, cancel := context.WithCancel(r.ctx)
		reporter := &receiverReporter{id: randomSSRC(), name: r.config.Name, cname: "slopify-" + NewInstance(), source: source, cancel: cancel}
		reporter.bye = created.bye
		created.reporter = reporter
		go func() {
			failed := false
			controlLoop(ctx, SenderConfig{Group: d.groupPort(), Interface: r.config.Interface, Control: r.config.Control, OnControl: r.config.OnControl}, nil, reporter.packet, reporter.receive, func(err error) {
				if err != nil && !failed {
					slog.Warn("rtp receiver reporting", "group", d.Group, "err", err)
				}
				failed = err != nil
			})
		}()
	}
	created.metadata.Store(&meta)
	r.sessions[key] = created
	slog.Info("rtp session announced", "source", source, "group", d.Group, "port", d.Port, "encoding", d.Encoding, "rate", d.Rate, "channels", d.Channels)
}

// packet hands audio to the session its sender announced; anything else is dropped.
func (r *Receiver) packet(group netip.AddrPort, source netip.Addr, packet []byte, now time.Time) {
	r.mu.Lock()
	s := r.sessions[sessionKey{source, group.Addr()}]
	r.mu.Unlock()
	if s != nil && s.description.Port == group.Port() {
		s.receive(packet, now)
	}
}

// sweep ends streams that stopped sending and forgets sessions that stopped announcing.
func (r *Receiver) sweep(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, s := range r.sessions {
		if s.sweep(now) {
			r.dropLocked(key, s)
		}
	}
}

func (s *session) touch(now time.Time, meta Metadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.announced = now
	s.metadata.Store(&meta)
	if s.stream != nil {
		s.stream.metadata.Store(&meta)
	}
}

func (s *session) recent(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.lastPacket.IsZero() && now.Sub(s.lastPacket) < idleAfter
}

// bye ends the stream when its sender says, with an RTCP BYE, that it has stopped,
// so that the silence which follows is not mistaken for an underrun.
func (s *session) bye(ssrc uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ended := s.stream != nil && s.ssrc == ssrc
	if ended {
		s.stream.ring.Finish()
		s.stream = nil
	}
	slog.Info("rtp bye received", "source", s.source, "group", s.description.Group, "ssrc", fmt.Sprintf("%08x", ssrc), "ended", ended)
}

// finish lets what is buffered play out.
func (s *session) finish() {
	if s.reporter != nil {
		s.reporter.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil {
		s.stream.ring.Finish()
		s.stream = nil
	}
}

func (s *session) sweep(now time.Time) (expired bool) {
	s.mu.Lock()
	idle := now.Sub(s.lastPacket) > idleAfter
	if s.stream != nil && idle {
		s.stream.ring.Finish()
		s.stream = nil
	}
	expired = s.stream == nil && now.Sub(s.announced) > expireAfter
	s.mu.Unlock()
	return expired
}

func (s *session) start(peer string) *Stream {
	d := s.description
	frame := d.sampleBytes() * d.Channels
	target := d.Rate * s.config.BufferMillis / 1000 * frame
	stream := &Stream{Description: d, Peer: peer, target: target}
	stream.ring = pcmring.New(target*2, target, frame, nil)
	stream.metadata.Store(s.metadata.Load())
	return stream
}

// receive runs on the group's reader, so it never blocks on the output.
func (s *session) receive(packet []byte, now time.Time) {
	payloadType, timestamp, ssrc, audio, ok := rtpPayload(packet)
	d := s.description
	frame := d.sampleBytes() * d.Channels
	if !ok || payloadType != d.Payload || len(audio)%frame != 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPacket = now
	if s.stream != nil {
		select {
		case <-s.stream.Done():
			s.stream = nil // The card replaced or closed it.
		default:
		}
	}
	if s.stream == nil {
		if now.Before(s.retryAt) {
			return
		}
		stream := s.start(s.source.String())
		if !s.config.OnStream(stream) {
			s.retryAt = now.Add(retryAfter)
			stream.Close()
			return
		}
		s.stream, s.synced, s.ssrc = stream, false, ssrc
	}
	stream := s.stream
	if ssrc != s.ssrc {
		if now.Sub(s.lastAccepted) < ssrcHold {
			return
		}
		s.ssrc, s.synced = ssrc, false
	}
	if s.reporter != nil {
		s.reporter.observe(ssrc, binary.BigEndian.Uint16(packet[2:]), timestamp, d.Rate, now)
	}
	frames := uint32(len(audio) / frame)
	if s.synced {
		gap := int64(int32(timestamp - s.next))
		capacity := int64(stream.target*2) / int64(frame)
		switch {
		case gap < 0 && -gap <= capacity:
			return // Late or repeated: its place has already been filled.
		case gap > 0 && gap <= capacity:
			// Lost packets keep their time as silence, since the stream must not shift.
			stream.lost.Add(uint64(gap))
			if need := int(gap) * frame; len(s.silence) < need {
				s.silence = make([]byte, need)
			}
			s.write(stream, s.silence[:int(gap)*frame])
		}
		// Anything further away is a restarted sender: carry on from here.
	}
	if s.synced && packet[1]&0x80 != 0 { // The sender marks each new track.
		stream.ring.Trim(stream.target + d.Rate*int(markerTrim/time.Millisecond)/1000*frame)
	}
	s.write(stream, audio)
	s.next, s.synced, s.lastAccepted = timestamp+frames, true, now
}

func (s *session) write(stream *Stream, audio []byte) {
	if discarded, _ := stream.ring.WriteLatest(audio); discarded > 0 {
		stream.discarded.Add(1)
	}
}
