package rtp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	senderMaxPayload  = 1400
	senderQueueMillis = 4000 // Room for what a device takes in at once when it opens.
	senderAnnounce    = 5 * time.Second
	senderMinSleep    = 2 * time.Millisecond
	senderFallBehind  = 200 * time.Millisecond
	senderRetry       = 5 * time.Second
	senderFirstPT     = 96
	senderPayloadSpan = 32
)

// Format is the audio a track decodes to.
type Format struct{ Rate, Channels, Bits int }

// Track describes what is playing. It reaches everyone on the LAN in clear text.
type Track struct{ Title, Artist, Album string }

// RandomGroup picks an address and an even port. It stays in the administratively
// scoped range, away from 239.0.0.x and 239.128.0.x, which share Ethernet addresses
// with the link-local block and are flooded to every port.
func RandomGroup() (netip.AddrPort, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return netip.AddrPort{}, err
	}
	second := 1 + b[0]%127 // 1..127: never 0 or 128.
	last := 1 + b[2]%254   // Never .0 or .255.
	port, err := rand.Int(rand.Reader, big.NewInt(4096))
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{239, second, b[1], last}), uint16(46000+port.Int64()*2)), nil
}

// ParseGroup reads ADDRESS or ADDRESS:PORT, choosing a random even port if none is given.
func ParseGroup(text string) (netip.AddrPort, error) {
	group, err := netip.ParseAddrPort(text)
	if err != nil {
		address, addressErr := netip.ParseAddr(text)
		if addressErr != nil {
			return group, fmt.Errorf("not an address: %q", text)
		}
		random, err := RandomGroup()
		if err != nil {
			return group, err
		}
		group = netip.AddrPortFrom(address, random.Port())
	}
	if !group.Addr().Is4() || !group.Addr().IsMulticast() {
		return group, errors.New("not an IPv4 multicast address")
	}
	if group.Port() < 1024 || group.Port() == sapPort {
		return group, errors.New("unusable port")
	}
	return group, nil
}

// NewInstance returns an identity for a sender and the receiver beside it.
func NewInstance() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SenderConfig describes the sender.
type SenderConfig struct {
	// Control carries bounded application topology on the existing RTCP group.
	Control   func() ([]byte, uint64)
	OnControl func([]byte, netip.Addr, time.Time)
	Name      string
	Group     netip.AddrPort
	Interface *net.Interface
	Instance  string
}

// SenderStatus is what the speaker page shows.
type SenderStatus struct {
	Receivers   []ReceiverStatus `json:"receivers"`
	ReportError string           `json:"reportError,omitempty"`
	Name        string           `json:"name"`
	Group       string           `json:"group"`
	Encoding    string           `json:"encoding,omitempty"`
	Rate        int              `json:"rate,omitempty"`
	Channels    int              `json:"channels,omitempty"`
	Sending     bool             `json:"sending"`
	QueueMS     int              `json:"queueMs"`
	QueueMaxMS  int              `json:"queueMaxMs"`
	Packets     uint64           `json:"packets"`
	Bytes       uint64           `json:"bytes"`
	Discards    uint64           `json:"discards"`
	Errors      uint64           `json:"errors"`
	Error       string           `json:"error,omitempty"`
}

type senderItem struct {
	begin *senderBegin
	audio []byte
}

type senderBegin struct {
	format Format
	track  Track
}

type packetWriter interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	Close() error
}

// Sender multicasts decoded audio at real-time pace, however bursty the writes are, and
// never holds up local playback: when its queue is full it discards the oldest audio.
// Uncompressed samples keep the source's precision. Mono stays mono.
type Sender struct {
	reports *senderReports
	config  SenderConfig
	dial    func() (packetWriter, netip.Addr, error)
	wake    chan struct{}
	bye     chan struct{} // wakes the control loop to send a BYE

	mu                 sync.Mutex
	muted              bool // queue sharing can keep the sender idle with one listener
	sourceBlocked      bool // incoming sources cannot be multicast senders
	items              []senderItem
	queued             int
	maxBytes           int
	width              int // Bytes per sample of what is being written.
	flushes            uint64
	active             senderBegin // What is on the wire.
	encoding           string
	lastErr            string
	lastSend           time.Time
	sessionDescription string // Published by the sender goroutine for HTTP readers.
	// Output state, used only by the run goroutine.
	conn      packetWriter
	local     netip.Addr
	retryAt   time.Time
	payload   uint8
	ssrc      uint32
	seq       uint16
	timestamp uint32
	marker    bool
	version   uint64
	nextSAP   time.Time
	next      time.Time
	sessionID uint32

	packets, bytes, discards, errors atomic.Uint64
}

// NewSender returns a sender. Its socket opens on first use, as the network may not
// be up yet when the player starts.
func NewSender(config SenderConfig) *Sender {
	s := &Sender{config: config, wake: make(chan struct{}, 1), bye: make(chan struct{}, 1), payload: senderFirstPT + senderPayloadSpan - 1}
	var id [4]byte
	rand.Read(id[:])
	s.sessionID = binary.BigEndian.Uint32(id[:])
	s.reports = &senderReports{name: config.Name, cname: "slopify-" + NewInstance(), receivers: map[string]ReceiverStatus{}}
	s.dial = func() (packetWriter, netip.Addr, error) { return dialMulticast(config) }
	return s
}

func dialMulticast(config SenderConfig) (packetWriter, netip.Addr, error) {
	local, err := outgoingAddress(config)
	if err != nil {
		return nil, local, err
	}
	// Binding the interface's address also chooses it for multicast on Linux. The
	// system default of one hop keeps the stream on this network segment.
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if err != nil {
		return nil, local, err
	}
	return conn, local, nil
}

func outgoingAddress(config SenderConfig) (netip.Addr, error) {
	if config.Interface != nil {
		addresses, err := config.Interface.Addrs()
		if err != nil {
			return netip.Addr{}, err
		}
		for _, address := range addresses {
			if prefix, err := netip.ParsePrefix(address.String()); err == nil && prefix.Addr().Is4() {
				return prefix.Addr(), nil
			}
		}
		return netip.Addr{}, fmt.Errorf("%s has no IPv4 address", config.Interface.Name)
	}
	// Connecting a UDP socket only asks the routing table; nothing is sent.
	probe, err := net.Dial("udp4", net.UDPAddrFromAddrPort(config.Group).String())
	if err != nil {
		return netip.Addr{}, err
	}
	defer probe.Close()
	return probe.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

func encodingFor(bits int) (string, int) {
	switch {
	case bits <= 16:
		return EncodingL16, 2
	case bits <= 24:
		return EncodingL24, 3
	}
	return EncodingL32, 4
}

func (s *Sender) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Begin starts a track. The first packet of its audio carries the RTP marker, which
// tells a receiver that this is a good place to correct its delay.
func (s *Sender) Begin(format Format, track Track) {
	_, width := encodingFor(format.Bits)
	s.mu.Lock()
	s.width = width
	s.maxBytes = format.Rate * format.Channels * width * senderQueueMillis / 1000
	s.items = append(s.items, senderItem{begin: &senderBegin{format, track}})
	s.mu.Unlock()
	s.notify()
}

// Write queues left-justified samples without blocking.
func (s *Sender) Write(samples []int32) {
	s.mu.Lock()
	width := s.width
	if s.muted || s.sourceBlocked {
		width = 0
	}
	s.mu.Unlock()
	if width == 0 {
		return
	}
	audio := make([]byte, 0, len(samples)*width)
	for _, v := range samples {
		switch width {
		case 2:
			audio = append(audio, byte(v>>24), byte(v>>16))
		case 3:
			audio = append(audio, byte(v>>24), byte(v>>16), byte(v>>8))
		default:
			audio = append(audio, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
		}
	}
	s.mu.Lock()
	if s.muted || s.sourceBlocked {
		s.mu.Unlock()
		return
	}
	s.items = append(s.items, senderItem{audio: audio})
	s.queued += len(audio)
	for i := 0; s.queued > s.maxBytes && i < len(s.items); {
		if s.items[i].begin != nil {
			i++
			continue
		}
		s.queued -= len(s.items[i].audio)
		s.items = append(s.items[:i], s.items[i+1:]...)
		s.discards.Add(1)
	}
	s.mu.Unlock()
	s.notify()
}

// Sender demand never overrides source eligibility. Disabling withdraws SAP,
// discards queued audio, and interrupts an in-flight chunk on the worker.
func (s *Sender) SetEnabled(enabled bool)         { s.setGate(enabled, false) }
func (s *Sender) SetSourceEligible(eligible bool) { s.setGate(eligible, true) }
func (s *Sender) setGate(enabled, source bool) {
	s.mu.Lock()
	wasBlocked := s.muted || s.sourceBlocked
	if source {
		s.sourceBlocked = !enabled
	} else {
		s.muted = !enabled
	}
	blocked := s.muted || s.sourceBlocked
	if blocked && !wasBlocked {
		kept := s.items[:0]
		for _, item := range s.items {
			if item.begin != nil {
				kept = append(kept, item)
			}
		}
		s.items, s.queued = kept, 0
		s.flushes++
		s.lastSend = time.Time{}
		s.sessionDescription = ""
	}
	s.mu.Unlock()
	if blocked && !wasBlocked {
		s.leave()
	}
	if blocked != wasBlocked {
		s.notify()
	}
}
func (s *Sender) enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.muted && !s.sourceBlocked
}

// Flush forgets queued audio, when local playback stops or seeks.
func (s *Sender) Flush() {
	s.mu.Lock()
	kept := s.items[:0]
	for _, item := range s.items {
		if item.begin != nil {
			kept = append(kept, item)
		}
	}
	s.items, s.queued = kept, 0
	s.flushes++
	s.mu.Unlock()
	s.leave()
}

// leave tells receivers, with an RTCP BYE, that the stream has stopped rather than
// stalled.
func (s *Sender) leave() {
	if !s.reports.leave() {
		return
	}
	select {
	case s.bye <- struct{}{}:
	default:
	}
}

// Status reports the sender's state.
func (s *Sender) Status() SenderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := SenderStatus{Name: s.config.Name, Group: s.config.Group.String(), Encoding: s.encoding, Rate: s.active.format.Rate, Channels: s.active.format.Channels,
		Sending: !s.muted && !s.sourceBlocked && time.Since(s.lastSend) < time.Second, Packets: s.packets.Load(), Bytes: s.bytes.Load(), Discards: s.discards.Load(), Errors: s.errors.Load(), Error: s.lastErr}
	if s.maxBytes > 0 {
		status.QueueMaxMS = senderQueueMillis
		status.QueueMS = s.queued * senderQueueMillis / s.maxBytes
	}
	status.Receivers, status.ReportError = s.reports.status(time.Now())
	return status
}

// Start sends until ctx ends.
func (s *Sender) Start(ctx context.Context) {
	go controlLoop(ctx, s.config, s.bye, func(now time.Time) []byte {
		if !s.enabled() && !s.reports.leaving() {
			return nil
		}
		return s.reports.packet(now)
	}, s.reports.receive, s.reports.error)
	go func() {
		s.run(ctx)
		s.withdraw()
	}()
}

func (s *Sender) run(ctx context.Context) {
	ticker := time.NewTicker(senderAnnounce)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.announceIfDue(time.Now())
		item, ok := s.pop()
		if !ok {
			select {
			case <-ctx.Done():
			case <-s.wake:
			case <-ticker.C:
			}
			continue
		}
		if item.begin != nil {
			s.apply(*item.begin)
			continue
		}
		s.sendAudio(ctx, item.audio)
	}
}

func (s *Sender) pop() (senderItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return senderItem{}, false
	}
	item := s.items[0]
	s.items = s.items[1:]
	s.queued -= len(item.audio)
	return item, true
}

// apply makes a track current. A new format gets a new payload type and identity, so
// that a receiver still holding the old announcement refuses its packets.
func (s *Sender) apply(begin senderBegin) {
	encoding, _ := encodingFor(begin.format.Bits)
	s.mu.Lock()
	changed := begin.format != s.active.format
	s.active, s.encoding = begin, encoding
	s.sessionDescription = ""
	s.mu.Unlock()
	if changed {
		s.payload = senderFirstPT + (s.payload-senderFirstPT+1)%senderPayloadSpan
		var id [8]byte
		rand.Read(id[:])
		s.ssrc = binary.BigEndian.Uint32(id[:])
		s.timestamp = binary.BigEndian.Uint32(id[4:])
	}
	s.marker = true
	s.version++
	s.nextSAP = time.Time{}
	s.announceIfDue(time.Now())
}

func (s *Sender) frameBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, width := encodingFor(s.active.format.Bits)
	return width * s.active.format.Channels
}

func (s *Sender) flushCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
}

func (s *Sender) sendAudio(ctx context.Context, audio []byte) {
	frame := s.frameBytes()
	rate := s.activeRate()
	if frame == 0 || rate == 0 {
		return
	}
	chunk := max(senderMaxPayload/frame, 1) * frame
	flushes := s.flushCount() // Audio queued after a flush belongs to the new playback.
	for len(audio) > 0 && ctx.Err() == nil {
		if !s.enabled() || s.flushCount() != flushes {
			s.next = time.Time{}
			return
		}
		payload := audio[:min(chunk, len(audio))]
		audio = audio[len(payload):]
		s.pace(time.Now())
		s.sendPacket(payload)
		frames := uint32(len(payload) / frame)
		s.timestamp += frames
		s.next = s.next.Add(time.Duration(frames) * time.Second / time.Duration(rate))
		s.announceIfDue(time.Now())
	}
}

func (s *Sender) activeRate() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active.format.Rate
}

// pace waits for the packet's time. Short waits collect into one sleep, which keeps
// high rates accurate on coarse timers; after a break the clock restarts from now.
func (s *Sender) pace(now time.Time) {
	if s.next.IsZero() || now.Sub(s.next) > senderFallBehind {
		s.next = now
		return
	}
	if wait := s.next.Sub(now); wait >= senderMinSleep {
		time.Sleep(wait)
	}
}

func (s *Sender) socket(now time.Time) packetWriter {
	if s.conn != nil {
		return s.conn
	}
	if now.Before(s.retryAt) {
		return nil
	}
	conn, local, err := s.dial()
	if err != nil {
		s.fail(now, err)
		return nil
	}
	s.conn, s.local = conn, local
	s.mu.Lock()
	s.lastErr = ""
	s.mu.Unlock()
	return conn
}

// fail reopens the socket shortly: an address change can leave it unusable for good.
func (s *Sender) fail(now time.Time, err error) {
	s.errors.Add(1)
	s.retryAt = now.Add(senderRetry)
	s.mu.Lock()
	first := s.lastErr == ""
	s.lastErr = "Multicast sending failed"
	s.mu.Unlock()
	if first {
		slog.Warn("rtp sender", "group", s.config.Group, "err", err)
	}
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *Sender) sendPacket(audio []byte) {
	if !s.enabled() {
		return
	}
	now := time.Now()
	conn := s.socket(now)
	if conn == nil {
		return // The audio's time still passes, so the stream stays continuous.
	}
	packet := make([]byte, 12, 12+len(audio))
	packet[0] = 2 << 6
	packet[1] = s.payload
	if s.marker {
		packet[1] |= 0x80
		s.marker = false
	}
	binary.BigEndian.PutUint16(packet[2:], s.seq)
	binary.BigEndian.PutUint32(packet[4:], s.timestamp)
	binary.BigEndian.PutUint32(packet[8:], s.ssrc)
	packet = append(packet, audio...)
	s.seq++
	if _, err := conn.WriteToUDPAddrPort(packet, s.config.Group); err != nil {
		s.fail(now, err)
		return
	}
	s.reports.sent(s.ssrc, s.timestamp, s.activeRate(), len(audio), now)
	s.packets.Add(1)
	s.bytes.Add(uint64(len(packet)))
	s.mu.Lock()
	s.lastSend = now
	if s.lastErr != "" {
		s.lastErr = ""
	}
	s.mu.Unlock()
}

func (s *Sender) announceIfDue(now time.Time) {
	if !s.enabled() {
		s.withdraw()
		s.nextSAP = time.Time{}
		s.next = time.Time{}
		return
	}
	if now.Before(s.nextSAP) {
		return
	}
	s.mu.Lock()
	active := s.active
	encoding := s.encoding
	s.mu.Unlock()
	if active.format.Rate == 0 {
		return
	}
	s.nextSAP = now.Add(senderAnnounce)
	conn := s.socket(now)
	if conn == nil {
		return
	}
	sdp := s.sdp(active, encoding, s.local)
	s.mu.Lock()
	s.sessionDescription = strings.ReplaceAll(sdp, "\n", "\r\n")
	s.mu.Unlock()
	if _, err := conn.WriteToUDPAddrPort(s.sapPacket(active, encoding, false), sapGroup); err != nil {
		s.fail(now, err)
	}
}

// withdraw tells receivers the session is over.
func (s *Sender) withdraw() {
	s.mu.Lock()
	active, encoding := s.active, s.encoding
	s.sessionDescription = ""
	s.mu.Unlock()
	if s.conn == nil {
		return
	}
	if active.format.Rate != 0 {
		s.conn.WriteToUDPAddrPort(s.sapPacket(active, encoding, true), sapGroup)
	}
	s.conn.Close()
	s.conn = nil
}

// SessionDescription returns the current on-wire session, or empty before the
// first announcement and after shutdown. It never reads the run goroutine's state.
func (s *Sender) SessionDescription() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.muted || s.sourceBlocked {
		return ""
	}
	return s.sessionDescription
}

func (s *Sender) sapPacket(active senderBegin, encoding string, deletion bool) []byte {
	first := byte(1 << 5)
	if deletion {
		first |= 0x04
	}
	local := s.local
	if !local.Is4() {
		local = netip.IPv4Unspecified()
	}
	source := local.As4()
	packet := []byte{first, 0, byte(s.sessionID >> 8), byte(s.sessionID), source[0], source[1], source[2], source[3]}
	packet = append(packet, sapMime...)
	return append(packet, s.sdp(active, encoding, local)...)
}

// sdp writes an announcement. Every sender-supplied value is cleaned first, as a line
// break would otherwise add fields of its own.
func (s *Sender) sdp(active senderBegin, encoding string, local netip.Addr) string {
	var b strings.Builder
	name := cleanName(s.config.Name)
	if name == "" {
		name = "Slopify"
	}
	fmt.Fprintf(&b, "v=0\no=slopify %d %d IN IP4 %s\ns=%s\n", s.sessionID, s.version, local, name)
	title, artist, album := cleanName(active.track.Title), cleanName(active.track.Artist), cleanName(active.track.Album)
	if artist != "" && title != "" {
		fmt.Fprintf(&b, "i=%s - %s\n", artist, title)
	} else if title != "" {
		fmt.Fprintf(&b, "i=%s\n", title)
	}
	fmt.Fprintf(&b, "c=IN IP4 %s/1\nt=0 0\na=recvonly\nm=audio %d RTP/AVP %d\na=rtpmap:%d %s/%d/%d\na=type:broadcast\na=tool:Slopify\n",
		s.config.Group.Addr(), s.config.Group.Port(), s.payload, s.payload, encoding, active.format.Rate, active.format.Channels)
	for _, attribute := range [][2]string{{attrTitle, title}, {attrArtist, artist}, {attrAlbum, album}, {attrInstance, s.config.Instance}} {
		if attribute[1] != "" {
			b.WriteString(attribute[0] + attribute[1] + "\n")
		}
	}
	return b.String()
}
