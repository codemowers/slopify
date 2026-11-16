package rtp

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRTCPPacketRoundTripAndMalformed(t *testing.T) {
	want := reportBlock{source: 123, fraction: 64, lost: -3, highest: 65537, jitter: 240, lsr: 9876, dlsr: 65536}
	packet := receiverPacket(456, want, "anonymous", "Kitchen\nSpeaker")
	got, ok := parseControl(packet)
	if !ok || got.reporter != 456 || len(got.blocks) != 1 || got.blocks[0] != want || got.names[456] != "KitchenSpeaker" || got.cnames[456] != "anonymous" {
		t.Fatalf("parsed %+v (%v)", got, ok)
	}
	for size := 0; size < len(packet); size++ {
		_, valid := parseControl(packet[:size])
		if valid && size != 32 {
			t.Fatalf("accepted truncation at %d", size)
		}
	}
	for _, bad := range [][]byte{{0x81, 201, 0xff, 0xff}, append(append([]byte{}, packet...), 0), {0x81, 202, 0, 1, 0, 0, 0, 0}} {
		if _, ok := parseControl(bad); ok {
			t.Fatalf("accepted malformed %x", bad)
		}
	}
}
func TestRTCPSequenceWrapLossDuplicatesAndJitter(t *testing.T) {
	now := time.Now()
	r := packetReception{}
	r.observe(123, 65534, 1000, 48000, now)
	r.observe(123, 0, 1480, 48000, now.Add(10*time.Millisecond)) // One lost packet, wrap.
	b := r.block(now.Add(time.Second))
	if b.highest != 65536 || b.lost != 1 || b.fraction != 85 || b.jitter != 0 {
		t.Fatalf("loss/wrap %+v", b)
	}
	r.observe(123, 65535, 1240, 48000, now.Add(15*time.Millisecond)) // Late packet fills loss.
	r.observe(123, 0, 1480, 48000, now.Add(20*time.Millisecond))     // Duplicate -> signed negative loss.
	b = r.block(now.Add(2 * time.Second))
	if b.lost != -1 || b.fraction != 0 || b.jitter == 0 {
		t.Fatalf("late/duplicate %+v", b)
	}
	r.observe(456, 5, 123, 48000, now.Add(3*time.Second))
	b = r.block(now.Add(4 * time.Second))
	if b.source != 456 || b.highest != 5 || b.lost != 0 {
		t.Fatalf("new SSRC %+v", b)
	}
	// A single implausible sequence jump must not create a huge loss count.
	r.observe(456, 10000, 500, 48000, now.Add(5*time.Second))
	if r.block(now).highest != 5 {
		t.Fatal("unconfirmed restart accepted")
	}
	r.observe(456, 10001, 740, 48000, now.Add(6*time.Second))
	b = r.block(now)
	if b.highest != 10001 || b.lost != 0 {
		t.Fatalf("restart %+v", b)
	}
}
func TestRTCPSenderReceiverIsolationExpiryAndSR(t *testing.T) {
	now := time.Now()
	s := &senderReports{name: "Sender", cname: "sender", receivers: map[string]ReceiverStatus{}}
	s.sent(123, 1000, 48000, 960, now)
	sr, ok := parseControl(s.packet(now.Add(time.Second)))
	if !ok || !sr.sr || sr.reporter != 123 || sr.stamp != uint32(ntp(now.Add(time.Second))>>16) {
		t.Fatalf("SR %+v", sr)
	}
	receiver := &receiverReporter{id: 456, name: "Kitchen", cname: "kitchen", source: netip.MustParseAddr("192.168.1.1")}
	receiver.observe(123, 1, 1000, 48000, now)
	receiver.receive(s.packet(now.Add(time.Second)), receiver.source, now.Add(time.Second))
	rr, ok := parseControl(receiver.packet(now.Add(2 * time.Second)))
	if !ok || rr.blocks[0].lsr != sr.stamp || rr.blocks[0].dlsr != 65536 {
		t.Fatalf("SR timing %+v", rr)
	}
	ip := netip.MustParseAddr("192.168.1.2")
	s.receive(receiverPacket(456, reportBlock{source: 999, highest: 1}, "wrong", "Wrong sender"), ip, now)
	if entries, _ := s.status(now); len(entries) != 0 {
		t.Fatal("report for other group sender admitted")
	}
	s.receive(receiver.packet(now.Add(2*time.Second)), ip, now.Add(2*time.Second))
	s.sent(123, 97000, 48000, 960, now.Add(2*time.Second))
	entries, _ := s.status(now.Add(2 * time.Second))
	if len(entries) != 1 || entries[0].Name != "Kitchen" || !entries[0].Receiving {
		t.Fatalf("inventory %+v", entries)
	}
	entries, _ = s.status(now.Add(30 * time.Second))
	if len(entries) != 1 || entries[0].Receiving {
		t.Fatalf("stale %+v", entries)
	}
	entries, _ = s.status(now.Add(100 * time.Second))
	if len(entries) != 0 {
		t.Fatal("expired reporter retained")
	}
	// A compound BYE with an RR must remove the reporter without readding it.
	packet := receiverPacket(456, reportBlock{source: 123}, "kitchen", "Kitchen")
	s.receive(packet, ip, now)
	bye := []byte{0x81, 203, 0, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(bye[4:], 456)
	s.receive(append(packet, bye...), ip, now)
	if entries, _ := s.status(now); len(entries) != 0 {
		t.Fatal("BYE reporter retained")
	}
}
func TestRTCPReceiverInventoryBound(t *testing.T) {
	now := time.Now()
	s := &senderReports{receivers: map[string]ReceiverStatus{}}
	s.sent(123, 1, 48000, 1, now)
	ip := netip.MustParseAddr("192.168.1.2")
	for id := uint32(1000); id < 1100; id++ {
		s.receive(receiverPacket(id, reportBlock{source: 123}, "receiver", "Receiver"), ip, now)
	}
	entries, _ := s.status(now)
	if len(entries) != reportLimit {
		t.Fatalf("bound %d", len(entries))
	}
	s.sent(999, 1, 48000, 1, now)
	entries, _ = s.status(now)
	if len(entries) != 0 {
		t.Fatal("old-format reporters retained")
	}
}

func TestRTCPUnicodeNameFitsSDES(t *testing.T) {
	packet := receiverPacket(456, reportBlock{source: 123}, "anonymous", strings.Repeat("🎵", 96))
	c, ok := parseControl(packet)
	if !ok || !utf8.ValidString(c.names[456]) || len(c.names[456]) > 255 {
		t.Fatalf("bad SDES name: %+v", c)
	}
}

func TestSenderByeEndsReceiverStream(t *testing.T) {
	r := &senderReports{cname: "c", name: "n", receivers: map[string]ReceiverStatus{}}
	if r.leave() {
		t.Fatal("a sender that never sent has nothing to leave")
	}
	now := time.Now()
	r.sent(0x1234, 100, 48000, 400, now)
	if !r.leave() {
		t.Fatal("bye not arranged")
	}
	c, ok := parseControl(r.packet(now))
	if !ok || len(c.bye) != 1 || c.bye[0] != 0x1234 || c.reporter != 0x1234 {
		t.Fatalf("bye packet %+v %v", c, ok)
	}
	if r.packet(now) != nil || r.leaving() {
		t.Fatal("a sender that said goodbye stays quiet until it sends again")
	}
	var got []uint32
	rr := &receiverReporter{source: netip.MustParseAddr("192.168.1.2"), bye: func(id uint32) { got = append(got, id) }}
	r.sent(0x1234, 200, 48000, 400, now)
	r.leave()
	rr.receive(r.packet(now), netip.MustParseAddr("192.168.1.3"), now)
	rr.receive(r.packet(now), netip.MustParseAddr("192.168.1.2"), now)
	r.sent(0x1234, 300, 48000, 400, now)
	r.leave()
	rr.receive(r.packet(now), netip.MustParseAddr("192.168.1.2"), now)
	if len(got) != 1 || got[0] != 0x1234 {
		t.Fatalf("bye from the announced source only: %v", got)
	}
}

func TestTopologyReportRoundTrip(t *testing.T) {
	payload := []byte(`{"id":"0123456789abcdef","eligible":false}`)
	if (14+len(payload))%4 == 0 {
		payload = append(payload, byte(32))
	}
	for _, base := range [][]byte{nil, receiverPacket(55, reportBlock{source: 42}, "receiver", "Speaker")} {
		packet := appendTopology(base, 99, payload)
		parsed, ok := parseControl(packet)
		if !ok || string(parsed.topology) != string(payload) {
			t.Fatalf("invalid topology packet: %+v", parsed)
		}
		if len(base) > 0 && (parsed.reporter != 55 || len(parsed.blocks) != 1) {
			t.Fatal("changed receiver report")
		}
		bad := append([]byte(nil), packet...)
		bad[len(bad)-1] = 1 // Payload length leaves padding which must be zero.
		if _, ok := parseControl(bad); ok {
			t.Fatal("accepted nonzero padding")
		}
		for n := 1; n < 4; n++ {
			if _, ok := parseControl(packet[:len(packet)-n]); ok {
				t.Fatal("accepted truncation")
			}
		}
	}
	if packet := appendTopology(nil, 1, make([]byte, 2049)); len(packet) != 0 {
		t.Fatal("oversize topology")
	}
}
