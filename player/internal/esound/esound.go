// Package esound is a playback-only Enlightened Sound Daemon server. It accepts
// raw PCM streams and the few control requests that clients make around them.
// Like the Pulse receiver it has no authentication: the client's key is read and
// ignored, so it belongs on a trusted LAN only. Cached samples, recording and
// monitoring are not implemented, and such a request ends the connection.
package esound

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/codemowers/slopify/player/internal/pcmring"
)

const (
	keyLength = 16
	nameMax   = 128

	protoLock       = 1
	protoUnlock     = 2
	protoStreamPlay = 3
	protoStandby    = 12
	protoResume     = 13
	protoServerInfo = 16
	protoLatency    = 23

	maskBits   = 0x000f
	bits16     = 0x0001
	maskChan   = 0x00f0
	mono       = 0x0010
	stereo     = 0x0020
	maskMode   = 0x0f00
	modeStream = 0x0000

	defaultRate    = 44100
	minRate        = 8000
	maxRate        = 192000
	maxClients     = 8
	requestTimeout = 5 * time.Second
	idleTimeout    = 5 * time.Minute
	receiveChunk   = 4096
	minSocket      = 4096
)

// DefaultBufferMillis is the stream buffer unless configured otherwise.
const DefaultBufferMillis = 250

// Sample formats. ESound sends samples in the client's own byte order.
const (
	FormatU8 = iota
	FormatS16LE
	FormatS16BE
)

// BytesPerSample returns the width of a format.
func BytesPerSample(format int) int {
	if format == FormatU8 {
		return 1
	}
	return 2
}

// DecodeS32 widens samples into the top of int32, which is lossless. It returns
// the number of samples written.
func DecodeS32(dst []int32, raw []byte, format int) int {
	n := min(len(dst), len(raw)/BytesPerSample(format))
	for i := range n {
		switch format {
		case FormatU8:
			dst[i] = (int32(raw[i]) - 128) << 24
		case FormatS16LE:
			dst[i] = int32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) << 16
		case FormatS16BE:
			dst[i] = int32(int16(binary.BigEndian.Uint16(raw[i*2:]))) << 16
		}
	}
	return n
}

// Config describes the server.
type Config struct {
	// BufferMillis is how much audio the stream holds; zero means DefaultBufferMillis.
	BufferMillis int
	// SocketMillis caps the kernel's receive queue, in audio time. Zero leaves the
	// system default, which can hold about a second that only adds delay.
	SocketMillis int
	// OnStream receives each new stream and reports whether it was admitted. Its
	// owner calls Read at the output's pace, which TCP carries back to the client.
	OnStream func(*Stream) bool
}

// Server accepts ESound clients.
type Server struct {
	config  Config
	mu      sync.Mutex
	clients map[net.Conn]struct{}
	delay   atomic.Int64
}

// NewServer returns a server.
func NewServer(config Config) *Server {
	if config.BufferMillis <= 0 {
		config.BufferMillis = DefaultBufferMillis
	}
	return &Server{config: config, clients: map[net.Conn]struct{}{}}
}

// Serve accepts connections until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		l.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for conn := range s.clients {
			conn.Close()
		}
	}()
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
		s.clients[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				conn.Close()
				s.mu.Lock()
				delete(s.clients, conn)
				s.mu.Unlock()
			}()
			if err := s.run(conn); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Info("esound client ended", "peer", peerHost(conn), "err", err)
			}
		}()
	}
}

func peerHost(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}

func (s *Server) run(conn net.Conn) error {
	conn.SetDeadline(time.Now().Add(requestTimeout))
	order, err := handshake(conn)
	if err != nil {
		return err
	}
	if err := writeInts(conn, order, 1); err != nil {
		return err
	}
	for {
		conn.SetDeadline(time.Now().Add(idleTimeout))
		var request [4]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			return err
		}
		conn.SetDeadline(time.Now().Add(requestTimeout))
		switch proto := order.Uint32(request[:]); proto {
		case protoStreamPlay:
			return s.play(conn, order)
		case protoLock, protoUnlock, protoStandby, protoResume:
			// The client authenticates again, then expects both results.
			if _, err := handshake(conn); err != nil {
				return err
			}
			if err := writeInts(conn, order, 1, 1); err != nil {
				return err
			}
		case protoServerInfo:
			var version [4]byte
			if _, err := io.ReadFull(conn, version[:]); err != nil {
				return err
			}
			if err := writeInts(conn, order, 0, defaultRate, bits16|stereo); err != nil {
				return err
			}
		case protoLatency:
			// Reported as bytes of 44.1 kHz 16-bit stereo.
			usec := int64(s.config.BufferMillis)*1000 + s.delay.Load()
			if err := writeInts(conn, order, uint32(usec*defaultRate*4/1_000_000)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported request %d", proto)
		}
	}
}

// handshake reads a key, which is ignored, and the client's byte order.
func handshake(conn net.Conn) (binary.ByteOrder, error) {
	var b [keyLength + 4]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return nil, err
	}
	switch string(b[keyLength:]) {
	case "NDNE":
		return binary.LittleEndian, nil
	case "ENDN":
		return binary.BigEndian, nil
	}
	return nil, errors.New("not an ESound client")
}

func writeInts(conn net.Conn, order binary.ByteOrder, values ...uint32) error {
	b := make([]byte, len(values)*4)
	for i, v := range values {
		order.PutUint32(b[i*4:], v)
	}
	_, err := conn.Write(b)
	return err
}

func (s *Server) play(conn net.Conn, order binary.ByteOrder) error {
	var b [8 + nameMax]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return err
	}
	format, rate := order.Uint32(b[:]), order.Uint32(b[4:])
	stream, err := newStream(conn, s, format, rate, cleanName(b[8:]), order)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Time{})
	stream.limitSocket(s.config.SocketMillis)
	if s.config.OnStream == nil || !s.config.OnStream(stream) {
		return errors.New("stream not admitted")
	}
	chunk := make([]byte, receiveChunk)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			stream.Write(chunk[:n])
		}
		if err != nil {
			stream.Finish()
			<-stream.Done()
			return nil
		}
	}
}

// cleanName keeps a client's stream name safe to display.
func cleanName(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(string(b), ""))
	return strings.TrimSpace(name)
}

// Stats describes a stream's buffer.
type Stats struct {
	BufferedUsec, TargetUsec uint64
	Underflows               uint64
}

// Stream is one client's PCM stream.
type Stream struct {
	*pcmring.Ring
	Format, Rate, Channels int
	Name, Peer             string

	server *Server
	conn   net.Conn
}

func newStream(conn net.Conn, server *Server, format, rate uint32, name string, order binary.ByteOrder) (*Stream, error) {
	if format&maskMode != modeStream {
		return nil, fmt.Errorf("unsupported stream mode %#x", format&maskMode)
	}
	if rate < minRate || rate > maxRate {
		return nil, fmt.Errorf("unsupported rate %d", rate)
	}
	s := &Stream{Rate: int(rate), Name: name, Peer: peerHost(conn), server: server, conn: conn}
	switch format & maskChan {
	case mono:
		s.Channels = 1
	case stereo:
		s.Channels = 2
	default:
		return nil, fmt.Errorf("unsupported channel layout %#x", format&maskChan)
	}
	switch format & maskBits {
	case 0:
		s.Format = FormatU8
	case bits16:
		s.Format = FormatS16LE
		if order == binary.ByteOrder(binary.BigEndian) {
			s.Format = FormatS16BE
		}
	default:
		return nil, fmt.Errorf("unsupported sample width %#x", format&maskBits)
	}
	frame := s.Channels * BytesPerSample(s.Format)
	size := max(s.Rate*server.config.BufferMillis/1000, 1) * frame
	s.Ring = pcmring.New(size, size/2/frame*frame, frame, func() { conn.Close() })
	return s, nil
}

// limitSocket keeps audio from queueing in the kernel, where it would only add
// delay: a client writes ahead until TCP holds it back. Linux doubles the value set.
func (s *Stream) limitSocket(millis int) {
	if tcp, ok := s.conn.(*net.TCPConn); ok && millis > 0 {
		size := s.Rate * s.frameSize() * millis / 1000 / 2
		tcp.SetReadBuffer(max(size, minSocket))
	}
}

func (s *Stream) frameSize() int { return s.Channels * BytesPerSample(s.Format) }

// Stats returns the buffer's current state.
func (s *Stream) Stats() Stats {
	size, underflows := s.Ring.Stats()
	rate := uint64(s.Rate) * uint64(s.frameSize())
	return Stats{BufferedUsec: uint64(size) * 1_000_000 / rate, TargetUsec: uint64(s.server.config.BufferMillis) * 1000, Underflows: underflows}
}

// ObserveReceive reports gaps between arrivals from the network.
func (s *Stream) ObserveReceive(observe func(time.Duration)) { s.Observe(observe) }

// SetDeviceDelay records the output's delay for latency requests.
func (s *Stream) SetDeviceDelay(usec uint64) { s.server.delay.Store(int64(usec)) }
