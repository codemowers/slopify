package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Queue gossip is confined to the existing trusted-LAN web listener. These are
// opaque comparison tags and playback telemetry, never signed media URLs.
type queueAdvertisement struct {
	Revision        uint64 `json:"revision,omitempty"`
	ControlPort     uint16 `json:"controlPort,omitempty"`
	PinnedListeners int    `json:"pinnedListeners,omitempty"`
	localIPs        map[string]bool
	PinnedAddress   string   `json:"pinnedAddress,omitempty"`
	PinnedInstance  string   `json:"pinnedInstance,omitempty"`
	PinnedGroup     string   `json:"pinnedGroup,omitempty"`
	Following       string   `json:"following,omitempty"`
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Group           string   `json:"group,omitempty"`
	Command         string   `json:"command,omitempty"`
	Track           string   `json:"track,omitempty"`
	Current         string   `json:"current,omitempty"`
	Ended           []string `json:"ended,omitempty"`
	Position        float64  `json:"position"`
	Eligible        bool     `json:"eligible"`
	Preferred       bool     `json:"preferred"`
	Leader          bool     `json:"leader"`
	Sending         bool     `json:"sending"`
	Multicast       string   `json:"multicast,omitempty"`
}
type queuePeer struct {
	queueAdvertisement
	address  string
	seen     time.Time
	rtcpAt   time.Time
	endpoint string
}
type queueLinkStatus struct {
	Listeners int    `json:"listeners,omitempty"`
	Role      string `json:"role"`
	Leader    string `json:"leader,omitempty"`
	SourceID  string `json:"sourceId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
type queueLink struct {
	mu       sync.Mutex
	self     queueAdvertisement
	peers    []queuePeer
	status   queueLinkStatus
	follow   queuePeer
	endedIDs []uint64
}

func newQueueLink(id string, preferred bool) *queueLink {
	addresses := map[string]bool{}
	if entries, err := net.InterfaceAddrs(); err == nil {
		for _, entry := range entries {
			if prefix, err := netip.ParsePrefix(entry.String()); err == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() {
				addresses[prefix.Addr().String()] = true
			}
		}
	}
	return &queueLink{self: queueAdvertisement{ID: id, Preferred: preferred, localIPs: addresses}, status: queueLinkStatus{Role: "direct", Reason: "Waiting for queue identity"}}
}
func queueTag(group, value string) string {
	sum := sha256.Sum256([]byte(group + "\x00" + value))
	return hex.EncodeToString(sum[:16])
}
func trackTag(group string, id uint64) string {
	if id == 0 {
		return ""
	}
	return queueTag(group, fmt.Sprint(id))
}
func (q *queueLink) ended(id uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.endedIDs = append(q.endedIDs, id)
	if len(q.endedIDs) > 8 {
		q.endedIDs = q.endedIDs[len(q.endedIDs)-8:]
	}
}
func (q *queueLink) snapshot() *queueLinkStatus {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	v := q.status
	return &v
}
func (q *queueLink) source() string {
	if q == nil {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.status.SourceID
}
func (c *controller) queueGossip(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c.link == nil {
		http.NotFound(w, r)
		return
	}
	c.link.mu.Lock()
	v := c.link.self
	c.link.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Prefer senders required by direct pins, then explicitly configured senders,
// then a deterministic ID. Eligibility, not continuing packets, controls leadership.
func electQueueLeader(self queueAdvertisement, peers []queuePeer, now time.Time) queuePeer {
	candidates := []queuePeer{{queueAdvertisement: self}}
	for _, p := range peers {
		if !queuePeerFresh(p, now) || p.ID == self.ID || p.Group != self.Group || p.Command != self.Command {
			continue
		}
		candidates = append(candidates, p)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.PinnedListeners > 0) != (b.PinnedListeners > 0) {
			return a.PinnedListeners > 0
		}
		if a.Preferred != b.Preferred {
			return a.Preferred
		}
		return a.ID < b.ID
	})
	for _, p := range candidates {
		if p.Eligible && p.Group != "" {
			return p
		}
	}
	return queuePeer{}
}
func (c *controller) pollQueuePeers(ctx context.Context) {
	client := &http.Client{Timeout: 800 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext, MaxConnsPerHost: 1, MaxIdleConns: 32, IdleConnTimeout: 10 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var discovered []string
		if c.local != nil && c.local.siblings != nil {
			for _, p := range c.local.siblings.Snapshot() {
				discovered = append(discovered, p.Endpoint)
			}
		}
		c.link.mu.Lock()
		peers := queuePollEndpoints(c.link.peers, discovered, time.Now())
		c.link.mu.Unlock()
		results := make(chan queuePeer, len(peers))
		jobs := make(chan string, len(peers))
		var workers sync.WaitGroup
		for _, endpoint := range peers {
			jobs <- endpoint
		}
		close(jobs)
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for endpoint := range jobs {
					u, err := url.Parse(endpoint)
					if err != nil {
						continue
					}
					addr, err := netip.ParseAddr(u.Hostname())
					if err != nil || !addr.IsPrivate() {
						continue
					}
					req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/api/queue-link", nil)
					if err != nil {
						continue
					}
					resp, err := client.Do(req)
					if err != nil {
						continue
					}
					var p queuePeer
					data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
					resp.Body.Close()
					if err != nil || resp.StatusCode != 200 || len(data) > 4096 || json.Unmarshal(data, &p.queueAdvertisement) != nil {
						continue
					}
					if !validQueueAdvertisement(p.queueAdvertisement) {
						continue
					}
					p.endpoint = endpoint
					p.address = addr.String()
					p.seen = time.Now()
					results <- p
				}
			}()
		}
		workers.Wait()
		close(results)
		fresh := []queuePeer{}
		for p := range results {
			fresh = append(fresh, p)
		}
		c.link.mu.Lock()
		c.link.peers = mergeQueuePeers(c.link.peers, fresh, time.Now())
		c.link.mu.Unlock()
	}
}
func (c *controller) keepQueueFollower(s *state) bool {
	if c.link == nil {
		return false
	}
	c.link.mu.Lock()
	defer c.link.mu.Unlock()
	if c.link.status.SourceID == "" {
		return false
	}
	group := queueTag(c.server, s.QueueGroup)
	valid := s.QueueGroup != "" && s.State == "playing" && s.Adopted && c.link.follow.Group == group && c.link.follow.Command == s.QueueCommand && s.Current != 0
	if !valid {
		c.link.status = queueLinkStatus{Role: "direct", Reason: "Queue changed"}
		c.link.follow = queuePeer{}
	}
	return valid
}

// Owned by controller.run. Network workers only replace the bounded peer list.
func (c *controller) updateQueueLink() {
	if c.link == nil || c.sources == nil {
		return
	}
	q, s := c.link, c.desired
	if s == nil {
		s = &state{}
	}
	c.sources.mu.Lock()
	selected := c.sources.Selected
	pinnedInstance, pinnedGroup, pinnedAddress := "", "", ""
	if p := c.sources.source(selected); c.sources.Mode == "off" && p != nil && p.Protocol == "rtp" {
		pinnedInstance, pinnedGroup, pinnedAddress = p.RTPInstance, p.RTPGroup, p.IP
		if pinnedGroup == "" {
			_, tail, found := strings.Cut(p.Name, " · ")
			if found {
				pinnedGroup = multicastAddress(tail)
			}
		}
	}
	c.sources.mu.Unlock()
	q.mu.Lock()
	oldSource := q.status.SourceID
	self := q.self
	self.PinnedInstance, self.PinnedGroup, self.PinnedAddress = pinnedInstance, pinnedGroup, pinnedAddress
	self.Following = ""
	if self.Command != s.QueueCommand {
		q.endedIDs = nil
	}
	self.Name = c.name
	self.Group = ""
	self.Command = s.QueueCommand
	self.Track = ""
	self.Current = ""
	self.Ended = nil
	self.Position = s.Position
	if s.QueueGroup != "" {
		self.Group = queueTag(c.server, s.QueueGroup)
		self.Track = trackTag(self.Group, s.Current)
	}
	self.Eligible = s.Adopted && s.State == "playing" && selected == "slopify" && self.Group != ""
	self.Sending = false
	self.Multicast = ""
	if c.rtpSender != nil {
		v := c.rtpSender.Status()
		self.Sending = v.Sending
		self.Multicast = v.Group
	}
	if c.sess != nil {
		self.Current = trackTag(self.Group, c.sess.current())
		self.Position = c.sess.position()
	}
	for _, id := range q.endedIDs {
		self.Ended = append(self.Ended, trackTag(self.Group, id))
	}
	members, pinned, listeners := queueDemand(self, q.peers, time.Now())
	self.PinnedListeners = pinned
	leader := electQueueLeader(self, q.peers, time.Now())
	if c.rtpSender != nil {
		c.rtpSender.SetEnabled(self.Preferred || pinned > 0 || (leader.ID == self.ID && members > 0))
	}
	status := queueLinkStatus{Role: "direct", Listeners: listeners}
	if self.Group == "" {
		status.Reason = "Waiting for server queue identity"
	} else if !self.Eligible {
		status.Reason = "Slopify is not playing or selected"
	} else if leader.ID == self.ID {
		if members > 0 || pinned > 0 {
			status.Role = "leader"
			status.Leader = c.name
		}
	} else if leader.ID != "" {
		status.Reason = "Waiting for multicast from " + leader.Name
	}
	// Only follow a peer that itself elected to send, and whose matching RTP
	// session has received fresh audio. Never follow another follower.
	continuing := oldSource != "" && q.follow.ID == leader.ID && q.follow.Group == self.Group && q.follow.Command == self.Command
	q.mu.Unlock()
	source := ""
	if self.Eligible && pinned == 0 && leader.ID != self.ID && leader.Leader && (leader.Sending || continuing) && (continuing || leader.Track == self.Track || leader.Current == self.Track) {
		c.sources.mu.Lock()
		for _, p := range c.sources.Sources {
			if p.Protocol != "rtp" || p.RTPInstance != leader.ID || p.RTPGroup != leader.Multicast {
				continue
			}
			if p.IP != leader.address {
				status.Reason = "Direct playback: multicast and discovery addresses differ"
			} else if !queueInputReady(p, oldSource, continuing, time.Now()) {
				status.Reason = "Direct playback: waiting for fresh multicast audio from " + leader.Name
			} else {
				source = p.ID
				break
			}
		}
		c.sources.mu.Unlock()
	}
	if source != "" {
		status = queueLinkStatus{Role: "follower", Leader: leader.Name, SourceID: source}
		self.Following = leader.ID
	}
	if pinned > 0 && source == "" && selected == "slopify" {
		status.Role = "sender"
		status.Reason = "Keeping this sender available for pinned listeners"
	}
	self.Leader = leader.ID == self.ID && self.Eligible
	if !multicastSourceEligible(selected, source) {
		self.Sending = false
		self.Multicast = ""
	}
	q.mu.Lock()
	if queueTopologyKey(self) != queueTopologyKey(q.self) {
		self.Revision = q.self.Revision + 1
	}
	q.self = self
	q.status = status
	q.follow = leader
	q.mu.Unlock()
	if source != "" {
		if c.sess != nil {
			c.stop()
		}
	} else if oldSource != "" {
		c.reconcile(s)
	}
}
func (c *controller) reportQueueFollower() {
	if c.link == nil || c.desired == nil {
		return
	}
	c.link.mu.Lock()
	follow := c.link.follow
	source := c.link.status.SourceID
	c.link.mu.Unlock()
	if source == "" || time.Since(follow.seen) > 3*time.Second {
		return
	}
	s := c.desired
	track := trackTag(follow.Group, s.Current)
	ended := false
	for _, tag := range follow.Ended {
		if tag == track {
			ended = true
			break
		}
	}
	if follow.Current != track && !ended {
		return
	}
	c.report(map[string]any{"version": s.Version, "entry": s.Current, "state": "playing", "position": follow.Position, "ended": ended})
}

func validQueueAdvertisement(p queueAdvertisement) bool {
	if p.PinnedListeners < 0 || p.PinnedListeners > 32 || len(p.ID) != 16 || len(p.PinnedAddress) > 45 || len(p.PinnedInstance) > 16 || len(p.PinnedGroup) > 64 || len(p.Following) > 16 || len(p.Name) > 128 || len(p.Group) > 64 || len(p.Command) > 64 || len(p.Track) > 64 || len(p.Current) > 64 || len(p.Ended) > 8 || math.IsNaN(p.Position) || math.IsInf(p.Position, 0) || p.Position < 0 {
		return false
	}
	if _, err := hex.DecodeString(p.ID); err != nil {
		return false
	}
	for _, tag := range p.Ended {
		if len(tag) > 64 {
			return false
		}
	}
	return true
}

// A pin is independent of queue eligibility and remains demand through silence.
// Count each receiving player once even if it also advertises queue membership.
func queueDemand(self queueAdvertisement, peers []queuePeer, now time.Time) (members, pinned, listeners int) {
	seen := map[string]bool{}
	for _, p := range peers {
		if p.ID == self.ID || seen[p.ID] || !queuePeerFresh(p, now) {
			continue
		}
		seen[p.ID] = true
		member := p.Eligible && self.Group != "" && p.Group == self.Group && p.Command == self.Command
		group := multicastAddress(self.Multicast)
		pin := group != "" && multicastAddress(p.PinnedGroup) == group && (p.PinnedInstance == self.ID || self.localIPs[p.PinnedAddress])
		if member {
			members++
		}
		if pin {
			pinned++
		}
		if pin || p.Following == self.ID {
			listeners++
		}
	}
	return
}
func mergeQueuePeers(old, fresh []queuePeer, now time.Time) []queuePeer {
	merged := make(map[string]queuePeer)
	for _, p := range old {
		if now.Sub(p.seen) <= 90*time.Second {
			merged[p.ID] = p
		}
	}
	for _, p := range fresh {
		if old, ok := merged[p.ID]; ok {
			if p.Revision < old.Revision {
				continue
			}
			if p.rtcpAt.Before(old.rtcpAt) {
				p.rtcpAt = old.rtcpAt
			}
			if p.endpoint == "" {
				p.endpoint = old.endpoint
			}
		}
		merged[p.ID] = p
	}
	result := make([]queuePeer, 0, min(32, len(merged)))
	// Fresh responses take precedence when discovery has a lot of churn.
	for _, p := range fresh {
		if _, ok := merged[p.ID]; ok && len(result) < 32 {
			result = append(result, merged[p.ID])
			delete(merged, p.ID)
		}
	}
	for _, p := range merged {
		if len(result) < 32 {
			result = append(result, p)
		}
	}
	return result
}

// A known input may briefly disappear when RTP changes format at a track
// boundary. Keep the route, but never turn this grace into indefinite silence.
func queueInputReady(p *speakerSource, previous string, continuing bool, now time.Time) bool {
	if p.SignalAt.IsZero() {
		return false
	}
	if continuing && p.ID == previous {
		return now.Sub(p.SignalAt) < 3*time.Second
	}
	return p.Connected && now.Sub(p.SignalAt) < time.Second
}

func multicastAddress(value string) string {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		if endpoint, e := netip.ParseAddrPort(value); e == nil {
			addr = endpoint.Addr()
		}
	}
	if addr.Is4() && addr.IsMulticast() {
		return addr.String()
	}
	return ""
}

// A sibling using an incoming source cannot supply a selectable multicast input.
// Keep remembered sources internally so pins and ordering survive its return.
func (q *queueLink) unavailableSenders(now time.Time) map[string]bool {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	result := map[string]bool{}
	for _, p := range q.peers {
		if queuePeerFresh(p, now) && p.address != "" {
			result[p.address] = !p.Eligible || p.Following != ""
		}
	}
	return result
}

// RTCP intervals reach 7.5 seconds; permit two lost reports without an election.
// HTTP-only peers retain the short failure detector. Remember endpoints longer
// for rediscovery, without counting an offline peer as an eligible candidate.
func queuePeerFresh(p queuePeer, now time.Time) bool {
	return now.Sub(p.seen) <= 3*time.Second || (!p.rtcpAt.IsZero() && now.Sub(p.rtcpAt) <= 20*time.Second)
}
func queueTopologyKey(p queueAdvertisement) string {
	p.Revision = 0
	p.Position = 0
	data, _ := json.Marshal(p)
	return string(data)
}
func (q *queueLink) controlPacket() ([]byte, uint64) {
	if q == nil {
		return nil, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	data, _ := json.Marshal(q.self)
	return data, q.self.Revision
}
func (q *queueLink) receiveControl(data []byte, ip netip.Addr, now time.Time) {
	if q == nil || len(data) > 2048 || !ip.Is4() || !ip.IsPrivate() {
		return
	}
	var p queuePeer
	if json.Unmarshal(data, &p.queueAdvertisement) != nil || !validQueueAdvertisement(p.queueAdvertisement) {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.ID == q.self.ID {
		return
	}
	p.address = ip.String()
	p.seen, p.rtcpAt = now, now
	if p.ControlPort != 0 {
		p.endpoint = fmt.Sprintf("http://%s:%d", ip, p.ControlPort)
	}
	q.peers = mergeQueuePeers(q.peers, []queuePeer{p}, now)
}
func queuePollEndpoints(peers []queuePeer, discovered []string, now time.Time) []string {
	result := []string{}
	seen := map[string]bool{}
	add := func(endpoint string) {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return
		}
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.Is4() || !ip.IsPrivate() || seen[endpoint] || len(result) >= 32 {
			return
		}
		seen[endpoint] = true
		result = append(result, strings.TrimSuffix(endpoint, "/"))
	}
	for _, p := range peers {
		if now.Sub(p.seen) <= 90*time.Second {
			add(p.endpoint)
		}
	}
	for _, endpoint := range discovered {
		add(endpoint)
	}
	return result
}
