package rtp

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionDescriptionTracksWireFormat(t *testing.T) {
	s, _ := newTestSender()
	if s.SessionDescription() != "" {
		t.Fatal("SDP available before first announcement")
	}
	for _, f := range []Format{{44100, 2, 16}, {96000, 2, 24}} {
		s.apply(senderBegin{format: f})
		text := s.SessionDescription()
		if !strings.Contains(text, "\r\n") {
			t.Fatal("SDP must use CRLF")
		}
		d, _, err := parseSDP([]byte(text))
		encoding, _ := encodingFor(f.Bits)
		if err != nil || d.Rate != f.Rate || d.Encoding != encoding || d.Payload != s.payload || d.Group != s.config.Group.Addr() {
			t.Fatalf("stale or invalid SDP: %+v, %v", d, err)
		}
	}
	s.withdraw()
	if s.SessionDescription() != "" {
		t.Fatal("SDP retained after withdrawal")
	}
}

const pipewireSDP = "v=0\no=user 123 0 IN IP4 192.168.1.5\ns=PipeWire RTP\x01 Stream\nc=IN IP4 239.1.2.3\nt=0 0\nm=audio 46000 RTP/AVP 96\na=recvonly\na=rtpmap:96 L24/48000/2\na=ptime:2\na=mediaclk:sender\na=type:broadcast\n"
const pulseSDP = "v=0\r\no=user 1 0 IN IP4 192.168.1.5\r\ns=Pulseaudio RTP Stream on laptop\r\nc=IN IP4 224.0.0.56\r\nt=0 0\r\na=recvonly\r\nm=audio 46224 RTP/AVP 10\r\na=rtpmap:10 L16/44100/2\r\na=type:broadcast\r\n"

func sap(sdp string, deletion bool) []byte {
	first := byte(1 << 5)
	if deletion {
		first |= 0x04
	}
	return append([]byte{first, 0, 0x12, 0x34, 192, 168, 1, 5}, append([]byte(sapMime), sdp...)...)
}

func packet(pt uint8, ssrc, timestamp uint32, audio []byte) []byte {
	p := make([]byte, 12, 12+len(audio))
	p[0], p[1] = 2<<6, pt
	binary.BigEndian.PutUint32(p[4:], timestamp)
	binary.BigEndian.PutUint32(p[8:], ssrc)
	return append(p, audio...)
}

func TestParseAnnouncements(t *testing.T) {
	text, deletion, err := parseSAP(sap(pipewireSDP, false))
	if err != nil || deletion {
		t.Fatal(err, deletion)
	}
	d, _, err := parseSDP(text)
	want := Description{Name: "PipeWire RTP Stream", Group: netip.MustParseAddr("239.1.2.3"), Port: 46000, Payload: 96, Encoding: EncodingL24, Rate: 48000, Channels: 2}
	if err != nil || d != want {
		t.Fatal(err, d)
	}
	if d, _, err = parseSDP([]byte(pulseSDP)); err != nil || d.Encoding != EncodingL16 || d.Rate != 44100 || d.Port != 46224 || d.Payload != 10 {
		t.Fatal(err, d)
	}
	if _, deletion, _ = parseSAP(sap(pulseSDP, true)); !deletion {
		t.Fatal("deletion flag")
	}
}

func TestRefusesUnplayableAnnouncements(t *testing.T) {
	for name, sdp := range map[string]string{
		"unicast": "c=IN IP4 192.168.1.9\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 L24/48000/2\n",
		"opus":    "c=IN IP4 239.1.2.3\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 opus/48000/2\n",
		"mu-law":  "c=IN IP4 239.1.2.3\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 PCMU/8000/1\n",
		"rate":    "c=IN IP4 239.1.2.3\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 L24/1000000/2\n",
		"chans":   "c=IN IP4 239.1.2.3\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 L24/48000/99\n",
		"sap":     "c=IN IP4 239.1.2.3\nm=audio 9875 RTP/AVP 96\na=rtpmap:96 L24/48000/2\n",
		"video":   "c=IN IP4 239.1.2.3\nm=video 46000 RTP/AVP 96\na=rtpmap:96 L24/48000/2\n",
		"none":    "v=0\n",
	} {
		if _, _, err := parseSDP([]byte(sdp)); err == nil {
			t.Error(name)
		}
	}
	for _, bad := range [][]byte{{}, {0, 0, 0, 0, 1, 2, 3, 4}, {1 << 5, 0, 0, 0}, {1<<5 | 2, 0, 0, 0, 1, 2, 3, 4}} {
		if _, _, err := parseSAP(bad); err == nil {
			t.Error(bad)
		}
	}
}

func TestDecodeIsLosslessAndBigEndian(t *testing.T) {
	dst := make([]int32, 2)
	(&Stream{Description: Description{Encoding: EncodingL16}}).Decode(dst, []byte{0x80, 0x00, 0x7f, 0xff})
	if dst[0] != -0x80000000 || dst[1] != 0x7fff0000 {
		t.Fatalf("%x", dst)
	}
	(&Stream{Description: Description{Encoding: EncodingL24}}).Decode(dst, []byte{0x80, 0, 0, 0x7f, 0xff, 0xff})
	if dst[0] != -0x80000000 || dst[1] != 0x7fffff00 {
		t.Fatalf("%x", dst)
	}
}

func TestRTPHeaderVariants(t *testing.T) {
	p := []byte{2<<6 | 0x20 | 0x10 | 1, 96, 0, 0, 0, 0, 0, 7, 0, 0, 0, 9,
		1, 1, 1, 1, // CSRC
		0, 0, 0, 1, 2, 2, 2, 2, // Extension
		0xaa, 0xbb, 0, 0, 0, 3} // Audio and padding.
	pt, timestamp, ssrc, audio, ok := rtpPayload(p)
	if !ok || pt != 96 || timestamp != 7 || ssrc != 9 || len(audio) != 3 || audio[0] != 0xaa {
		t.Fatal(ok, pt, timestamp, ssrc, audio)
	}
	for _, bad := range [][]byte{nil, make([]byte, 12), {2 << 6, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, {2<<6 | 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 200}} {
		if _, _, _, _, ok := rtpPayload(bad); ok {
			t.Error(bad)
		}
	}
}

type fakeConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *fakeConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func (c *fakeConn) ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error) {
	<-c.closed
	return 0, netip.AddrPort{}, errors.New("closed")
}

type harness struct {
	r       *Receiver
	mu      sync.Mutex
	streams []*Stream
	admit   bool
	joined  map[netip.AddrPort]*fakeConn
}

func newHarness() *harness {
	h := &harness{admit: true, joined: map[netip.AddrPort]*fakeConn{}}
	h.r = NewReceiver(Config{BufferMillis: 100, OnStream: func(s *Stream) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.admit {
			h.streams = append(h.streams, s)
		}
		return h.admit
	}})
	h.r.listen = func(group netip.AddrPort) (packetConn, error) {
		c := &fakeConn{closed: make(chan struct{})}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.joined[group] = c
		return c, nil
	}
	return h
}

const testSDP = "v=0\ns=Laptop\nc=IN IP4 239.1.2.3\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 L16/48000/1\n"

var (
	testGroup = netip.MustParseAddrPort("239.1.2.3:46000")
	laptop    = netip.MustParseAddr("192.168.1.5")
	other     = netip.MustParseAddr("192.168.1.6")
)

func TestOnlyTheAnnouncingSenderIsHeard(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.packet(testGroup, laptop, packet(96, 1, 0, []byte{1, 2}), now)
	h.r.announce(netip.MustParseAddr("8.8.8.8"), sap(testSDP, false), now)
	if len(h.r.sessions) != 0 {
		t.Fatal("public source admitted")
	}
	h.r.announce(laptop, sap(testSDP, false), now)
	h.r.packet(testGroup, other, packet(96, 1, 0, []byte{1, 2}), now)
	h.r.packet(testGroup, laptop, packet(97, 1, 0, []byte{1, 2}), now)
	h.r.packet(testGroup, laptop, packet(96, 1, 0, []byte{1}), now)
	if len(h.streams) != 0 {
		t.Fatal("stream from bad packet")
	}
	h.r.packet(testGroup, laptop, packet(96, 1, 0, []byte{1, 2}), now)
	if len(h.streams) != 1 || h.streams[0].Peer != "192.168.1.5" {
		t.Fatal(h.streams)
	}
}

func TestEachSenderOnEachGroupIsItsOwnSession(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	h.r.announce(other, sap(testSDP, false), now)
	h.r.announce(laptop, sap("v=0\nc=IN IP4 239.1.2.4\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 L16/48000/1\n", false), now)
	if len(h.r.sessions) != 3 || len(h.joined) != 2 {
		t.Fatal(len(h.r.sessions), len(h.joined))
	}
	h.r.announce(laptop, sap(testSDP, true), now)
	if len(h.r.sessions) != 2 {
		t.Fatal("deletion ignored")
	}
	select {
	case <-h.joined[testGroup].closed:
		t.Fatal("left a group still in use")
	default:
	}
	h.r.announce(other, sap(testSDP, true), now)
	<-h.joined[testGroup].closed
}

func TestPortChangeKeepsTheSessionButNotOverAPlayingStream(t *testing.T) {
	h := newHarness()
	now := time.Now()
	moved := "v=0\ns=Laptop\nc=IN IP4 239.1.2.3\nm=audio 46002 RTP/AVP 96\na=rtpmap:96 L16/48000/1\n"
	h.r.announce(laptop, sap(testSDP, false), now)
	h.r.packet(testGroup, laptop, packet(96, 1, 0, []byte{1, 2}), now)
	h.r.announce(laptop, sap(moved, false), now.Add(time.Second))
	if len(h.r.sessions) != 1 || h.r.sessions[sessionKey{laptop, testGroup.Addr()}].description.Port != 46000 {
		t.Fatal("playing stream displaced")
	}
	h.r.announce(laptop, sap(moved, false), now.Add(idleAfter+time.Second))
	if h.r.sessions[sessionKey{laptop, testGroup.Addr()}].description.Port != 46002 {
		t.Fatal("port change ignored")
	}
}

func TestLostPacketsBecomeSilenceAndLateOnesAreDropped(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	send := func(ts uint32, sample byte) {
		h.r.packet(testGroup, laptop, packet(96, 1, ts, []byte{sample, sample}), now)
	}
	send(0, 1)
	send(1, 2)
	send(4, 3) // Frames 2 and 3 lost.
	send(2, 9) // Too late.
	send(5, 4)
	stream := h.streams[0]
	if stream.Stats().Lost != 2 {
		t.Fatal(stream.Stats())
	}
	filler := make([]byte, 9600-12) // Reach the start threshold.
	h.r.packet(testGroup, laptop, packet(96, 1, 6, filler), now)
	out := make([]byte, 12)
	if n := stream.Read(out); n != 12 {
		t.Fatal(n)
	}
	if want := []byte{1, 1, 2, 2, 0, 0, 0, 0, 3, 3, 4, 4}; string(out) != string(want) {
		t.Fatal(out)
	}
}

func TestSenderOutrunningTheOutputKeepsDelayBounded(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	block := make([]byte, 960)
	for i := range 30 {
		h.r.packet(testGroup, laptop, packet(96, 1, uint32(i*480), block), now)
	}
	stats := h.streams[0].Stats()
	if stats.Overflows == 0 || stats.BufferedUsec > 200_000 {
		t.Fatal(stats)
	}
}

func TestRestartedSenderIsFollowed(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	h.r.packet(testGroup, laptop, packet(96, 1, 1000, []byte{1, 1}), now)
	h.r.packet(testGroup, laptop, packet(96, 2, 5, []byte{2, 2}), now.Add(time.Millisecond))
	if size, _ := h.streams[0].ring.Stats(); size != 2 {
		t.Fatal("new identity accepted at once")
	}
	h.r.packet(testGroup, laptop, packet(96, 2, 5, []byte{2, 2}), now.Add(2*ssrcHold))
	if size, _ := h.streams[0].ring.Stats(); size != 4 || h.streams[0].Stats().Lost != 0 {
		t.Fatal(size, h.streams[0].Stats())
	}
}

func TestStreamEndsWhenIdleAndReturnsOnNextPacket(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	h.r.packet(testGroup, laptop, packet(96, 1, 0, []byte{1, 1}), now)
	h.r.sweep(now.Add(idleAfter + time.Second))
	stream := h.streams[0]
	stream.Read(make([]byte, 2))
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream stayed open")
	}
	h.r.packet(testGroup, laptop, packet(96, 1, 1, []byte{1, 1}), now.Add(idleAfter+2*time.Second))
	if len(h.streams) != 2 {
		t.Fatal("did not return")
	}
	h.r.sweep(now.Add(expireAfter + time.Minute))
	if len(h.r.sessions) != 0 {
		t.Fatal("unannounced session kept")
	}
	<-h.joined[testGroup].closed
}

func TestFullSlotsBackOff(t *testing.T) {
	h := newHarness()
	h.admit = false
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	calls := 0
	s := h.r.sessions[sessionKey{laptop, testGroup.Addr()}]
	s.config.OnStream = func(*Stream) bool { calls++; return false }
	h.r.packet(testGroup, laptop, packet(96, 1, 0, []byte{1, 1}), now)
	h.r.packet(testGroup, laptop, packet(96, 1, 1, []byte{1, 1}), now.Add(time.Millisecond))
	if calls != 1 {
		t.Fatal(calls)
	}
	h.r.packet(testGroup, laptop, packet(96, 1, 2, []byte{1, 1}), now.Add(retryAfter+time.Millisecond))
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestSessionLimit(t *testing.T) {
	h := newHarness()
	now := time.Now()
	for i := range maxSessions + 2 {
		h.r.announce(netip.AddrFrom4([4]byte{192, 168, 1, byte(10 + i)}), sap(testSDP, false), now)
	}
	if len(h.r.sessions) != maxSessions {
		t.Fatal(len(h.r.sessions))
	}
}

// recordingConn keeps what a sender would have put on the network.
type recordingConn struct {
	mu   sync.Mutex
	sent map[netip.AddrPort][][]byte
	fail bool
}

func (c *recordingConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return 0, errors.New("no route")
	}
	c.sent[addr] = append(c.sent[addr], append([]byte(nil), b...))
	return len(b), nil
}
func (c *recordingConn) Close() error { return nil }
func (c *recordingConn) packets(addr netip.AddrPort) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent[addr]
}

func newTestSender() (*Sender, *recordingConn) {
	conn := &recordingConn{sent: map[netip.AddrPort][][]byte{}}
	s := NewSender(SenderConfig{Name: "Kitchen\nm=video 1 x", Group: netip.MustParseAddrPort("239.1.2.3:46000"), Instance: "abc"})
	s.dial = func() (packetWriter, netip.Addr, error) { return conn, netip.MustParseAddr("192.168.1.5"), nil }
	return s, conn
}

func TestRandomGroupAvoidsFloodedAddresses(t *testing.T) {
	for range 2000 {
		group, err := RandomGroup()
		if err != nil {
			t.Fatal(err)
		}
		a := group.Addr().As4()
		if a[0] != 239 || a[1] == 0 || a[1] >= 128 || a[3] == 0 || a[3] == 255 || group.Port()%2 != 0 || group.Port() < 46000 {
			t.Fatal(group)
		}
	}
}

func TestParseGroup(t *testing.T) {
	if group, err := ParseGroup("239.1.2.3:47000"); err != nil || group.Port() != 47000 {
		t.Fatal(group, err)
	}
	if group, err := ParseGroup("239.1.2.3"); err != nil || group.Port() < 46000 {
		t.Fatal(group, err)
	}
	for _, bad := range []string{"", "x", "192.168.1.5:46000", "239.1.2.3:9875", "239.1.2.3:80", "[ff02::1]:46000"} {
		if _, err := ParseGroup(bad); err == nil {
			t.Error(bad)
		}
	}
}

func TestSenderPacksAtSourcePrecisionAndAReceiverAgrees(t *testing.T) {
	for bits, encoding := range map[int]string{16: EncodingL16, 24: EncodingL24, 28: EncodingL32, 32: EncodingL32} {
		s, conn := newTestSender()
		s.Begin(Format{48000, 2, bits}, Track{Title: "Song\nm=audio 9 x", Artist: "Band", Album: "LP"})
		in := []int32{0x12345678 &^ (1<<(32-min(bits, 32)) - 1), -0x80000000}
		s.Write(in)
		item, _ := s.pop()
		s.apply(*item.begin)
		item, _ = s.pop()
		s.sendAudio(t.Context(), item.audio)

		announced := conn.packets(sapGroup)
		if len(announced) != 1 {
			t.Fatal(len(announced))
		}
		text, deletion, err := parseSAP(announced[0])
		if err != nil || deletion {
			t.Fatal(err)
		}
		d, meta, err := parseSDP(text)
		if err != nil || d.Encoding != encoding || d.Rate != 48000 || d.Channels != 2 || d.Group != netip.MustParseAddr("239.1.2.3") || d.Name != "Kitchenm=video 1 x" {
			t.Fatal(err, d, string(text))
		}
		if meta.Title != "Songm=audio 9 x" || meta.Artist != "Band" || meta.Album != "LP" || meta.Instance != "abc" {
			t.Fatal(meta)
		}
		audio := conn.packets(netip.MustParseAddrPort("239.1.2.3:46000"))
		if len(audio) != 1 || audio[0][1]&0x80 == 0 || audio[0][1]&0x7f != d.Payload {
			t.Fatal(audio)
		}
		_, _, _, payload, ok := rtpPayload(audio[0])
		if !ok {
			t.Fatal("bad packet")
		}
		out := make([]int32, 2)
		(&Stream{Description: d}).Decode(out, payload)
		if out[0] != in[0] || out[1] != in[1] {
			t.Fatalf("%d bits: %x, want %x", bits, out, in)
		}
	}
}

func TestSenderMarksOnlyTrackStartsAndChangesIdentityOnlyWithFormat(t *testing.T) {
	s, conn := newTestSender()
	group := netip.MustParseAddrPort("239.1.2.3:46000")
	send := func() {
		for {
			item, ok := s.pop()
			if !ok {
				return
			}
			if item.begin != nil {
				s.apply(*item.begin)
			} else {
				s.sendAudio(t.Context(), item.audio)
			}
		}
	}
	f := Format{48000, 2, 16}
	s.Begin(f, Track{Title: "A"})
	s.Write(make([]int32, 2))
	s.Write(make([]int32, 2))
	s.Begin(f, Track{Title: "B"})
	s.Write(make([]int32, 2))
	send()
	p := conn.packets(group)
	if len(p) != 3 || p[0][1]&0x80 == 0 || p[1][1]&0x80 != 0 || p[2][1]&0x80 == 0 {
		t.Fatal(p)
	}
	if string(p[0][8:12]) != string(p[2][8:12]) || binary.BigEndian.Uint32(p[2][4:])-binary.BigEndian.Uint32(p[0][4:]) != 2 {
		t.Fatal("gapless track change broke the stream")
	}
	s.Begin(Format{96000, 2, 24}, Track{Title: "C"})
	s.Write(make([]int32, 2))
	send()
	p = conn.packets(group)
	if p[3][1]&0x7f == p[0][1]&0x7f || string(p[3][8:12]) == string(p[0][8:12]) {
		t.Fatal("format change kept its identity")
	}
	if got := conn.packets(sapGroup); len(got) != 3 {
		t.Fatal("each track should announce at once", len(got))
	}
}

func TestSenderNeverBlocksAndDiscardsOldest(t *testing.T) {
	s, _ := newTestSender()
	s.Begin(Format{8000, 1, 16}, Track{})
	s.Write(make([]int32, 8000))
	for range 10 {
		s.Write(make([]int32, 8000))
	}
	status := s.Status()
	if status.Discards == 0 || status.QueueMS > senderQueueMillis || status.QueueMaxMS != senderQueueMillis {
		t.Fatal(status)
	}
	s.Flush()
	if status = s.Status(); status.QueueMS != 0 {
		t.Fatal(status)
	}
	if item, ok := s.pop(); !ok || item.begin == nil {
		t.Fatal("flush lost the track start")
	}
}

func TestSenderPacketsFitAndKeepRealTimePace(t *testing.T) {
	s, conn := newTestSender()
	s.Begin(Format{48000, 2, 24}, Track{})
	s.Write(make([]int32, 48000/10*2)) // 100 ms.
	item, _ := s.pop()
	s.apply(*item.begin)
	item, _ = s.pop()
	began := time.Now()
	s.sendAudio(t.Context(), item.audio)
	if elapsed := time.Since(began); elapsed < 80*time.Millisecond {
		t.Fatal(elapsed)
	}
	for _, p := range conn.packets(netip.MustParseAddrPort("239.1.2.3:46000")) {
		if len(p)-12 > senderMaxPayload || (len(p)-12)%6 != 0 {
			t.Fatal(len(p))
		}
	}
}

func TestSenderSurvivesNetworkFailure(t *testing.T) {
	s, conn := newTestSender()
	conn.fail = true
	s.Begin(Format{48000, 2, 16}, Track{})
	item, _ := s.pop()
	s.apply(*item.begin)
	status := s.Status()
	if status.Errors == 0 || status.Error == "" {
		t.Fatal(status)
	}
	conn.fail = false
	s.retryAt = time.Time{}
	s.sendPacket(make([]byte, 4))
	if status = s.Status(); status.Error != "" || status.Packets != 1 {
		t.Fatal(status)
	}
}

func TestReceiverIgnoresItsOwnSenderAndFollowsTracks(t *testing.T) {
	h := newHarness()
	h.r.config.Instance = "abc"
	now := time.Now()
	sdp := func(title, instance string) string {
		return "v=0\ns=K\nc=IN IP4 239.1.2.3/1\nm=audio 46000 RTP/AVP 96\na=rtpmap:96 L32/48000/1\n" + attrTitle + title + "\n" + attrInstance + instance + "\n"
	}
	h.r.announce(laptop, sap(sdp("A", "abc"), false), now)
	if len(h.r.sessions) != 0 {
		t.Fatal("heard itself")
	}
	h.r.announce(laptop, sap(sdp("A", "def"), false), now)
	h.r.packet(testGroup, laptop, packet(96, 1, 0, make([]byte, 4)), now)
	if len(h.streams) != 1 || h.streams[0].Metadata().Title != "A" || h.streams[0].sampleBytes() != 4 {
		t.Fatal(h.streams)
	}
	h.r.announce(laptop, sap(sdp("B", "def"), false), now.Add(time.Second))
	if len(h.r.sessions) != 1 || h.streams[0].Metadata().Title != "B" {
		t.Fatal("retitling disturbed the stream")
	}
}

func TestMarkerBringsDriftedStreamBackToItsDelay(t *testing.T) {
	h := newHarness()
	now := time.Now()
	h.r.announce(laptop, sap(testSDP, false), now)
	block := make([]byte, 960) // 10 ms of 48 kHz mono L16.
	for i := range 18 {
		h.r.packet(testGroup, laptop, packet(96, 1, uint32(i*480), block), now)
	}
	stream := h.streams[0]
	if stats := stream.Stats(); stats.BufferedUsec != 180_000 {
		t.Fatal(stats)
	}
	marked := packet(96, 1, 18*480, block)
	marked[1] |= 0x80
	h.r.packet(testGroup, laptop, marked, now)
	if stats := stream.Stats(); stats.BufferedUsec != 160_000 || stats.Overflows != 0 {
		t.Fatal(stats)
	}
}

// Even identical SDP names, ports, payload IDs and SSRCs from two hosts must
// remain independent. Removing one announcement must not close the other's socket.
func TestSharedGroupSendersHaveIsolatedAudio(t *testing.T) {
	h := newHarness()
	defer h.r.stop()
	now := time.Now()
	for _, source := range []netip.Addr{laptop, other} {
		h.r.announce(source, sap(testSDP, false), now)
	}
	for i := 0; i < 10; i++ {
		for n, source := range []netip.Addr{laptop, other} {
			audio := make([]byte, 960)
			for j := range audio {
				audio[j] = byte(n + 1)
			}
			h.r.packet(testGroup, source, packet(96, 7, uint32(i*480), audio), now)
		}
	}
	if len(h.streams) != 2 || len(h.joined) != 1 {
		t.Fatalf("streams=%d sockets=%d", len(h.streams), len(h.joined))
	}
	for i, stream := range h.streams {
		buf := make([]byte, 9600)
		if n := stream.Read(buf); n != len(buf) {
			t.Fatalf("sender %d: read %d", i, n)
		}
		for _, v := range buf {
			if v != byte(i+1) {
				t.Fatalf("sender %d got audio from another sender", i)
			}
		}
	}
	h.r.announce(laptop, sap(testSDP, true), now)
	select {
	case <-h.streams[0].Done():
	default:
		t.Fatal("deleted stream still open")
	}
	select {
	case <-h.streams[1].Done():
		t.Fatal("other sender closed")
	default:
	}
	h.r.packet(testGroup, other, packet(96, 7, 4800, make([]byte, 960)), now)
	if len(h.streams) != 2 {
		t.Fatal("other sender recreated")
	}
}

func TestSourceGateWithdrawsAndRestoresSender(t *testing.T) {
	s, conn := newTestSender()
	s.apply(senderBegin{format: Format{Rate: 44100, Channels: 2, Bits: 16}, track: Track{Title: "Track"}})
	s.sendPacket([]byte{0, 0, 0, 0})
	if !s.Status().Sending || s.SessionDescription() == "" {
		t.Fatal("sender did not start")
	}
	before := len(conn.packets(s.config.Group))
	s.Write([]int32{1, 2, 3, 4})
	s.SetSourceEligible(false)
	if s.Status().Sending || s.SessionDescription() != "" {
		t.Fatal("disabled sender still advertised")
	}
	if s.queued != 0 {
		t.Fatal("queued audio retained")
	}
	s.SetEnabled(true) // Explicit sender configuration cannot override this gate.
	s.sendPacket([]byte{0, 0, 0, 0})
	s.announceIfDue(time.Now())
	packets := conn.packets(sapGroup)
	if len(packets) < 2 || packets[len(packets)-1][0]&0x04 == 0 {
		t.Fatal("SAP withdrawal missing")
	}
	if len(conn.packets(s.config.Group)) != before {
		t.Fatal("audio sent while source blocked")
	}
	announcements := len(packets)
	s.announceIfDue(time.Now().Add(time.Minute))
	if len(conn.packets(sapGroup)) != announcements {
		t.Fatal("blocked sender reannounced")
	}
	s.SetSourceEligible(true)
	s.announceIfDue(time.Now().Add(time.Minute))
	packets = conn.packets(sapGroup)
	if packets[len(packets)-1][0]&0x04 != 0 || s.SessionDescription() == "" {
		t.Fatal("sender did not reannounce")
	}
	s.sendPacket([]byte{0, 0, 0, 0})
	if len(conn.packets(s.config.Group)) != before+1 {
		t.Fatal("sender did not resume")
	}
}
