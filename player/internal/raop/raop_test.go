package raop

import (
	"bufio"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// alacFrame builds the uncompressed ALAC frame PipeWire and PulseAudio send.
func alacFrame(samples []int16) []byte {
	var bit int
	out := make([]byte, 16+len(samples)*2)
	put := func(v uint32, n int) {
		for i := n - 1; i >= 0; i-- {
			if v>>i&1 == 1 {
				out[bit>>3] |= 0x80 >> (bit & 7)
			}
			bit++
		}
	}
	put(1, 3)
	put(0, 16)
	put(1, 1)
	put(0, 2)
	put(1, 1)
	put(uint32(len(samples)/2), 32)
	for _, s := range samples {
		put(uint32(uint16(s)), 16)
	}
	put(7, 3)
	return out[:bit/8+1]
}

type sender struct {
	t        *testing.T
	conn     net.Conn
	br       *bufio.Reader
	cseq     int
	control  *net.UDPConn
	timing   *net.UDPConn
	audio    *net.UDPConn
	block    cipher.Block
	iv       []byte
	controlS int
}

func (s *sender) request(method string, headers map[string]string, body string) (int, map[string]string) {
	s.t.Helper()
	s.cseq++
	var b strings.Builder
	fmt.Fprintf(&b, "%s rtsp://127.0.0.1/1 RTSP/1.0\r\nCSeq: %d\r\n", method, s.cseq)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n" + body)
	if _, err := s.conn.Write([]byte(b.String())); err != nil {
		s.t.Fatal(err)
	}
	line, err := s.br.ReadString('\n')
	if err != nil {
		s.t.Fatal(err)
	}
	status, _ := strconv.Atoi(strings.Fields(line)[1])
	reply := map[string]string{}
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			s.t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		k, v, _ := strings.Cut(line, ":")
		reply[k] = strings.TrimSpace(v)
	}
	if n, _ := strconv.Atoi(reply["Content-Length"]); n > 0 {
		s.br.Discard(n)
	}
	return status, reply
}

func (s *sender) send(seq uint16, samples []int16) {
	s.t.Helper()
	payload := alacFrame(samples)
	if s.block != nil {
		n := len(payload) &^ 15
		cipher.NewCBCEncrypter(s.block, s.iv).CryptBlocks(payload[:n], payload[:n])
	}
	packet := make([]byte, 12, 12+len(payload))
	packet[0], packet[1] = 0x80, 0x60
	binary.BigEndian.PutUint16(packet[2:], seq)
	s.audio.Write(append(packet, payload...))
}

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func startServer(t *testing.T, config Config) (string, <-chan *Stream) {
	t.Helper()
	streams := make(chan *Stream, 4)
	config.OnStream = func(s *Stream) bool { streams <- s; return true }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewServer(config).Serve(ctx, listener)
	return listener.Addr().String(), streams
}

func connect(t *testing.T, address string) *sender {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return &sender{t: t, conn: conn, br: bufio.NewReader(conn), control: listenUDP(t), timing: listenUDP(t)}
}

const fmtp = "96 352 0 16 40 10 14 2 255 0 0 44100"

func (s *sender) negotiate(extraSDP string) {
	s.t.Helper()
	sdp := "v=0\r\nm=audio 0 RTP/AVP 96\r\na=rtpmap:96 AppleLossless\r\na=fmtp:" + fmtp + "\r\n" + extraSDP
	if status, _ := s.request("ANNOUNCE", map[string]string{"Content-Type": "application/sdp"}, sdp); status != 200 {
		s.t.Fatalf("announce: %d", status)
	}
	status, reply := s.request("SETUP", map[string]string{"Transport": fmt.Sprintf("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		s.control.LocalAddr().(*net.UDPAddr).Port, s.timing.LocalAddr().(*net.UDPAddr).Port)}, "")
	if status != 200 || reply["Session"] == "" || reply["Audio-Jack-Status"] == "" {
		s.t.Fatalf("setup: %d %v", status, reply)
	}
	for _, field := range strings.Split(reply["Transport"], ";") {
		k, v, _ := strings.Cut(field, "=")
		port, _ := strconv.Atoi(v)
		switch k {
		case "server_port":
			s.audio, _ = net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		case "control_port":
			s.controlS = port
		}
	}
	if s.audio == nil || s.controlS == 0 {
		s.t.Fatalf("transport %q", reply["Transport"])
	}
	s.t.Cleanup(func() { s.audio.Close() })
	if status, reply := s.request("RECORD", map[string]string{"RTP-Info": "seq=100;rtptime=0"}, ""); status != 200 || reply["Audio-Latency"] == "" {
		s.t.Fatalf("record: %d %v", status, reply)
	}
}

// drain reads samples from a stream until want have arrived.
func drain(t *testing.T, s *Stream, want int) []int32 {
	t.Helper()
	var got []int32
	raw := make([]byte, 4096)
	samples := make([]int32, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < want && time.Now().Before(deadline) {
		if n := s.Read(raw); n > 0 {
			got = append(got, samples[:s.Decode(samples, raw[:n])]...)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return got
}

func TestEncryptedSessionWithRetransmission(t *testing.T) {
	key := testKey()
	id := HardwareID("test")
	address, streams := startServer(t, Config{Key: key, HardwareID: id, BufferMillis: 20})
	s := connect(t, address)

	challenge := make([]byte, 16)
	rand.Read(challenge)
	status, reply := s.request("OPTIONS", map[string]string{"Apple-Challenge": base64.RawStdEncoding.EncodeToString(challenge)}, "")
	signature, err := base64.RawStdEncoding.DecodeString(reply["Apple-Response"])
	if status != 200 || err != nil {
		t.Fatal(status, reply, err)
	}
	signed := append(append(append([]byte(nil), challenge...), 127, 0, 0, 1), id[:]...)
	signed = append(signed, make([]byte, 32-len(signed))...)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.Hash(0), signed, signature); err != nil {
		t.Fatal("Apple-Response does not verify:", err)
	}

	aesKey, iv := make([]byte, 16), make([]byte, 16)
	rand.Read(aesKey)
	rand.Read(iv)
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &key.PublicKey, aesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.block, _ = aes.NewCipher(aesKey)
	s.iv = iv
	s.negotiate("a=rsaaeskey:" + base64.RawStdEncoding.EncodeToString(wrapped) + "\r\na=aesiv:" + base64.RawStdEncoding.EncodeToString(iv) + "\r\n")
	stream := <-streams

	packet := func(seq uint16) []int16 {
		samples := make([]int16, 704)
		for i := range samples {
			samples[i] = int16(seq)*7 + int16(i)
		}
		return samples
	}
	s.send(100, packet(100))
	s.send(102, packet(102)) // 101 is lost on its way.
	s.control.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, from, err := s.control.ReadFromUDP(buf)
	if err != nil || n != 8 || buf[1] != 0xd5 || binary.BigEndian.Uint16(buf[4:]) != 101 || binary.BigEndian.Uint16(buf[6:]) != 1 {
		t.Fatalf("resend request: %x %v", buf[:n], err)
	}
	payload := alacFrame(packet(101))
	nb := len(payload) &^ 15
	cipher.NewCBCEncrypter(s.block, s.iv).CryptBlocks(payload[:nb], payload[:nb])
	resent := append([]byte{0x80, 0xd6, 0, 1, 0x80, 0x60, 0, 101}, make([]byte, 8)...)
	s.control.WriteToUDP(append(resent, payload...), from)

	got := drain(t, stream, 3*704)
	if len(got) != 3*704 {
		t.Fatalf("got %d samples", len(got))
	}
	for p, seq := range []uint16{100, 101, 102} {
		for i, want := range packet(seq) {
			if got[p*704+i] != int32(want)<<16 {
				t.Fatalf("packet %d sample %d: %d, want %d", seq, i, got[p*704+i]>>16, want)
			}
		}
	}
	if stream.Stats().Lost != 0 {
		t.Fatal("a retransmitted packet was counted as lost")
	}
}

func TestPlainSessionFillsGapsAndReportsVolume(t *testing.T) {
	volumes := make(chan float64, 4)
	address, streams := startServer(t, Config{BufferMillis: 20, OnVolume: func(_ *Stream, db float64) { volumes <- db }})
	s := connect(t, address)
	s.negotiate("")
	stream := <-streams
	ones := make([]int16, 704)
	for i := range ones {
		ones[i] = 1
	}
	s.send(100, ones)
	s.send(102, ones)            // 101 never arrives, and nobody resends it.
	long := make([]int16, 359*2) // Filled to the MTU, beyond the announced 352.
	s.send(103, long)
	got := drain(t, stream, 3*704+718)
	if len(got) != 3*704+718 || got[704] != 0 || got[2*704] != 1<<16 {
		t.Fatalf("gap was not filled with silence: %d samples", len(got))
	}
	if stream.Stats().Lost != 352 {
		t.Fatal(stream.Stats())
	}
	s.request("SET_PARAMETER", map[string]string{"Content-Type": "text/parameters"}, "volume: -15.5\r\n")
	s.request("SET_PARAMETER", map[string]string{"Content-Type": "text/parameters"}, "volume: 12\r\n")
	if db := <-volumes; db != -15.5 {
		t.Fatal(db)
	}
	select {
	case db := <-volumes:
		t.Fatal("accepted out-of-range volume", db)
	default:
	}
	s.request("TEARDOWN", nil, "")
	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not end the stream")
	}
}

func TestRefusesWhatItCannotPlay(t *testing.T) {
	address, _ := startServer(t, Config{})
	s := connect(t, address)
	for _, sdp := range []string{
		"a=rtpmap:96 mpeg4-generic/44100/2\r\na=fmtp:" + fmtp + "\r\n",
		"a=rtpmap:96 AppleLossless\r\na=fmtp:96 352 0 8 40 10 14 2 255 0 0 44100\r\n",
		"a=rtpmap:96 AppleLossless\r\na=fmtp:96 352\r\n",
		"a=rtpmap:96 AppleLossless\r\na=fmtp:" + fmtp + "\r\na=fpaeskey:AAAA\r\n",
		"a=rtpmap:96 AppleLossless\r\na=fmtp:" + fmtp + "\r\na=rsaaeskey:AAAA\r\na=aesiv:AAAA\r\n", // No key configured.
	} {
		if status, _ := s.request("ANNOUNCE", nil, sdp); status != 415 {
			t.Fatalf("%q: %d", sdp, status)
		}
	}
	if status, _ := s.request("SETUP", map[string]string{"Transport": "RTP/AVP/UDP;unicast"}, ""); status != 455 {
		t.Fatal("setup before announce:", status)
	}
	s.request("ANNOUNCE", nil, "a=rtpmap:96 AppleLossless\r\na=fmtp:"+fmtp+"\r\n")
	if status, _ := s.request("SETUP", map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1"}, ""); status != 461 {
		t.Fatal("accepted TCP transport:", status)
	}
}

func TestLoadKeyAcceptsOnlyTheRAOPKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	der := x509.MarshalPKCS1PrivateKey(testKey())
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), 0o600)
	if _, err := LoadKey(path); err == nil || !strings.Contains(err.Error(), "not the RAOP key") {
		t.Fatal("accepted a different key:", err)
	}
	os.WriteFile(path, []byte("garbage"), 0o600)
	if _, err := LoadKey(path); err == nil {
		t.Fatal("accepted garbage")
	}
}

func TestAdvertisement(t *testing.T) {
	id := HardwareID("x")
	if id[0]&3 != 2 || HardwareID("x") != id || HardwareID("y") == id {
		t.Fatalf("hardware ID %x", id)
	}
	name, txt := Advertisement("Kitchen", id, false)
	if !strings.HasSuffix(name, "@Kitchen") || len(name) != 12+1+7 {
		t.Fatal(name)
	}
	joined := strings.Join(txt, " ")
	if !strings.Contains(joined, "et=0") || strings.Contains(joined, "et=0,1") || !strings.Contains(joined, "tp=UDP") {
		t.Fatal(joined)
	}
	if _, txt := Advertisement("Kitchen", id, true); !strings.Contains(strings.Join(txt, " "), "et=0,1") {
		t.Fatal(txt)
	}
}
