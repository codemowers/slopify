//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func openTCPConnections(t *testing.T, host string) float64 {
	t.Helper()
	families, err := nativeMetricsRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "slopify_tcp_connections" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if label := metric.GetLabel()[0]; label.GetName() == "source" && label.GetValue() == host {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func TestCloudDialerSetsTCPOptionsAndTracksConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		io.Copy(peer, peer)
	}()
	tuning := tcpTuning{DialTimeout: time.Second, UserTimeout: 7 * time.Second, KeepAliveIdle: 11 * time.Second, KeepAliveInterval: 3 * time.Second, KeepAliveCount: 2}
	set := newTCPConnSet()
	opened := openTCPConnections(t, "127.0.0.1")
	conn, err := set.dial(tuning.dialer())(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if got := openTCPConnections(t, "127.0.0.1"); got != opened+1 {
		t.Fatalf("open connections = %v, want %v", got, opened+1)
	}
	tracked := conn.(*trackedConn)
	get := func(level, option int) int {
		var value int
		var optErr error
		if err := tracked.raw.Control(func(fd uintptr) { value, optErr = unix.GetsockoptInt(int(fd), level, option) }); err != nil {
			t.Fatal(err)
		}
		if optErr != nil {
			t.Fatal(optErr)
		}
		return value
	}
	for name, tc := range map[string]struct{ level, option, want int }{
		"user timeout": {unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, 7000},
		"keepalive":    {unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
		"idle":         {unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, 11},
		"interval":     {unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, 3},
		"count":        {unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 2},
	} {
		if got := get(tc.level, tc.option); got != tc.want {
			t.Errorf("%s = %d, want %d", name, got, tc.want)
		}
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	sample, ok := readTCPSample(tracked.raw)
	if !ok || sample.rtt <= 0 || sample.segments == 0 {
		t.Fatalf("sample = %+v, ok = %v", sample, ok)
	}
	conn.Close()
	conn.Close()
	if got := openTCPConnections(t, "127.0.0.1"); got != opened {
		t.Fatalf("open connections after close = %v, want %v", got, opened)
	}
	if len(set.conns) != 0 {
		t.Fatal("closed connection is still tracked")
	}
}

func TestTCPHostLabelIsBounded(t *testing.T) {
	set := newTCPConnSet()
	if got := set.hostLabel("Audio.Example.COM.:443"); got != "audio.example.com" {
		t.Fatalf("label = %q", got)
	}
	for i := 1; i < maxTCPHostLabels; i++ {
		set.hostLabel(fmt.Sprintf("cdn%d.example.com:443", i))
	}
	if got := set.hostLabel("late.example.com:443"); got != "other" {
		t.Fatalf("host past the cap = %q", got)
	}
	if got := set.hostLabel("audio.example.com:80"); got != "audio.example.com" {
		t.Fatalf("known host past the cap = %q", got)
	}
}

func TestTCPHostWildcards(t *testing.T) {
	set := newTCPConnSet()
	for address, want := range map[string]string{
		"audio-ak.spotifycdn.com:443": "*.spotifycdn.com",
		"spotify.com:443":             "*.spotify.com",
		"A.B.scdn.co.:443":            "*.scdn.co",
		"notspotify.com:443":          "notspotify.com",
	} {
		if got := set.hostLabel(address); got != want {
			t.Errorf("%s = %q, want %q", address, got, want)
		}
	}
	if len(set.hosts) != 1 {
		t.Errorf("wildcards used the host cap: %v", set.hosts)
	}
}

func TestTCPErrorReasons(t *testing.T) {
	for err, want := range map[error]string{
		nil:                 "",
		io.EOF:              "",
		net.ErrClosed:       "",
		syscall.ETIMEDOUT:   "timeout",
		syscall.ECONNRESET:  "reset",
		syscall.ENETUNREACH: "unreachable",
		syscall.EACCES:      "other",
	} {
		if got := tcpErrorReason(err); got != want {
			t.Errorf("%v = %q, want %q", err, got, want)
		}
	}
}
