package rtp

// RTCP reports describe reception, not whether a receiver selected this stream
// for audible playback. All identities remain untrusted, bounded LAN metadata.
import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const reportLimit = 32
const reportFresh = 20 * time.Second
const reportExpiry = 90 * time.Second

func randomSSRC() uint32 { var b [4]byte; rand.Read(b[:]); return binary.BigEndian.Uint32(b[:]) }
func reportInterval() time.Duration {
	return 2500*time.Millisecond + time.Duration(randomSSRC()%5000)*time.Millisecond
}
func ntp(t time.Time) uint64 {
	return uint64(t.Unix()+2208988800)<<32 | uint64(t.Nanosecond())*(1<<32)/1_000_000_000
}
func controlGroup(group netip.AddrPort) (netip.AddrPort, error) {
	if group.Port() == 65535 || group.Port()+1 == sapPort {
		return netip.AddrPort{}, fmt.Errorf("no usable RTCP port after %d", group.Port())
	}
	return netip.AddrPortFrom(group.Addr(), group.Port()+1), nil
}

// controlLoop owns its sockets, and never performs network IO in an audio
// callback. Failed opens/read/writes retry while playback carries on. A signal on
// wake, which may be nil, sends a packet at once rather than at the next interval.
func controlLoop(ctx context.Context, config SenderConfig, wake <-chan struct{}, build func(time.Time) []byte, receive func([]byte, netip.Addr, time.Time), status func(error)) {
	group, err := controlGroup(config.Group)
	if err != nil {
		status(err)
		return
	}
	controlID := randomSSRC()
	var controlRevision uint64
	var urgent atomic.Bool
	for ctx.Err() == nil {
		var woken chan struct{}
		reader, err := net.ListenMulticastUDP("udp4", config.Interface, net.UDPAddrFromAddrPort(group))
		var writer packetWriter
		if err == nil {
			dialConfig := config
			dialConfig.Group = group
			writer, _, err = dialMulticast(dialConfig)
		}
		if err == nil {
			status(nil)
			woken = make(chan struct{})
			if wake != nil {
				go func() { // Interrupt the read below, so that the packet goes now.
					for {
						select {
						case <-wake:
							urgent.Store(true)
							reader.SetReadDeadline(time.Now())
						case <-woken:
							return
						}
					}
				}()
			}
			next := time.Now().Add(reportInterval() / 2)
			buf := make([]byte, 4097)
			for ctx.Err() == nil {
				now := time.Now()
				var topology []byte
				if config.Control != nil {
					data, revision := config.Control()
					topology = data
					if revision != controlRevision {
						next = now
						controlRevision = revision
					}
				}
				if urgent.Swap(false) {
					next = now
				}
				if !now.Before(next) {
					if packet := appendTopology(build(now), controlID, topology); len(packet) > 0 {
						if udp, ok := writer.(*net.UDPConn); ok {
							udp.SetWriteDeadline(now.Add(200 * time.Millisecond))
						}
						if _, err = writer.WriteToUDPAddrPort(packet, group); err != nil {
							break
						}
					}
					next = now.Add(reportInterval())
				}
				deadline := time.Now().Add(time.Second)
				if next.Before(deadline) {
					deadline = next
				}
				if urgent.Load() {
					continue
				}
				reader.SetReadDeadline(deadline)
				var n int
				var from netip.AddrPort
				n, from, err = reader.ReadFromUDPAddrPort(buf)
				if e, ok := err.(net.Error); ok && e.Timeout() {
					err = nil
					continue
				}
				if err != nil {
					break
				}
				ip := from.Addr().Unmap()
				if n <= 4096 && ip.Is4() && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()) {
					if config.OnControl != nil {
						if packet, valid := parseControl(buf[:n]); valid && len(packet.topology) > 0 {
							config.OnControl(packet.topology, ip, time.Now())
						}
					}
					receive(buf[:n], ip, time.Now())
				}
			}
		}
		if woken != nil {
			close(woken)
		}
		if reader != nil {
			reader.Close()
		}
		if writer != nil {
			writer.Close()
		}
		if ctx.Err() != nil {
			return
		}
		status(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

type reportBlock struct {
	source                     uint32
	fraction                   byte
	lost                       int32
	highest, jitter, lsr, dlsr uint32
}
type controlPacket struct {
	topology []byte
	reporter uint32
	blocks   []reportBlock
	names    map[uint32]string
	cnames   map[uint32]string
	bye      []uint32
	sr       bool
	stamp    uint32
}

// parseControl validates the whole compound before any part changes state.
func parseControl(data []byte) (controlPacket, bool) {
	c := controlPacket{names: map[uint32]string{}, cnames: map[uint32]string{}}
	if len(data) > 4096 || len(data) < 8 || (data[1] != 200 && data[1] != 201) {
		return c, false
	}
	first := true
	for len(data) > 0 {
		if len(data) < 4 || data[0]>>6 != 2 {
			return c, false
		}
		size := (int(binary.BigEndian.Uint16(data[2:])) + 1) * 4
		if size > len(data) || size < 4 {
			return c, false
		}
		p := data[:size]
		data = data[size:]
		if p[0]&32 != 0 {
			padding := int(p[len(p)-1])
			if len(data) != 0 || padding == 0 || padding > len(p)-4 {
				return c, false
			}
			p = p[:len(p)-padding]
		}
		count := int(p[0] & 31)
		switch p[1] {
		case 200, 201:
			offset := 8
			if p[1] == 200 {
				offset = 28
			}
			if len(p) < offset+count*24 {
				return c, false
			}
			if first {
				c.reporter = binary.BigEndian.Uint32(p[4:])
				c.sr = p[1] == 200
				if c.sr {
					c.stamp = binary.BigEndian.Uint32(p[10:14])
				}
				for i := 0; i < count; i++ {
					b := p[offset+i*24:]
					lost := int32(uint32(b[5])<<16 | uint32(b[6])<<8 | uint32(b[7]))
					if lost&0x800000 != 0 {
						lost |= ^int32(0xffffff)
					}
					c.blocks = append(c.blocks, reportBlock{binary.BigEndian.Uint32(b), b[4], lost, binary.BigEndian.Uint32(b[8:]), binary.BigEndian.Uint32(b[12:]), binary.BigEndian.Uint32(b[16:]), binary.BigEndian.Uint32(b[20:])})
				}
			}
		case 202:
			offset := 4
			for i := 0; i < count; i++ {
				if offset+4 > len(p) {
					return c, false
				}
				id := binary.BigEndian.Uint32(p[offset:])
				offset += 4
				ended := false
				for offset < len(p) {
					kind := p[offset]
					offset++
					if kind == 0 {
						ended = true
						for offset%4 != 0 {
							if offset >= len(p) || p[offset] != 0 {
								return c, false
							}
							offset++
						}
						break
					}
					if offset >= len(p) {
						return c, false
					}
					n := int(p[offset])
					offset++
					if offset+n > len(p) {
						return c, false
					}
					if kind == 1 {
						c.cnames[id] = cleanName(string(p[offset : offset+n]))
					}
					if kind == 2 {
						c.names[id] = cleanName(string(p[offset : offset+n]))
					}
					offset += n
				}
				if !ended {
					return c, false
				}
			}
		case 204:
			if len(p) < 12 {
				return c, false
			}
			if string(p[8:12]) == "SLQ1" {
				if len(p) < 14 || c.topology != nil || binary.BigEndian.Uint32(p[4:8]) != c.reporter {
					return c, false
				}
				n := int(binary.BigEndian.Uint16(p[12:14]))
				if n == 0 || n > 2048 || 14+n > len(p) || len(p)-(14+n) > 3 {
					return c, false
				}
				for _, pad := range p[14+n:] {
					if pad != 0 {
						return c, false
					}
				}
				c.topology = p[14 : 14+n]
			}
		case 203:
			if len(p) < 4+count*4 {
				return c, false
			}
			for i := 0; i < count; i++ {
				c.bye = append(c.bye, binary.BigEndian.Uint32(p[4+i*4:]))
			}
		}
		first = false
	}
	return c, true
}
func put32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func sdes(id uint32, cname, name string) []byte {
	p := make([]byte, 8)
	p[0] = 0x81
	p[1] = 202
	put32(p[4:], id)
	for _, v := range []struct {
		kind byte
		text string
	}{{1, cname}, {2, name}} {
		text := cleanName(v.text)
		for len(text) > 255 {
			_, size := utf8.DecodeLastRuneInString(text)
			text = text[:len(text)-size]
		}
		if text != "" {
			p = append(p, v.kind, byte(len(text)))
			p = append(p, text...)
		}
	}
	p = append(p, 0)
	for len(p)%4 != 0 {
		p = append(p, 0)
	}
	binary.BigEndian.PutUint16(p[2:], uint16(len(p)/4-1))
	return p
}
func receiverPacket(id uint32, b reportBlock, cname, name string) []byte {
	p := make([]byte, 32)
	p[0] = 0x81
	p[1] = 201
	binary.BigEndian.PutUint16(p[2:], 7)
	put32(p[4:], id)
	put32(p[8:], b.source)
	put32(p[12:], uint32(b.lost)&0xffffff)
	p[12] = b.fraction
	put32(p[16:], b.highest)
	put32(p[20:], b.jitter)
	put32(p[24:], b.lsr)
	put32(p[28:], b.dlsr)
	return append(p, sdes(id, cname, name)...)
}

// packetReception follows RFC 3550 sequence wrap, restart confirmation, interval
// loss and integer interarrival jitter. Duplicates and late packets count as
// received, so cumulative loss may be negative, as specified by RTCP.
type packetReception struct {
	initialized                            bool
	source                                 uint32
	base, cycles, maxSeq, badSeq           uint32
	received, priorReceived, priorExpected uint32
	transit                                int32
	jitter                                 int64 // Sixteen times the timestamp-unit estimate.
	last                                   time.Time
	origin                                 time.Time
	lsr                                    uint32
	srAt                                   time.Time
}

func (r *packetReception) observe(source uint32, seq uint16, stamp uint32, rate int, now time.Time) {
	if !r.initialized || r.source != source {
		*r = packetReception{initialized: true, source: source, base: uint32(seq), maxSeq: uint32(seq), badSeq: 1<<16 + 1, origin: now}
	}
	delta := uint16(uint32(seq) - r.maxSeq)
	if delta < 3000 {
		if uint32(seq) < r.maxSeq {
			r.cycles += 1 << 16
		}
		r.maxSeq = uint32(seq)
	} else if delta <= 65536-100 {
		if uint32(seq) != r.badSeq {
			r.badSeq = (uint32(seq) + 1) & 0xffff
			return
		}
		*r = packetReception{initialized: true, source: source, base: uint32(seq), maxSeq: uint32(seq), badSeq: 1<<16 + 1, origin: now}
	}
	transit := int32(uint32((int64(now.Sub(r.origin)/time.Second)*int64(rate) + int64(now.Sub(r.origin)%time.Second)*int64(rate)/1_000_000_000)) - stamp)
	if r.received > 0 {
		d := int64(int32(transit - r.transit))
		if d < 0 {
			d = -d
		}
		r.jitter += d - ((r.jitter + 8) >> 4)
	}
	r.transit = transit
	r.received++
	r.last = now
}
func (r *packetReception) block(now time.Time) reportBlock {
	highest := r.cycles + r.maxSeq
	expected := highest - r.base + 1
	lost := int64(expected) - int64(r.received)
	lost = max(-8388608, min(8388607, lost))
	interval := expected - r.priorExpected
	received := r.received - r.priorReceived
	missing := int64(interval) - int64(received)
	fraction := byte(0)
	if interval > 0 && missing > 0 {
		fraction = byte(min(255, missing*256/int64(interval)))
	}
	r.priorExpected = expected
	r.priorReceived = r.received
	b := reportBlock{source: r.source, fraction: fraction, lost: int32(lost), highest: highest, jitter: uint32(max(0, r.jitter) >> 4), lsr: r.lsr}
	if !r.srAt.IsZero() {
		b.dlsr = uint32(min(int64(0xffffffff), min(now.Sub(r.srAt), 65535*time.Second).Nanoseconds()*65536/1_000_000_000))
	}
	return b
}

type receiverReporter struct {
	mu          sync.Mutex
	stats       packetReception
	id          uint32
	name, cname string
	source      netip.Addr
	cancel      context.CancelFunc
	// bye is told when the source says goodbye for a synchronization source.
	bye func(ssrc uint32)
}

func (r *receiverReporter) observe(source uint32, seq uint16, stamp uint32, rate int, now time.Time) {
	r.mu.Lock()
	r.stats.observe(source, seq, stamp, rate, now)
	r.mu.Unlock()
}
func (r *receiverReporter) packet(now time.Time) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stats.initialized || now.Sub(r.stats.last) > reportExpiry {
		return nil
	}
	return receiverPacket(r.id, r.stats.block(now), r.cname, r.name)
}
func (r *receiverReporter) receive(data []byte, ip netip.Addr, now time.Time) {
	if ip != r.source {
		return
	}
	c, ok := parseControl(data)
	if !ok {
		return
	}
	if r.bye != nil {
		for _, id := range c.bye {
			r.bye(id)
		}
	}
	if !c.sr {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats.initialized && c.reporter == r.stats.source {
		r.stats.lsr = c.stamp
		r.stats.srAt = now
	}
}

// ReceiverStatus represents a reporter, never an authenticated device or a claim
// that it is audible. Sources sharing a group are separated by report-block SSRC.
type ReceiverStatus struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IP          string    `json:"ip"`
	LastSeen    time.Time `json:"lastSeen"`
	Receiving   bool      `json:"receiving"`
	LossPercent float64   `json:"lossPercent"`
	Lost        int32     `json:"lost"`
	JitterMS    float64   `json:"jitterMs"`
	highest     uint32
	progress    time.Time
}
type senderReports struct {
	mu              sync.Mutex
	source          uint32
	rate            int
	stamp           uint32
	at              time.Time
	packets, octets uint32
	name, cname     string
	receivers       map[string]ReceiverStatus
	err             string
	bye             bool // the next packet says goodbye
}

// leave arranges for the next packet to carry a BYE, if this sender has been
// sending, so that receivers end the stream at once instead of counting the
// silence as an underrun. It reports whether a BYE is due.
func (r *senderReports) leave() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at.IsZero() {
		return false
	}
	r.bye = true
	return true
}

func (r *senderReports) leaving() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bye
}

// byePacket is an RTCP BYE for one source, closing a compound packet.
func byePacket(source uint32) []byte {
	p := make([]byte, 8)
	p[0] = 0x81
	p[1] = 203
	binary.BigEndian.PutUint16(p[2:], 1)
	put32(p[4:], source)
	return p
}

func (r *senderReports) sent(source, stamp uint32, rate, octets int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.source != source {
		r.source = source
		r.packets = 0
		r.octets = 0
		clear(r.receivers)
	}
	r.stamp = stamp
	r.rate = rate
	r.at = now
	r.packets++
	r.octets += uint32(octets)
}
func (r *senderReports) packet(now time.Time) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at.IsZero() {
		return nil
	}
	// A silent sender becomes a receiver after two report intervals.
	var p []byte
	if now.Sub(r.at) < 10*time.Second {
		p = make([]byte, 28)
		p[0] = 0x80
		p[1] = 200
		binary.BigEndian.PutUint16(p[2:], 6)
		binary.BigEndian.PutUint64(p[8:], ntp(now))
		put32(p[16:], r.stamp+uint32(now.Sub(r.at).Nanoseconds()*int64(r.rate)/1_000_000_000))
		put32(p[20:], r.packets)
		put32(p[24:], r.octets)
	} else {
		p = make([]byte, 8)
		p[0] = 0x80
		p[1] = 201
		binary.BigEndian.PutUint16(p[2:], 1)
	}
	put32(p[4:], r.source)
	p = append(p, sdes(r.source, r.cname, r.name)...)
	if r.bye {
		// Until it sends again, this sender has nothing to report.
		p = append(p, byePacket(r.source)...)
		r.bye, r.at = false, time.Time{}
	}
	return p
}
func (r *senderReports) receive(data []byte, ip netip.Addr, now time.Time) {
	c, ok := parseControl(data)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range c.bye {
		delete(r.receivers, fmt.Sprintf("%s/%08x", ip, id))
	}
	for _, id := range c.bye {
		if id == c.reporter {
			return
		}
	}
	if r.at.IsZero() || c.reporter == r.source {
		return
	}
	for _, b := range c.blocks {
		if b.source != r.source {
			continue
		}
		id := fmt.Sprintf("%s/%08x", ip, c.reporter)
		old, exists := r.receivers[id]
		for key, v := range r.receivers {
			if now.Sub(v.LastSeen) > reportExpiry {
				delete(r.receivers, key)
			}
		}
		if !exists && len(r.receivers) >= reportLimit {
			return
		}
		progress := old.progress
		if !exists || b.highest != old.highest {
			progress = now
		}
		name := c.names[c.reporter]
		if name == "" {
			name = old.Name
		}
		if name == "" {
			name = "RTP receiver"
		}
		r.receivers[id] = ReceiverStatus{ID: id, Name: name, IP: ip.String(), LastSeen: now, LossPercent: float64(b.fraction) * 100 / 256, Lost: b.lost, JitterMS: float64(b.jitter) * 1000 / float64(max(1, r.rate)), highest: b.highest, progress: progress}
		return
	}
}
func (r *senderReports) status(now time.Time) ([]ReceiverStatus, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReceiverStatus, 0, len(r.receivers))
	for id, v := range r.receivers {
		if now.Sub(v.LastSeen) > reportExpiry {
			delete(r.receivers, id)
			continue
		}
		v.Receiving = now.Sub(v.LastSeen) < reportFresh && now.Sub(v.progress) < reportFresh && now.Sub(r.at) < time.Second
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, r.err
}
func (r *senderReports) error(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = ""
	if err != nil {
		r.err = "Receiver reporting unavailable: " + err.Error()
	}
}

// appendTopology adds a private RTCP APP packet. Other receivers ignore it.
// Keep topology alive even while audio is idle, using an empty receiver report.
func appendTopology(packet []byte, id uint32, payload []byte) []byte {
	if len(payload) == 0 || len(payload) > 2048 {
		return packet
	}
	if len(packet) == 0 {
		packet = make([]byte, 8)
		packet[0], packet[1] = 0x80, 201
		binary.BigEndian.PutUint16(packet[2:], 1)
		put32(packet[4:], id)
		packet = append(packet, sdes(id, "slopify-control", "")...)
	} else {
		id = binary.BigEndian.Uint32(packet[4:8])
	}
	app := make([]byte, (14+len(payload)+3)&^3)
	app[0], app[1] = 0x80, 204
	binary.BigEndian.PutUint16(app[2:], uint16(len(app)/4-1))
	put32(app[4:], id)
	copy(app[8:], "SLQ1")
	binary.BigEndian.PutUint16(app[12:], uint16(len(payload)))
	copy(app[14:], payload)
	return append(packet, app...)
}
