package main

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codemowers/slopify/internal/volume"
	"github.com/codemowers/slopify/player/internal/audiodec"
)

func TestCheckUPNPURI(t *testing.T) {
	for _, bad := range []string{"", "https://192.0.2.1/", "file:///etc/passwd", "http://user:pass@192.0.2.1/", "http://", strings.Repeat("a", upnpMaxURI+1)} {
		if checkUPNPURI(bad) == nil {
			t.Fatal(bad)
		}
	}
	if err := checkUPNPURI("http://192.168.1.5:5901/stream/swyh.flac?bd=24"); err != nil {
		t.Fatal(err)
	}
}

func TestStreamsStayOnTheLocalNetwork(t *testing.T) {
	for address, ok := range map[string]bool{"192.168.1.5:80": true, "10.0.0.1:80": true, "172.16.0.1:80": true, "[fe80::1]:80": true,
		"127.0.0.1:8080": false, "[::1]:80": false, "8.8.8.8:80": false, "169.254.169.254:80": true} {
		if err := lanOnlyControl("tcp4", address, nil); (err == nil) != ok {
			t.Fatal(address, err)
		}
	}
	// The dial check applies to real connections, including this loopback server.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if res, err := newUPNPClient().Get(srv.URL); err == nil {
		res.Body.Close()
		t.Fatal("connected to loopback")
	}
}

func TestStreamIsNotFollowedThroughRedirects(t *testing.T) {
	client := newUPNPClient()
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects are followed")
	}
}

func TestUPNPStreamDeliversSamplesUnchanged(t *testing.T) {
	stream := &upnpStream{uri: "http://192.0.2.5:5901/stream/swyh.raw?secret=1", cancel: func() {}}
	if err := stream.prepare(audiodec.Format{Rate: 8000, Channels: 2, Bits: 16}); err != nil {
		t.Fatal(err)
	}
	if stream.Peer() != "192.0.2.5" || strings.Contains(stream.Props()["application.process.host"], "secret") {
		t.Fatal("address details leak", stream.Props())
	}
	raw := make([]byte, 16)
	want := []int32{1 << 16, -1 << 16, 0x7fff << 16, -0x8000 << 16}
	for i, v := range want {
		binary.LittleEndian.PutUint32(raw[i*4:], uint32(v))
	}
	if !stream.ring.Write(raw) {
		t.Fatal("write refused")
	}
	buf := make([]byte, 64)
	if stream.Read(buf) != 0 {
		t.Fatal("played before the start threshold")
	}
	stream.ring.Finish()
	n := stream.Read(buf)
	got := make([]int32, 4)
	if n != 16 || stream.Decode(got, buf[:n]) != 4 {
		t.Fatal(n)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream did not end after draining")
	}
	if stream.ring.Write(raw) {
		t.Fatal("write accepted after the end")
	}
}

func TestUPNPStreamClosesBeforeItsFormatIsKnown(t *testing.T) {
	cancelled := false
	stream := &upnpStream{uri: "http://192.0.2.5/", cancel: func() { cancelled = true }}
	if stream.ended() {
		t.Fatal("ended before starting")
	}
	stream.Close()
	if !cancelled || !stream.ended() {
		t.Fatal("close did not end the stream")
	}
	if stream.prepare(audiodec.Format{Rate: 8000, Channels: 1, Bits: 16}) == nil {
		t.Fatal("prepared after close")
	}
}

func TestUPNPVolumeUsesHardwareRange(t *testing.T) {
	c := volume.Control{Min: 0, Max: 40, Level: 20}
	if got := int(senderVolume(c.Level, c)) * 100 / 0x10000; got != 50 {
		t.Fatal(got)
	}
	if got := senderLevel(uint32(50)*0x10000/100, c); got != 20 {
		t.Fatal(got)
	}
}
