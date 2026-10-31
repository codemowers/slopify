package esound

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func startServer(t *testing.T, onStream func(*Stream) bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewServer(Config{OnStream: onStream}).Serve(ctx, l)
	return l.Addr().String()
}

func dial(t *testing.T, addr string, order binary.ByteOrder) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	key := append(make([]byte, keyLength), "NDNE"...)
	if order == binary.ByteOrder(binary.BigEndian) {
		key = append(make([]byte, keyLength), "ENDN"...)
	}
	conn.Write(key)
	expect(t, conn, order, 1)
	return conn
}

func expect(t *testing.T, conn net.Conn, order binary.ByteOrder, want ...uint32) {
	t.Helper()
	for _, v := range want {
		var b [4]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			t.Fatal(err)
		}
		if got := order.Uint32(b[:]); got != v {
			t.Fatalf("reply %d, want %d", got, v)
		}
	}
}

func request(conn net.Conn, order binary.ByteOrder, values ...uint32) {
	b := make([]byte, len(values)*4)
	for i, v := range values {
		order.PutUint32(b[i*4:], v)
	}
	conn.Write(b)
}

func play(conn net.Conn, order binary.ByteOrder, format, rate uint32, name string) {
	request(conn, order, protoStreamPlay, format, rate)
	field := make([]byte, nameMax)
	copy(field, name)
	conn.Write(field)
}

func readAll(t *testing.T, stream *Stream, want int) []byte {
	t.Helper()
	var got []byte
	buf := make([]byte, 4096)
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		n := stream.Read(buf)
		got = append(got, buf[:n]...)
		if n == 0 {
			select {
			case <-deadline:
				t.Fatalf("read %d of %d bytes", len(got), want)
			case <-time.After(time.Millisecond):
			}
		}
	}
	return got
}

func TestStreamDeliversSamplesAndDrainsAfterClose(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		streams := make(chan *Stream, 1)
		addr := startServer(t, func(s *Stream) bool { streams <- s; return true })
		conn := dial(t, addr, order)
		play(conn, order, bits16|stereo|0x1000, 48000, "xmms\x00junk")
		stream := <-streams
		wantFormat := FormatS16LE
		if order == binary.ByteOrder(binary.BigEndian) {
			wantFormat = FormatS16BE
		}
		if stream.Format != wantFormat || stream.Rate != 48000 || stream.Channels != 2 || stream.Name != "xmms" || stream.Peer != "127.0.0.1" {
			t.Fatalf("stream %+v", stream)
		}
		// Less than the start threshold still plays once the client has finished.
		audio := bytes.Repeat([]byte{1, 2, 3, 4}, 100)
		conn.Write(audio)
		conn.Close()
		if got := readAll(t, stream, len(audio)); !bytes.Equal(got, audio) {
			t.Fatal("audio changed")
		}
		select {
		case <-stream.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not end")
		}
	}
}

func TestFullBufferHoldsBackClientAndCloseReleasesIt(t *testing.T) {
	streams := make(chan *Stream, 1)
	addr := startServer(t, func(s *Stream) bool { streams <- s; return true })
	conn := dial(t, addr, binary.LittleEndian)
	play(conn, binary.LittleEndian, bits16|stereo, 8000, "")
	stream := <-streams
	conn.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	big := make([]byte, 64<<20)
	n, err := conn.Write(big)
	if err == nil || n == len(big) {
		t.Fatal("client was not held back")
	}
	stream.Close()
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("close did not end the stream")
	}
	if stream.Read(make([]byte, 16)) != 0 {
		t.Fatal("read after close")
	}
}

func TestControlRequests(t *testing.T) {
	addr := startServer(t, nil)
	order := binary.ByteOrder(binary.LittleEndian)
	conn := dial(t, addr, order)
	request(conn, order, protoServerInfo, 0)
	expect(t, conn, order, 0, defaultRate, bits16|stereo)
	request(conn, order, protoStandby)
	conn.Write(append(make([]byte, keyLength), "NDNE"...))
	expect(t, conn, order, 1, 1)
	request(conn, order, protoLatency)
	expect(t, conn, order, DefaultBufferMillis*defaultRate*4/1000)
	// A request this server does not support ends the connection.
	request(conn, order, 6)
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("unsupported request was accepted")
	}
}

func TestRejectsBadClients(t *testing.T) {
	addr := startServer(t, func(*Stream) bool { t.Error("bad stream admitted"); return false })
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	raw.Write(make([]byte, keyLength+4))
	if _, err := raw.Read(make([]byte, 1)); err == nil {
		t.Fatal("bad endian key accepted")
	}
	order := binary.ByteOrder(binary.LittleEndian)
	for _, format := range []uint32{bits16 | stereo | 0x0100, bits16 | 0x40, 2 | stereo} {
		conn := dial(t, addr, order)
		play(conn, order, format, 44100, "")
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatalf("format %#x accepted", format)
		}
	}
	conn := dial(t, addr, order)
	play(conn, order, bits16|stereo, 1_000_000, "")
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("rate accepted")
	}
}

func TestDecodeIsLossless(t *testing.T) {
	dst := make([]int32, 2)
	if n := DecodeS32(dst, []byte{0, 255}, FormatU8); n != 2 || dst[0] != -128<<24 || dst[1] != 127<<24 {
		t.Fatal("U8", dst)
	}
	if n := DecodeS32(dst, []byte{0x34, 0x12, 0xff, 0xff}, FormatS16LE); n != 2 || dst[0] != 0x1234<<16 || dst[1] != -1<<16 {
		t.Fatal("S16LE", dst)
	}
	if n := DecodeS32(dst, []byte{0x12, 0x34, 0xff, 0xff}, FormatS16BE); n != 2 || dst[0] != 0x1234<<16 || dst[1] != -1<<16 {
		t.Fatal("S16BE", dst)
	}
}

func TestCleanName(t *testing.T) {
	if got := cleanName(append([]byte(" a\x07\xffb "), 0, 'c')); got != "ab" {
		t.Fatalf("%q", got)
	}
}
