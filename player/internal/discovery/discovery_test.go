package discovery

import (
	"fmt"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func browser() *Browser { return &Browser{id: "self", records: map[string]Peer{}} }
func peer(name, host, address string, port uint16) resolved {
	return resolved{Key: serviceKey{Interface: 1, Protocol: 0, Name: name, Type: service, Domain: "local"}, Host: host, Address: address, Port: port}
}
func TestDiscoveryNamesPortsSelfAndDeduplication(t *testing.T) {
	b := browser()
	r := peer("HiFiBerry Amp4", "hifiberry-amp4.local", "192.168.1.2", 80)
	b.record(r)
	r.Key.Interface = 2
	b.record(r)
	p := b.Snapshot()
	if len(p) != 1 || p[0].Name != "HiFiBerry Amp4" || p[0].URL != "http://hifiberry-amp4.local/" {
		t.Fatalf("peers %+v", p)
	}
	b.clear()
	r.TXT = [][]byte{[]byte("id=self")}
	b.record(r)
	if len(b.Snapshot()) != 0 {
		t.Fatal("self listed")
	}
	r.TXT = nil
	r.Port = 8080
	b.record(r)
	if b.Snapshot()[0].URL != "http://hifiberry-amp4.local:8080/" {
		t.Fatal("lost port")
	}
}
func TestDiscoveryRejectsUnsafeURLsAndBounds(t *testing.T) {
	b := browser()
	for _, host := range []string{"example.com", "evil.local/anything", "evil.local@public.com", "-x.local", "x-.local", "x..local", strings.Repeat("a", 64) + ".local"} {
		b.record(peer("Bad", host, "8.8.8.8", 80))
	}
	b.record(peer("Bad", "good.local", "192.168.1.2", 0))
	if len(b.Snapshot()) != 0 {
		t.Fatalf("unsafe records %+v", b.Snapshot())
	}
	for i := 0; i < 100; i++ {
		b.record(peer(fmt.Sprint(i), fmt.Sprintf("peer%d.local", i), "192.168.1.2", 80))
	}
	if len(b.Snapshot()) != maxRecords {
		t.Fatalf("bound %d", len(b.Snapshot()))
	}
	b.clear()
	b.record(peer("Player", "invalid/host", "fd00::2", 80))
	if p := b.Snapshot(); len(p) != 1 || p[0].URL != "http://[fd00::2]/" {
		t.Fatalf("IPv6 %+v", p)
	}
}
func TestSignalOverflowIsBoundedAndDetected(t *testing.T) {
	s := &signals{events: make(chan *dbus.Signal, 2), overflow: make(chan struct{}, 1)}
	for range 100 {
		s.DeliverSignal("", "", &dbus.Signal{})
	}
	if len(s.events) != 2 || len(s.overflow) != 1 {
		t.Fatal("unbounded/lost overflow")
	}
}
