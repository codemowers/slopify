// Package pulse is a playback-only server for the PulseAudio native protocol, so
// that laptops can send audio to the speaker with an ordinary tunnel sink. It has
// no authentication: like the listener it replaces, it is for a trusted LAN only.
// Sender names are descriptive; they must never be treated as identity.
package pulse

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	protocolVersion = 35
	versionMask     = 0xffff
	minVersion      = 13

	controlChannel = 0xffffffff
	descriptorSize = 20
	maxFrame       = 4 << 20
	maxClients     = 8
	maxStreams     = 8
	writeTimeout   = 5 * time.Second

	// A sink input changed, and the subscription mask that asks to hear about it.
	eventSinkInputChange = 0x0002 | 0x0010
	maskSinkInput        = 0x0004

	encodingPCM  = 1
	volumeNorm   = 0x10000
	invalidIndex = 0xffffffff
	sinkIndex    = 0

	maxBufferBytes = 8 << 20
)

// Protocol commands.
const (
	cmdError                  = 0
	cmdReply                  = 2
	cmdCreatePlaybackStream   = 3
	cmdDeletePlaybackStream   = 4
	cmdExit                   = 7
	cmdAuth                   = 8
	cmdSetClientName          = 9
	cmdLookupSink             = 10
	cmdDrainPlaybackStream    = 12
	cmdStat                   = 13
	cmdGetPlaybackLatency     = 14
	cmdGetServerInfo          = 20
	cmdGetSinkInfo            = 21
	cmdGetSinkInfoList        = 22
	cmdGetSourceInfoList      = 24
	cmdGetModuleInfoList      = 26
	cmdGetClientInfoList      = 28
	cmdGetSinkInputInfo       = 29
	cmdGetSinkInputInfoList   = 30
	cmdGetSourceOutputList    = 32
	cmdGetSampleInfoList      = 34
	cmdSubscribe              = 35
	cmdSetSinkVolume          = 36
	cmdSetSinkInputVolume     = 37
	cmdSetSinkMute            = 39
	cmdCorkPlaybackStream     = 41
	cmdFlushPlaybackStream    = 42
	cmdTriggerPlaybackStream  = 43
	cmdSetPlaybackStreamName  = 46
	cmdPrebufPlaybackStream   = 60
	cmdRequest                = 61
	cmdSubscribeEvent         = 66
	cmdOverflow               = 62
	cmdUnderflow              = 63
	cmdSetSinkInputMute       = 69
	cmdSetPlaybackBufferAttr  = 72
	cmdUpdatePlaybackRate     = 74
	cmdUpdatePlaybackProplist = 81
	cmdUpdateClientProplist   = 82
	cmdRemovePlaybackProplist = 84
	cmdRemoveClientProplist   = 85
	cmdStarted                = 86
	cmdGetCardInfoList        = 89
)

// Protocol errors.
const (
	errInvalid      = 3
	errNoEntity     = 5
	errProtocol     = 7
	errVersion      = 17
	errNotSupported = 19
)

// Sample formats, as numbered by the protocol.
const (
	FormatFloat32LE = 5
	FormatS16LE     = 3
	FormatS32LE     = 7
	FormatS24LE     = 9
	FormatS2432LE   = 11
	channelMono     = 0
	channelFrontL   = 1
	channelFrontR   = 2
)

// BytesPerSample returns a supported format's sample size, or zero.
func BytesPerSample(format int) int {
	switch format {
	case FormatS16LE:
		return 2
	case FormatS24LE:
		return 3
	case FormatS32LE, FormatS2432LE, FormatFloat32LE:
		return 4
	}
	return 0
}

// Config describes the one sink that senders see.
type Config struct {
	Name, Description string
	Rate              int
	// MinBufferMillis raises what senders ask for; 0 honours them. A PipeWire tunnel
	// aims for 200 ms in total, and starves itself into ticks if this exceeds that.
	MinBufferMillis int
	// OnStream receives each new playback stream. Its owner calls Stream.Read at
	// the output's pace; that pace is what the sender's flow control follows.
	OnStream func(*Stream)
	// OnVolume receives a sender's requested volume, where 0x10000 is 100%.
	OnVolume func(*Stream, uint32)
}

// Server accepts connections until its context ends.
type Server struct {
	config Config

	mu      sync.Mutex
	clients map[*client]struct{}
	nextID  uint32
	inputs  uint32 // Sink-input indexes are unique across the whole server.
}

func NewServer(config Config) *Server {
	if config.Rate == 0 {
		config.Rate = 48000
	}
	return &Server{config: config, clients: map[*client]struct{}{}}
}

// Serve runs until ctx ends or the listener fails.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if len(s.clients) >= maxClients {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		c := &client{server: s, conn: conn, streams: map[uint32]*Stream{}, props: map[string]string{}}
		if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			c.peer = host
		}
		s.nextID++
		c.index = s.nextID
		s.clients[c] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer conn.Close()
			if err := c.run(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Info("pulse client ended", "peer", c.peer, "err", err)
			}
			c.closeStreams()
			s.mu.Lock()
			delete(s.clients, c)
			s.mu.Unlock()
		}()
	}
}

type client struct {
	server     *Server
	conn       net.Conn
	peer       string
	index      uint32
	version    uint32
	authed     bool
	subscribed atomic.Uint32

	writeMu sync.Mutex
	mu      sync.Mutex
	props   map[string]string
	streams map[uint32]*Stream
	next    uint32
}

func (c *client) run() error {
	var head [descriptorSize]byte
	for {
		if _, err := io.ReadFull(c.conn, head[:]); err != nil {
			return err
		}
		length := binary.BigEndian.Uint32(head[0:])
		channel := binary.BigEndian.Uint32(head[4:])
		offset := int64(binary.BigEndian.Uint64(head[8:]))
		flags := binary.BigEndian.Uint32(head[16:])
		if length > maxFrame {
			return fmt.Errorf("frame of %d bytes", length)
		}
		if channel != controlChannel {
			if err := c.data(channel, length, offset, flags); err != nil {
				return err
			}
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(c.conn, body); err != nil {
			return err
		}
		if err := c.control(body); err != nil {
			return err
		}
	}
}

func (c *client) send(channel uint32, payload []byte) error {
	var head [descriptorSize]byte
	binary.BigEndian.PutUint32(head[0:], uint32(len(payload)))
	binary.BigEndian.PutUint32(head[4:], channel)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.conn.Write(head[:]); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

func (c *client) message(command, tag uint32) *writer {
	w := &writer{}
	return w.u32(command).u32(tag)
}

func (c *client) reply(tag uint32) *writer { return c.message(cmdReply, tag) }

func (c *client) finish(w *writer) error { return c.send(controlChannel, w.b) }

func (c *client) fail(tag, code uint32) error {
	return c.finish(c.message(cmdError, tag).u32(code))
}

func (c *client) control(body []byte) error {
	r := &reader{b: body}
	command, tag := r.u32(), r.u32()
	if r.err != nil {
		return r.err
	}
	if command != cmdAuth && !c.authed {
		return c.fail(tag, errProtocol)
	}
	switch command {
	case cmdAuth:
		version := r.u32() & versionMask
		r.arbitrary() // The cookie: this listener is for a trusted LAN only.
		if r.err != nil {
			return r.err
		}
		if version < minVersion {
			return c.fail(tag, errVersion)
		}
		c.version, c.authed = min(version, protocolVersion), true
		return c.finish(c.reply(tag).u32(protocolVersion))
	case cmdSetClientName:
		props := r.proplist()
		if r.err != nil {
			return r.err
		}
		c.mu.Lock()
		c.props = props
		c.mu.Unlock()
		return c.finish(c.reply(tag).u32(c.index))
	case cmdCreatePlaybackStream:
		return c.createStream(tag, r)
	case cmdDeletePlaybackStream:
		id := r.u32()
		if r.err != nil {
			return r.err
		}
		c.mu.Lock()
		stream := c.streams[id]
		delete(c.streams, id)
		c.mu.Unlock()
		if stream == nil {
			return c.fail(tag, errNoEntity)
		}
		stream.close()
		return c.finish(c.reply(tag))
	case cmdDrainPlaybackStream:
		return c.withStream(tag, r, func(s *Stream) error {
			s.drain(tag)
			return nil
		})
	case cmdFlushPlaybackStream:
		return c.withStream(tag, r, func(s *Stream) error {
			s.flush()
			return c.finish(c.reply(tag))
		})
	case cmdTriggerPlaybackStream:
		return c.withStream(tag, r, func(s *Stream) error {
			s.setPrebuf(false)
			return c.finish(c.reply(tag))
		})
	case cmdPrebufPlaybackStream:
		return c.withStream(tag, r, func(s *Stream) error {
			s.setPrebuf(true)
			return c.finish(c.reply(tag))
		})
	case cmdCorkPlaybackStream:
		id := r.u32()
		corked := r.boolean()
		if r.err != nil {
			return r.err
		}
		s := c.stream(id)
		if s == nil {
			return c.fail(tag, errNoEntity)
		}
		s.setCorked(corked)
		return c.finish(c.reply(tag))
	case cmdGetPlaybackLatency:
		id := r.u32()
		sec, usec := r.timeval()
		if r.err != nil {
			return r.err
		}
		s := c.stream(id)
		if s == nil {
			return c.fail(tag, errNoEntity)
		}
		return c.finish(s.latencyReply(c.reply(tag), sec, usec))
	case cmdSetPlaybackStreamName:
		id := r.u32()
		name := r.str()
		if r.err != nil {
			return r.err
		}
		if s := c.stream(id); s != nil {
			s.setProps(map[string]string{"media.name": name})
		}
		return c.finish(c.reply(tag))
	case cmdUpdatePlaybackProplist:
		id := r.u32()
		r.u32() // Update mode: anything but replacement behaves the same here.
		props := r.proplist()
		if r.err != nil {
			return r.err
		}
		if s := c.stream(id); s != nil {
			s.setProps(props)
		}
		return c.finish(c.reply(tag))
	case cmdUpdateClientProplist:
		r.u32()
		props := r.proplist()
		if r.err != nil {
			return r.err
		}
		c.mu.Lock()
		for k, v := range props {
			c.props[k] = v
		}
		c.mu.Unlock()
		return c.finish(c.reply(tag))
	case cmdSetPlaybackBufferAttr:
		id := r.u32()
		maxLength, tLength, prebuf, minReq := r.u32(), r.u32(), r.u32(), r.u32()
		if r.err != nil {
			return r.err
		}
		s := c.stream(id)
		if s == nil {
			return c.fail(tag, errNoEntity)
		}
		s.setAttr(maxLength, tLength, prebuf, minReq)
		w := c.reply(tag)
		attr := s.attrs()
		w.u32(attr.maxLength).u32(attr.tLength).u32(attr.prebuf).u32(attr.minReq).usec(s.latencyUsec())
		return c.finish(w)
	case cmdUpdatePlaybackRate:
		// Rate adjustment would mean resampling, which this speaker refuses.
		return c.fail(tag, errNotSupported)
	case cmdSetSinkInputVolume:
		id := r.u32()
		volumes := r.cvolume()
		if r.err != nil {
			return r.err
		}
		s := c.streamByInput(id)
		if s != nil && len(volumes) > 0 {
			s.storeVolume(volumes[0])
			if c.server.config.OnVolume != nil {
				c.server.config.OnVolume(s, volumes[0])
			}
		}
		return c.finish(c.reply(tag))
	case cmdSubscribe:
		mask := r.u32()
		if r.err != nil {
			return r.err
		}
		c.subscribed.Store(mask)
		return c.finish(c.reply(tag))
	case cmdGetSinkInputInfo:
		input := r.u32()
		if r.err != nil {
			return r.err
		}
		s := c.server.streamByInput(input)
		if s == nil {
			return c.fail(tag, errNoEntity)
		}
		return c.finish(s.inputInfo(c.reply(tag), c.version))
	case cmdGetSinkInputInfoList:
		w := c.reply(tag)
		for _, s := range c.server.allStreams() {
			s.inputInfo(w, c.version)
		}
		return c.finish(w)
	case cmdSetSinkVolume, cmdSetSinkMute, cmdSetSinkInputMute, cmdRemovePlaybackProplist, cmdRemoveClientProplist:
		return c.finish(c.reply(tag))
	case cmdGetServerInfo:
		return c.finish(c.serverInfo(tag))
	case cmdLookupSink:
		name := r.str()
		if r.err != nil {
			return r.err
		}
		if !c.sinkMatches(sinkIndex, name) {
			return c.fail(tag, errNoEntity)
		}
		return c.finish(c.reply(tag).u32(sinkIndex))
	case cmdGetSinkInfo:
		index := r.u32()
		name := r.str()
		if r.err != nil {
			return r.err
		}
		if !c.sinkMatches(index, name) {
			return c.fail(tag, errNoEntity)
		}
		return c.finish(c.sinkInfo(c.reply(tag)))
	case cmdGetSinkInfoList:
		return c.finish(c.sinkInfo(c.reply(tag)))
	case cmdStat:
		return c.finish(c.reply(tag).u32(0).u32(0).u32(0).u32(0).u32(0))
	case cmdGetSourceInfoList, cmdGetModuleInfoList, cmdGetClientInfoList,
		cmdGetSourceOutputList, cmdGetSampleInfoList, cmdGetCardInfoList:
		return c.finish(c.reply(tag))
	case cmdExit:
		c.finish(c.reply(tag))
		return io.EOF
	}
	return c.fail(tag, errNotSupported)
}

func (s *Server) allStreams() []*Stream {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	var streams []*Stream
	for _, c := range clients {
		c.mu.Lock()
		for _, stream := range c.streams {
			streams = append(streams, stream)
		}
		c.mu.Unlock()
	}
	return streams
}

func (s *Server) streamByInput(input uint32) *Stream {
	for _, stream := range s.allStreams() {
		if stream.Input == input {
			return stream
		}
	}
	return nil
}

// notifyInputChange tells subscribed clients to re-read a sink input.
func (s *Server) notifyInputChange(input uint32) {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		if c.subscribed.Load()&maskSinkInput != 0 {
			c.finish(c.message(cmdSubscribeEvent, unset).u32(eventSinkInputChange).u32(input))
		}
	}
}

func (c *client) sinkMatches(index uint32, name string) bool {
	if name != "" {
		return name == c.server.config.Name || name == "@DEFAULT_SINK@"
	}
	return index == sinkIndex || index == invalidIndex
}

func (c *client) stream(id uint32) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *client) streamByInput(input uint32) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.streams {
		if s.Input == input {
			return s
		}
	}
	return nil
}

func (c *client) withStream(tag uint32, r *reader, f func(*Stream) error) error {
	id := r.u32()
	if r.err != nil {
		return r.err
	}
	s := c.stream(id)
	if s == nil {
		return c.fail(tag, errNoEntity)
	}
	return f(s)
}

func (c *client) serverInfo(tag uint32) *writer {
	config := c.server.config
	w := c.reply(tag)
	w.str("Slopify").str("16.0").str("slopify").str("slopify")
	w.sampleSpec(FormatS32LE, 2, uint32(config.Rate))
	w.str(config.Name).null().u32(uint32(c.index))
	return w.channelMap([]byte{channelFrontL, channelFrontR})
}

func (c *client) sinkInfo(w *writer) *writer {
	config := c.server.config
	w.u32(sinkIndex).str(config.Name).str(config.Description)
	w.sampleSpec(FormatS32LE, 2, uint32(config.Rate))
	w.channelMap([]byte{channelFrontL, channelFrontR})
	w.u32(invalidIndex).cvolume(2, volumeNorm).boolean(false)
	w.u32(invalidIndex).null() // No monitor source: this server cannot record.
	w.usec(0).str("slopify").u32(0)
	w.proplist(map[string]string{"device.description": config.Description})
	w.usec(0)
	w.volume(volumeNorm).u32(0).u32(0x10001).u32(invalidIndex)
	w.u32(0).null() // No ports.
	w.u8(1).pcmFormatInfo()
	return w
}

func (c *client) createStream(tag uint32, r *reader) error {
	format, channels, rate := r.sampleSpec()
	channelMap := r.channelMap()
	r.u32() // Sink index.
	sinkName := r.str()
	maxLength := r.u32()
	corked := r.boolean()
	tLength, prebuf, minReq := r.u32(), r.u32(), r.u32()
	r.u32() // Sync ID: outputs here are not sample-synchronized.
	r.cvolume()
	for range 7 { // no_remap .. variable_rate
		r.boolean()
	}
	r.boolean() // muted
	r.boolean() // adjust_latency
	props := r.proplist()
	if c.version >= 14 {
		r.boolean()
		r.boolean()
	}
	if c.version >= 15 {
		r.boolean()
		r.boolean()
		r.boolean()
	}
	if c.version >= 17 {
		r.boolean()
	}
	if c.version >= 18 {
		r.boolean()
	}
	if c.version >= 21 {
		for n := r.u8(); n > 0 && r.err == nil; n-- {
			r.formatInfo()
		}
	}
	if r.err != nil {
		return r.err
	}
	if sinkName != "" && !c.sinkMatches(sinkIndex, sinkName) {
		return c.fail(tag, errNoEntity)
	}
	bytes := BytesPerSample(int(format))
	if bytes == 0 || channels < 1 || channels > 2 || rate < 8000 || rate > 384000 || len(channelMap) != int(channels) {
		return c.fail(tag, errInvalid)
	}
	c.mu.Lock()
	if len(c.streams) >= maxStreams {
		c.mu.Unlock()
		return c.fail(tag, errInvalid)
	}
	id := c.next
	c.next++
	merged := map[string]string{}
	for k, v := range c.props {
		merged[k] = v
	}
	for k, v := range props {
		merged[k] = v
	}
	c.server.mu.Lock()
	input := c.server.inputs
	c.server.inputs++
	c.server.mu.Unlock()
	s := newStream(c, id, input, int(format), int(channels), int(rate), merged)
	s.channelMap = channelMap
	s.setCorked(corked)
	s.setAttr(maxLength, tLength, prebuf, minReq)
	c.streams[id] = s
	c.mu.Unlock()

	attr := s.attrs()
	missing := s.takeRequest()
	w := c.reply(tag).u32(id).u32(s.Input).u32(missing)
	w.u32(attr.maxLength).u32(attr.tLength).u32(attr.prebuf).u32(attr.minReq)
	w.sampleSpec(format, channels, rate).channelMap(channelMap)
	w.u32(sinkIndex).str(c.server.config.Name).boolean(false)
	w.usec(s.latencyUsec())
	if c.version >= 21 {
		w.pcmFormatInfo()
	}
	if err := c.finish(w); err != nil {
		return err
	}
	if c.server.config.OnStream != nil {
		c.server.config.OnStream(s)
	}
	return nil
}

func (c *client) data(channel, length uint32, offset int64, flags uint32) error {
	s := c.stream(channel)
	if s == nil {
		// A stream can be deleted with data already in flight.
		_, err := io.CopyN(io.Discard, c.conn, int64(length))
		return err
	}
	return s.receive(c.conn, length, offset, flags)
}

func (c *client) closeStreams() {
	c.mu.Lock()
	streams := make([]*Stream, 0, len(c.streams))
	for _, s := range c.streams {
		streams = append(streams, s)
	}
	c.streams = map[uint32]*Stream{}
	c.mu.Unlock()
	for _, s := range streams {
		s.close()
	}
}

// DecodeS32 widens raw integer samples to left-justified S32 and returns the
// count. Nothing else happens to audio here: this server has no resampling, mixing
// or format conversion, and refuses streams that would need it.
// float32BitsToS32 scales an IEEE 754 sample with integer operations only, which
// keeps every mantissa bit: Chrome sends float, as desktop clients often do. Samples
// outside -1 to 1 clip, as they would at any integer output; NaN is silence.
func float32BitsToS32(bits uint32) int32 {
	exponent := int(bits >> 23 & 0xff)
	fraction := bits & 0x7fffff
	negative := bits>>31 != 0
	if exponent == 0xff && fraction != 0 {
		return 0
	}
	shift := exponent - 119 // Moves the 24-bit mantissa to full scale at 2^31.
	if shift >= 8 {
		if negative {
			return math.MinInt32
		}
		return math.MaxInt32
	}
	if exponent == 0 || shift <= -24 {
		return 0
	}
	mantissa := fraction | 1<<23
	if shift >= 0 {
		mantissa <<= shift
	} else {
		mantissa >>= -shift
	}
	if negative {
		return -int32(mantissa)
	}
	return int32(mantissa)
}

// float32Clips reports a sample beyond full scale. Exactly -1 and 1 are not: those are
// what a limiter or normalised track produces, and lose nothing audible.
func float32Clips(bits uint32) bool {
	magnitude := bits &^ (1 << 31)
	return magnitude > 0x3f800000 && magnitude <= 0x7f800000
}

func DecodeS32(dst []int32, raw []byte, format int) int {
	size := BytesPerSample(format)
	if size == 0 {
		return 0
	}
	n := min(len(dst), len(raw)/size)
	for i := range n {
		p := raw[i*size:]
		switch format {
		case FormatS16LE:
			dst[i] = int32(int16(binary.LittleEndian.Uint16(p))) << 16
		case FormatS24LE:
			dst[i] = int32(uint32(p[0])<<8 | uint32(p[1])<<16 | uint32(p[2])<<24)
		case FormatS32LE:
			dst[i] = int32(binary.LittleEndian.Uint32(p))
		case FormatS2432LE:
			dst[i] = int32(binary.LittleEndian.Uint32(p)) << 8
		case FormatFloat32LE:
			dst[i] = float32BitsToS32(binary.LittleEndian.Uint32(p))
		}
	}
	return n
}
