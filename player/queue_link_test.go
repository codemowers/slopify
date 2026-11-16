package main

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/rtp"
)

func TestQueueElection(t *testing.T) {
	now := time.Now()
	self := queueAdvertisement{ID: "b", Group: "queue", Command: "command", Track: "track", Eligible: true}
	peer := queuePeer{queueAdvertisement: queueAdvertisement{ID: "a", Name: "Amp2", Group: "queue", Command: "command", Track: "track", Eligible: true}, seen: now}
	if got := electQueueLeader(self, []queuePeer{peer}, now); got.ID != "a" {
		t.Fatal(got)
	}
	peer.Eligible = false // A leader switched to a laptop; its RTP may still transmit.
	peer.Sending = true
	peer.Leader = true
	if got := electQueueLeader(self, []queuePeer{peer}, now); got.ID != "b" {
		t.Fatal(got)
	}
	peer.Eligible = true
	peer.seen = now.Add(-4 * time.Second)
	if got := electQueueLeader(self, []queuePeer{peer}, now); got.ID != "b" {
		t.Fatal(got)
	}
	peer.seen = now
	peer.Command = "old seek"
	if got := electQueueLeader(self, []queuePeer{peer}, now); got.ID != "b" {
		t.Fatal(got)
	}
	peer.Command = "command"
	peer.Group = "other queue"
	if got := electQueueLeader(self, []queuePeer{peer}, now); got.ID != "b" {
		t.Fatal(got)
	}
	peer.Group = "queue"
	self.Preferred = true
	if got := electQueueLeader(self, []queuePeer{peer}, now); got.ID != "b" {
		t.Fatal(got)
	}
}
func TestQueueFollowerInvalidatedBySeek(t *testing.T) {
	s := &state{QueueGroup: "opaque", QueueCommand: "seek1", Adopted: true, State: "playing", Current: 7}
	c := &controller{server: "https://example.test", link: newQueueLink("self", false)}
	group := queueTag(c.server, s.QueueGroup)
	c.link.status = queueLinkStatus{Role: "follower", SourceID: "lan_1"}
	c.link.follow = queuePeer{queueAdvertisement: queueAdvertisement{Group: group, Command: s.QueueCommand, Track: trackTag(group, 7)}}
	if !c.keepQueueFollower(s) {
		t.Fatal("same command lost follower")
	}
	s.QueueCommand = "seek2"
	if c.keepQueueFollower(s) || c.link.source() != "" {
		t.Fatal("seek retained stale multicast")
	}
}
func TestQueueTagSeparatesServersAndQueues(t *testing.T) {
	if queueTag("https://one", "queue") == queueTag("https://two", "queue") {
		t.Fatal("cross-server collision")
	}
	if trackTag("one", 1) == trackTag("two", 1) {
		t.Fatal("cross-queue collision")
	}
}

func TestQueueAdvertisementAcceptsRTPInstance(t *testing.T) {
	p := queueAdvertisement{ID: rtp.NewInstance(), Name: "Amp2"}
	if !validQueueAdvertisement(p) {
		t.Fatal("rejected RTP identity")
	}
	p.ID = "not-a-valid-node"
	if validQueueAdvertisement(p) {
		t.Fatal("accepted malformed identity")
	}
}

type queueTestSession struct{ closed bool }

func (s *queueTestSession) current() uint64    { return 7 }
func (s *queueTestSession) position() float64  { return 12 }
func (s *queueTestSession) finished() bool     { return false }
func (s *queueTestSession) failure() bool      { return false }
func (s *queueTestSession) started() time.Time { return time.Now() }
func (s *queueTestSession) close()             { s.closed = true }

type queueTestBackend struct{ starts int }

func (b *queueTestBackend) Start(uint64, float64) playbackSession {
	b.starts++
	return &queueTestSession{}
}
func TestQueueFollowerStopsDecoderAndRecovers(t *testing.T) {
	backend := &queueTestBackend{}
	original := &queueTestSession{}
	c := &controller{server: "https://example.test", name: "DAC", link: newQueueLink("bbbbbbbbbbbbbbbb", false), sources: newSpeakerSources(""), backend: backend, sess: original}
	c.desired = &state{QueueGroup: "opaque", QueueCommand: "cmd", Adopted: true, State: "playing", Current: 7, Queue: []entry{{ID: 7}}}
	c.sources.Selected = "slopify"
	group := queueTag(c.server, c.desired.QueueGroup)
	peer := queuePeer{queueAdvertisement: queueAdvertisement{ID: "aaaaaaaaaaaaaaaa", Name: "Amp2", Preferred: true, Group: group, Command: "cmd", Track: trackTag(group, 7), Eligible: true, Leader: true, Sending: true, Multicast: "239.192.1.2:5004"}, address: "192.168.1.2", seen: time.Now()}
	c.link.peers = []queuePeer{peer}
	input := &speakerSource{ID: "lan_1", Protocol: "rtp", RTPInstance: peer.ID, RTPGroup: peer.Multicast, IP: peer.address, Connected: true, SignalAt: time.Now()}
	c.sources.Sources = append(c.sources.Sources, input)
	c.updateQueueLink()
	if !original.closed || c.sess != nil || c.link.source() != "lan_1" || c.sources.Selected != "slopify" {
		t.Fatal("did not follow underneath logical Slopify selection")
	}
	c.link.peers[0].Eligible = false // Old leader still transmits while using its laptop.
	c.updateQueueLink()
	if c.link.source() != "" || c.sess == nil || backend.starts != 1 {
		t.Fatal("did not recover direct playback")
	}
}

func TestPinnedQueueDemandAndPeerGrace(t *testing.T) {
	now := time.Now()
	self := queueAdvertisement{ID: "sender", Group: "queue", Command: "cmd", Track: "track", Multicast: "239.192.1.2:5004"}
	pin := queuePeer{queueAdvertisement: queueAdvertisement{ID: "receiver", PinnedInstance: "sender", PinnedGroup: self.Multicast}, seen: now}
	members, pinned, listeners := queueDemand(self, []queuePeer{pin, pin}, now)
	if members != 0 || pinned != 1 || listeners != 1 {
		t.Fatalf("pin lost or double counted: %d %d %d", members, pinned, listeners)
	}
	peers := mergeQueuePeers([]queuePeer{pin}, nil, now.Add(time.Second))
	if len(peers) != 1 {
		t.Fatal("one missed poll discarded pin")
	}
	if _, n, _ := queueDemand(self, peers, now.Add(4*time.Second)); n != 0 {
		t.Fatal("stale pin retained")
	}
	if len(mergeQueuePeers(peers, nil, now.Add(91*time.Second))) != 0 {
		t.Fatal("peer did not expire")
	}
}
func TestReceiverOnlyAdvertisesPin(t *testing.T) {
	c := &controller{name: "receiver", link: newQueueLink("receiver", false), sources: newSpeakerSources("")}
	c.sources.Mode = "off"
	c.sources.Selected = "lan_1"
	c.sources.Sources = append(c.sources.Sources, &speakerSource{ID: "lan_1", Protocol: "rtp", RTPInstance: "sender", RTPGroup: "239.192.1.2:5004"})
	c.updateQueueLink()
	if c.link.self.PinnedInstance != "sender" || c.link.self.Eligible {
		t.Fatal("receiver-only pin not advertised")
	}
}

func TestPinnedListenersKeepDecoderInsteadOfFollowing(t *testing.T) {
	original := &queueTestSession{}
	c := &controller{server: "https://example.test", name: "DAC", link: newQueueLink("bbbbbbbbbbbbbbbb", false), sources: newSpeakerSources(""), sess: original}
	c.desired = &state{QueueGroup: "opaque", QueueCommand: "cmd", Adopted: true, State: "playing", Current: 7, Queue: []entry{{ID: 7}}}
	c.sources.Selected = "slopify"
	group, _ := rtp.ParseGroup("239.192.1.3:5004")
	c.rtpSender = rtp.NewSender(rtp.SenderConfig{Name: "DAC", Group: group, Instance: c.link.self.ID})
	tag := queueTag(c.server, c.desired.QueueGroup)
	leader := queuePeer{queueAdvertisement: queueAdvertisement{ID: "aaaaaaaaaaaaaaaa", Name: "Amp2", Preferred: true, Group: tag, Command: "cmd", Track: trackTag(tag, 7), Eligible: true, Leader: true, Sending: true, Multicast: "239.192.1.2:5004"}, address: "192.168.1.2", seen: time.Now()}
	pin := queuePeer{queueAdvertisement: queueAdvertisement{ID: "cccccccccccccccc", PinnedInstance: c.link.self.ID, PinnedGroup: group.String()}, seen: time.Now()}
	c.link.peers = []queuePeer{leader, pin}
	c.sources.Sources = append(c.sources.Sources, &speakerSource{ID: "lan_1", Protocol: "rtp", RTPInstance: leader.ID, RTPGroup: leader.Multicast, IP: leader.address, Connected: true, SignalAt: time.Now()})
	c.updateQueueLink()
	if original.closed || c.sess == nil || c.link.source() != "" || c.link.status.Listeners != 1 {
		t.Fatal("pinned listeners lost their decoder")
	}
	c.link.peers = []queuePeer{leader}
	c.updateQueueLink()
	if !original.closed || c.link.source() != "lan_1" {
		t.Fatal("did not resume deduplication after last pin left")
	}
}

func TestQueueFollowerKeepsRouteAcrossNaturalAdvance(t *testing.T) {
	s := &state{QueueGroup: "opaque", QueueCommand: "continuous", Adopted: true, State: "playing", Current: 8}
	c := &controller{server: "https://example.test", link: newQueueLink("self", false)}
	group := queueTag(c.server, s.QueueGroup)
	c.link.status = queueLinkStatus{Role: "follower", SourceID: "lan_1"}
	c.link.follow = queuePeer{queueAdvertisement: queueAdvertisement{Group: group, Command: s.QueueCommand, Track: trackTag(group, 7)}}
	if !c.keepQueueFollower(s) {
		t.Fatal("ordinary track advancement discarded multicast")
	}
	now := time.Now()
	input := &speakerSource{ID: "lan_1", SignalAt: now.Add(-1500 * time.Millisecond)}
	if !queueInputReady(input, "lan_1", true, now) {
		t.Fatal("format-change gap discarded existing route")
	}
	if queueInputReady(input, "", false, now) {
		t.Fatal("new follower admitted without fresh audio")
	}
	if queueInputReady(input, "lan_1", true, now.Add(2*time.Second)) {
		t.Fatal("format-change grace did not expire")
	}
	s.QueueCommand = "seek"
	if c.keepQueueFollower(s) {
		t.Fatal("explicit seek retained obsolete route")
	}
}

func TestPinnedDemandSurvivesSenderRestart(t *testing.T) {
	now := time.Now()
	self := queueAdvertisement{ID: "new-instance", Multicast: "239.192.1.2:5004", localIPs: map[string]bool{"192.168.1.2": true}}
	pin := queuePeer{queueAdvertisement: queueAdvertisement{ID: "receiver", PinnedInstance: "old-instance", PinnedAddress: "192.168.1.2", PinnedGroup: "239.192.1.2"}, seen: now}
	if _, pinned, _ := queueDemand(self, []queuePeer{pin}, now); pinned != 1 {
		t.Fatal("restart lost pinned demand")
	}
	pin.PinnedAddress = "192.168.1.3"
	if _, pinned, _ := queueDemand(self, []queuePeer{pin}, now); pinned != 0 {
		t.Fatal("another sender on the same group matched")
	}
}

func TestMulticastSourceEligibility(t *testing.T) {
	for _, source := range []string{"slopify", "spotify"} {
		if !multicastSourceEligible(source, "") {
			t.Fatalf("%s must be eligible", source)
		}
	}
	for _, source := range []string{"", "lan_1", "lan_2", "pulse", "esound", "rtp", "upnp", "airplay", "chromecast"} {
		if multicastSourceEligible(source, "") {
			t.Fatalf("unimplemented or incoming source allowed: %s", source)
		}
	}
	if multicastSourceEligible("slopify", "lan_1") {
		t.Fatal("multicast-backed Slopify cannot retransmit")
	}
}

func TestPinnedSenderOutranksExplicitSender(t *testing.T) {
	now := time.Now()
	self := queueAdvertisement{ID: "amp2", Group: "queue", Command: "cmd", Eligible: true, Preferred: true}
	amp4 := queuePeer{queueAdvertisement: queueAdvertisement{ID: "amp4", Group: "queue", Command: "cmd", Eligible: true, PinnedListeners: 1}, seen: now}
	if got := electQueueLeader(self, []queuePeer{amp4}, now); got.ID != "amp4" {
		t.Fatal("explicit sender displaced pinned topology", got.ID)
	}
	amp4.Eligible = false
	if got := electQueueLeader(self, []queuePeer{amp4}, now); got.ID != "amp2" {
		t.Fatal("ineligible pinned sender retained leadership")
	}
}
func TestReceivingSiblingUnavailableAsSender(t *testing.T) {
	now := time.Now()
	q := newQueueLink("self", false)
	q.peers = []queuePeer{{queueAdvertisement: queueAdvertisement{ID: "peer", Eligible: true, Following: "leader"}, address: "192.168.1.2", seen: now}}
	if !q.unavailableSenders(now)["192.168.1.2"] {
		t.Fatal("follower remained selectable")
	}
	q.peers[0].Following = ""
	if q.unavailableSenders(now)["192.168.1.2"] {
		t.Fatal("eligible sender hidden")
	}
	q.peers[0].Eligible = false
	if !q.unavailableSenders(now)["192.168.1.2"] {
		t.Fatal("incoming source remained selectable")
	}
	if len(q.unavailableSenders(now.Add(4*time.Second))) != 0 {
		t.Fatal("stale peer prevented source recovery")
	}
}

func TestRTCPMaintainsElectionAndExplicitWithdrawal(t *testing.T) {
	now := time.Now()
	q := newQueueLink("bbbbbbbbbbbbbbbb", false)
	q.self.Group, q.self.Command, q.self.Eligible = "queue", "command", true
	p := queueAdvertisement{ID: "aaaaaaaaaaaaaaaa", Name: "Sender", Group: "queue", Command: "command", Eligible: true, Leader: true, ControlPort: 8080, Revision: 1}
	data, _ := json.Marshal(p)
	q.receiveControl(data, netip.MustParseAddr("192.168.1.2"), now)
	if got := electQueueLeader(q.self, q.peers, now.Add(10*time.Second)); got.ID != p.ID {
		t.Fatal("RTCP peer lost between reports")
	}
	endpoints := queuePollEndpoints(q.peers, nil, now.Add(10*time.Second))
	if len(endpoints) != 1 || endpoints[0] != "http://192.168.1.2:8080" {
		t.Fatalf("lost endpoint without mDNS: %v", endpoints)
	}
	p.Eligible = false
	p.Leader = false
	p.Revision++
	data, _ = json.Marshal(p)
	q.receiveControl(data, netip.MustParseAddr("192.168.1.2"), now.Add(11*time.Second))
	if got := electQueueLeader(q.self, q.peers, now.Add(11*time.Second)); got.ID != q.self.ID {
		t.Fatal("withdrawal did not reelect")
	}
	p.Revision--
	p.Eligible = true
	data, _ = json.Marshal(p)
	q.receiveControl(data, netip.MustParseAddr("192.168.1.2"), now.Add(12*time.Second))
	if q.peers[0].Eligible {
		t.Fatal("old report restored withdrawn sender")
	}
}
func TestRTCPListenerWithdrawalAndTimeout(t *testing.T) {
	now := time.Now()
	q := newQueueLink("bbbbbbbbbbbbbbbb", true)
	q.self.Group, q.self.Command, q.self.Multicast = "queue", "command", "239.192.1.2:5004"
	p := queueAdvertisement{ID: "aaaaaaaaaaaaaaaa", Group: "queue", Command: "command", Eligible: true, Following: q.self.ID, Revision: 1}
	send := func(at time.Time) {
		data, _ := json.Marshal(p)
		q.receiveControl(data, netip.MustParseAddr("192.168.1.2"), at)
	}
	send(now)
	if members, _, listeners := queueDemand(q.self, q.peers, now.Add(10*time.Second)); members != 1 || listeners != 1 {
		t.Fatal("RTCP listener lost between reports")
	}
	if members, _, listeners := queueDemand(q.self, q.peers, now.Add(21*time.Second)); members != 0 || listeners != 0 {
		t.Fatal("vanished listener retained")
	}
	p.Eligible = false
	p.Following = ""
	p.Revision++ // Pinned to Pulse instead.
	send(now.Add(time.Second))
	if members, _, listeners := queueDemand(q.self, q.peers, now.Add(time.Second)); members != 0 || listeners != 0 {
		t.Fatal("background reception counted as demand")
	}
}
func TestQueueControlBoundsAndEndpointValidation(t *testing.T) {
	now := time.Now()
	q := newQueueLink("bbbbbbbbbbbbbbbb", false)
	for _, ip := range []string{"8.8.8.8", "127.0.0.1", "::1"} {
		q.receiveControl([]byte(`{"id":"aaaaaaaaaaaaaaaa"}`), netip.MustParseAddr(ip), now)
	}
	if len(q.peers) != 0 {
		t.Fatal("non-LAN peer accepted")
	}
	got := queuePollEndpoints(nil, []string{"http://192.168.1.2:80", "http://192.168.1.2:80", "http://8.8.8.8", "http://speaker.local", "https://192.168.1.3", "http://user:pass@192.168.1.4", "http://192.168.1.4/private"}, now)
	if len(got) != 1 {
		t.Fatalf("unsafe or duplicate endpoints: %v", got)
	}
}
