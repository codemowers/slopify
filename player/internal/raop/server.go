package raop

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/hex"
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
	"time"

	"github.com/codemowers/slopify/player/internal/audiodec"
)

const (
	maxConnections  = 8
	maxHeaderLine   = 8 << 10
	maxHeaders      = 64
	maxBody         = 64 << 10
	setupTimeout    = time.Minute
	writeTimeout    = 5 * time.Second
	timingInterval  = 3 * time.Second
	tickInterval    = 20 * time.Millisecond
	nameMax         = 128
	ntpEpochOffset  = 2208988800
	udpPacketBytes  = 16 << 10
	maxPacketFrames = 4096 // Far beyond any MTU-sized packet.
)

// Config describes the server.
type Config struct {
	// HardwareID must match the advertised instance name's prefix.
	HardwareID [6]byte
	// Key enables RSA sessions; nil offers plain RAOP only.
	Key *rsa.PrivateKey
	// BufferMillis is the playout delay; zero means DefaultBufferMillis.
	BufferMillis int
	// OnStream receives each new stream and reports whether it was admitted.
	OnStream func(*Stream) bool
	// OnVolume receives the sender's level in dB: -30 to 0, or -144 for mute.
	OnVolume func(stream *Stream, db float64)
	// Resolve finds a sender's DACP remote control; nil disables remote control.
	Resolve Resolver
}

// Server accepts RAOP senders.
type Server struct {
	config Config
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

// NewServer returns a server.
func NewServer(config Config) *Server {
	if config.BufferMillis == 0 {
		config.BufferMillis = DefaultBufferMillis
	}
	return &Server{config: config, conns: map[net.Conn]struct{}{}}
}

// Serve runs until ctx ends.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		listener.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for conn := range s.conns {
			conn.Close()
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			slog.Warn("raop accept", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		s.mu.Lock()
		if len(s.conns) >= maxConnections || ctx.Err() != nil {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				conn.Close()
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			c := &session{server: s, conn: conn, br: bufio.NewReaderSize(conn, maxHeaderLine)}
			defer c.close()
			if err := c.run(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Debug("raop session ended", "err", err)
			}
		}()
	}
}

type request struct {
	method, uri string
	headers     map[string]string
	body        []byte
}

func (r *request) header(name string) string { return r.headers[strings.ToLower(name)] }

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("oversized RTSP line")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func readRequest(br *bufio.Reader) (*request, error) {
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "RTSP/") {
		return nil, errors.New("malformed RTSP request")
	}
	r := &request{method: parts[0], uri: parts[1], headers: map[string]string{}}
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		if len(r.headers) >= maxHeaders {
			return nil, errors.New("too many RTSP headers")
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("malformed RTSP header")
		}
		r.headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if text := r.header("Content-Length"); text != "" {
		size, err := strconv.Atoi(text)
		if err != nil || size < 0 || size > maxBody {
			return nil, errors.New("unacceptable RTSP body")
		}
		r.body = make([]byte, size)
		if _, err := io.ReadFull(br, r.body); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// session is one sender's RTSP connection and the UDP sockets it negotiates.
type session struct {
	server *Server
	conn   net.Conn
	br     *bufio.Reader

	name    string
	config  audiodec.ALACConfig
	decoder *audiodec.ALAC
	cipher  packetCipher

	peer                    netip.Addr
	remote                  remote
	audio, control, timing  *net.UDPConn
	controlPort, timingPort int
	id                      string
	stream                  *Stream
	streamMu                sync.Mutex
	done                    chan struct{}
	closeOnce               sync.Once
	wg                      sync.WaitGroup
}

func (c *session) close() {
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
		for _, socket := range []*net.UDPConn{c.audio, c.control, c.timing} {
			if socket != nil {
				socket.Close()
			}
		}
		c.wg.Wait()
		if stream := c.current(); stream != nil {
			stream.ring.Finish()
		}
	})
}

func (c *session) current() *Stream {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.stream
}

func (c *session) run() error {
	c.conn.SetDeadline(time.Now().Add(setupTimeout))
	for {
		r, err := readRequest(c.br)
		if err != nil {
			return err
		}
		headers, body, status := c.handle(r)
		if err := c.respond(r, status, headers, body); err != nil {
			return err
		}
		if r.method == "TEARDOWN" {
			return nil
		}
		if c.current() != nil {
			c.conn.SetReadDeadline(time.Time{}) // TCP keepalive watches an idle session.
		} else {
			c.conn.SetDeadline(time.Now().Add(setupTimeout))
		}
	}
}

func (c *session) respond(r *request, status int, headers map[string]string, body string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "RTSP/1.0 %d %s\r\nCSeq: %s\r\nServer: AirTunes/105.1\r\n", status, statusText(status), r.header("CSeq"))
	if challenge := r.header("Apple-Challenge"); challenge != "" && c.server.config.Key != nil {
		if response, err := c.appleResponse(challenge); err == nil {
			fmt.Fprintf(&b, "Apple-Response: %s\r\n", response)
		} else {
			slog.Warn("raop challenge", "err", err)
		}
	}
	for key, value := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", key, value)
	}
	if body != "" {
		fmt.Fprintf(&b, "Content-Type: text/parameters\r\nContent-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n" + body)
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := io.WriteString(c.conn, b.String())
	c.conn.SetWriteDeadline(time.Time{})
	return err
}

func (c *session) appleResponse(challenge string) (string, error) {
	local, ok := c.conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return "", errors.New("not a TCP connection")
	}
	ip := local.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return appleResponse(c.server.config.Key, challenge, ip, c.server.config.HardwareID)
}

func statusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 415:
		return "Unsupported Media Type"
	case 453:
		return "Not Enough Bandwidth"
	case 455:
		return "Method Not Valid In This State"
	case 461:
		return "Unsupported Transport"
	case 500:
		return "Internal Server Error"
	}
	return "Not Implemented"
}

func (c *session) handle(r *request) (map[string]string, string, int) {
	if id := r.header("DACP-ID"); id != "" {
		if rem, ok := parseRemote(id, r.header("Active-Remote")); ok {
			c.remote = rem
		}
	}
	switch r.method {
	case "OPTIONS":
		return map[string]string{"Public": "ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER"}, "", 200
	case "ANNOUNCE":
		if err := c.announce(r); err != nil {
			slog.Warn("raop announce refused", "peer", peerHost(c.conn), "err", err)
			return nil, "", 415
		}
		return nil, "", 200
	case "SETUP":
		return c.setup(r)
	case "RECORD":
		return c.record(r)
	case "FLUSH":
		if stream := c.current(); stream != nil {
			seq, known := rtpInfoSeq(r.header("RTP-Info"))
			stream.flush(seq, known)
		}
		return nil, "", 200
	case "SET_PARAMETER":
		c.setParameter(r)
		return nil, "", 200
	case "GET_PARAMETER":
		return nil, "volume: 0.000000\r\n", 200
	case "TEARDOWN", "PAUSE":
		return nil, "", 200
	}
	return nil, "", 501
}

// announce reads the session description. Only ALAC is accepted, in the clear or
// under RSA-wrapped AES: FairPlay keys and other codecs are refused here, so that
// nothing downstream has to consider them.
func (c *session) announce(r *request) error {
	var rtpmap, fmtp, rsaKey, iv string
	for _, line := range strings.Split(string(r.body), "\n") {
		line = strings.TrimSpace(line)
		key, value, _ := strings.Cut(line, ":")
		switch key {
		case "a=rtpmap":
			rtpmap = value
		case "a=fmtp":
			fmtp = value
		case "a=rsaaeskey":
			rsaKey = value
		case "a=aesiv":
			iv = value
		case "a=fpaeskey":
			return errors.New("FairPlay is not supported")
		}
	}
	if !strings.Contains(rtpmap, "AppleLossless") {
		return fmt.Errorf("unsupported codec %q", rtpmap)
	}
	config, err := parseFmtp(fmtp)
	if err != nil {
		return err
	}
	decoder, err := audiodec.NewALAC(config)
	if err != nil {
		return err
	}
	c.cipher = nil
	if rsaKey != "" {
		if c.cipher, err = newAESCBC(c.server.config.Key, rsaKey, iv); err != nil {
			return err
		}
	}
	c.config, c.decoder = config, decoder
	c.name = senderName(r)
	return nil
}

// parseFmtp reads "96 frames version depth pb mb kb channels maxrun maxbytes bitrate rate".
func parseFmtp(fmtp string) (audiodec.ALACConfig, error) {
	fields := strings.Fields(fmtp)
	if len(fields) != 12 {
		return audiodec.ALACConfig{}, fmt.Errorf("malformed fmtp %q", fmtp)
	}
	values := make([]int, 12)
	for i, field := range fields {
		v, err := strconv.Atoi(field)
		if err != nil || v < 0 {
			return audiodec.ALACConfig{}, fmt.Errorf("malformed fmtp %q", fmtp)
		}
		values[i] = v
	}
	if values[11] < 8000 || values[11] > 192000 || values[4] > 255 || values[5] > 255 || values[6] > 255 {
		return audiodec.ALACConfig{}, fmt.Errorf("unsupported fmtp %q", fmtp)
	}
	// A sender's packets fill a socket buffer, so they cannot be longer than this.
	if values[1] > 16384 {
		return audiodec.ALACConfig{}, fmt.Errorf("unsupported frame length %d", values[1])
	}
	// PipeWire announces 352 frames but fills each packet to the MTU.
	return audiodec.ALACConfig{FrameLength: uint32(values[1]), BitDepth: values[3], HistoryMult: uint8(values[4]),
		InitialHistory: uint8(values[5]), Limit: uint8(values[6]), Channels: values[7], SampleRate: values[11],
		MaxFrames: maxPacketFrames}, nil
}

// senderName keeps display text only: it is untrusted and reaches the local page.
func senderName(r *request) string {
	name := r.header("X-Apple-Client-Name")
	if name == "" {
		name, _, _ = strings.Cut(r.header("User-Agent"), "/")
	}
	if name = cleantext.Line(name, nameMax); name == "" {
		name = "AirPlay sender"
	}
	return name
}

func peerHost(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}

func (c *session) setup(r *request) (map[string]string, string, int) {
	if c.decoder == nil || c.audio != nil {
		return nil, "", 455
	}
	transport := r.header("Transport")
	if !strings.HasPrefix(transport, "RTP/AVP/UDP") {
		return nil, "", 461
	}
	for _, field := range strings.Split(transport, ";") {
		key, value, _ := strings.Cut(field, "=")
		port, _ := strconv.Atoi(value)
		if port < 0 || port > 65535 {
			return nil, "", 400
		}
		switch key {
		case "control_port":
			c.controlPort = port
		case "timing_port":
			c.timingPort = port
		}
	}
	remote, ok := c.conn.RemoteAddr().(*net.TCPAddr)
	local, okLocal := c.conn.LocalAddr().(*net.TCPAddr)
	if !ok || !okLocal {
		return nil, "", 500
	}
	addr, _ := netip.AddrFromSlice(remote.IP)
	c.peer = addr.Unmap()
	var err error
	if c.audio, err = net.ListenUDP("udp", &net.UDPAddr{IP: local.IP, Zone: local.Zone}); err == nil {
		if c.control, err = net.ListenUDP("udp", &net.UDPAddr{IP: local.IP, Zone: local.Zone}); err == nil {
			c.timing, err = net.ListenUDP("udp", &net.UDPAddr{IP: local.IP, Zone: local.Zone})
		}
	}
	if err != nil {
		slog.Warn("raop sockets", "err", err)
		return nil, "", 500
	}
	var id [8]byte
	rand.Read(id[:])
	c.id = strings.ToUpper(hex.EncodeToString(id[:]))
	c.done = make(chan struct{})
	c.wg.Add(4)
	go c.readAudio()
	go c.readControl()
	go c.readTiming()
	go c.keepTime()
	return map[string]string{
		"Session":           c.id,
		"Audio-Jack-Status": "connected; type=analog",
		"Transport": fmt.Sprintf("RTP/AVP/UDP;unicast;mode=record;server_port=%d;control_port=%d;timing_port=%d",
			c.audio.LocalAddr().(*net.UDPAddr).Port, c.control.LocalAddr().(*net.UDPAddr).Port, c.timing.LocalAddr().(*net.UDPAddr).Port),
	}, "", 200
}

func rtpInfoSeq(info string) (uint16, bool) {
	for _, field := range strings.Split(info, ";") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(field), "seq="); ok {
			if seq, err := strconv.ParseUint(value, 10, 16); err == nil {
				return uint16(seq), true
			}
		}
	}
	return 0, false
}

func (c *session) record(r *request) (map[string]string, string, int) {
	if c.audio == nil {
		return nil, "", 455
	}
	if c.current() != nil {
		return nil, "", 200
	}
	stream := newStream(c.decoder, c.config, c.cipher, c.server.config.BufferMillis, func() { c.conn.Close() })
	stream.Name, stream.Peer = c.name, peerHost(c.conn)
	stream.control = &remoteControl{remote: c.remote, peer: c.peer, resolve: c.server.config.Resolve}
	seq, known := rtpInfoSeq(r.header("RTP-Info"))
	stream.start(seq, known, c.requestResend)
	if c.server.config.OnStream != nil && !c.server.config.OnStream(stream) {
		return nil, "", 453
	}
	c.streamMu.Lock()
	c.stream = stream
	c.streamMu.Unlock()
	go c.runTicks(stream)
	return map[string]string{
		"Audio-Latency": strconv.Itoa(c.config.SampleRate * c.server.config.BufferMillis / 1000),
		"Session":       c.id,
	}, "", 200
}

func (c *session) runTicks(stream *Stream) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-ticker.C:
			stream.tick(now)
		}
	}
}

func (c *session) setParameter(r *request) {
	stream := c.current()
	if stream != nil && strings.HasPrefix(r.header("Content-Type"), "application/x-dmap-tagged") {
		m := parseDMAP(r.body)
		stream.metadata.Store(&m)
		return
	}
	if !strings.HasPrefix(r.header("Content-Type"), "text/parameters") {
		return // Artwork and progress are not used.
	}
	if stream == nil || c.server.config.OnVolume == nil {
		return
	}
	for _, line := range strings.Split(string(r.body), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "volume" {
			continue
		}
		db, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && (db == -144 || db >= -30 && db <= 0) {
			c.server.config.OnVolume(stream, db)
		}
	}
}

// fromPeer keeps a stranger on the LAN from injecting audio into the session.
func (c *session) fromPeer(addr netip.AddrPort) bool { return addr.Addr().Unmap() == c.peer }

func (c *session) readAudio() {
	defer c.wg.Done()
	buf := make([]byte, udpPacketBytes)
	for {
		n, addr, err := c.audio.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if c.fromPeer(addr) {
			c.deliver(buf[:n])
		}
	}
}

// deliver accepts an RTP audio packet; retransmissions carry the same format.
func (c *session) deliver(packet []byte) {
	stream := c.current()
	if stream == nil || len(packet) < 12 || packet[0]>>6 != 2 {
		return
	}
	stream.receive(binary.BigEndian.Uint16(packet[2:]), packet[12:])
}

func (c *session) readControl() {
	defer c.wg.Done()
	buf := make([]byte, udpPacketBytes)
	for {
		n, addr, err := c.control.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if !c.fromPeer(addr) || n < 4 {
			continue
		}
		switch buf[1] & 0x7f {
		case 0x54: // Sync. The extension bit marks the first since playback started.
			if stream := c.current(); stream != nil && buf[0]&0x10 != 0 {
				stream.realign()
			}
		case 0x56: // A retransmission wraps the original packet.
			c.deliver(buf[4:n])
		}
	}
}

func (c *session) requestResend(first, count uint16) {
	if c.controlPort == 0 {
		return
	}
	var packet [8]byte
	packet[0], packet[1] = 0x80, 0xd5
	binary.BigEndian.PutUint16(packet[2:], 1)
	binary.BigEndian.PutUint16(packet[4:], first)
	binary.BigEndian.PutUint16(packet[6:], count)
	c.control.WriteToUDPAddrPort(packet[:], netip.AddrPortFrom(c.peer, uint16(c.controlPort)))
}

func ntpNow(now time.Time) uint64 {
	seconds := uint64(now.Unix() + ntpEpochOffset)
	return seconds<<32 | uint64(now.Nanosecond())<<32/1_000_000_000
}

func (c *session) readTiming() {
	defer c.wg.Done()
	buf := make([]byte, 128)
	for {
		n, addr, err := c.timing.ReadFromUDPAddrPort(buf)
		received := ntpNow(time.Now())
		if err != nil {
			return
		}
		if !c.fromPeer(addr) || n != 32 || buf[1]&0x7f != 0x52 {
			continue // Replies to our own requests are not used: playout is paced by the output.
		}
		var reply [32]byte
		reply[0], reply[1], reply[2], reply[3] = 0x80, 0xd3, 0, 7
		copy(reply[8:], buf[24:32])
		binary.BigEndian.PutUint64(reply[16:], received)
		binary.BigEndian.PutUint64(reply[24:], ntpNow(time.Now()))
		c.timing.WriteToUDPAddrPort(reply[:], addr)
	}
}

// keepTime sends the timing requests Apple senders expect from a receiver.
func (c *session) keepTime() {
	defer c.wg.Done()
	if c.timingPort == 0 {
		return
	}
	ticker := time.NewTicker(timingInterval)
	defer ticker.Stop()
	for {
		var packet [32]byte
		packet[0], packet[1], packet[2], packet[3] = 0x80, 0xd2, 0, 7
		binary.BigEndian.PutUint64(packet[24:], ntpNow(time.Now()))
		c.timing.WriteToUDPAddrPort(packet[:], netip.AddrPortFrom(c.peer, uint16(c.timingPort)))
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
	}
}
