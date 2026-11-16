package raop

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func dmapItem(tag string, value []byte) []byte {
	b := make([]byte, 8, 8+len(value))
	copy(b, tag)
	binary.BigEndian.PutUint32(b[4:], uint32(len(value)))
	return append(b, value...)
}

func TestParseDMAP(t *testing.T) {
	item := append(append(append(append(dmapItem("minm", []byte("Song\x00 One")), dmapItem("asar", []byte("Artist"))...),
		dmapItem("asal", []byte("Album"))...), dmapItem("asgn", []byte("Jazz"))...), dmapItem("mper", make([]byte, 8))...)
	m := parseDMAP(dmapItem("mlit", item))
	if m != (Metadata{Title: "Song One", Artist: "Artist", Album: "Album", Genre: "Jazz"}) {
		t.Fatalf("%+v", m)
	}
	// A length running past the end stops parsing instead of reading beyond it.
	bad := dmapItem("mlit", dmapItem("minm", []byte("x")))
	binary.BigEndian.PutUint32(bad[12:], 1000)
	if m := parseDMAP(bad); m.Title != "" {
		t.Fatal(m)
	}
	if m := parseDMAP(dmapItem("mlit", dmapItem("mlit", dmapItem("minm", []byte("deep"))))); m.Title != "" {
		t.Fatal("followed nested listings", m)
	}
}

func TestParseRemote(t *testing.T) {
	if r, ok := parseRemote("14d6e6e5bae3b2c7", "1986535575"); !ok || r.id != "14D6E6E5BAE3B2C7" {
		t.Fatal(r, ok)
	}
	for _, c := range [][2]string{{"", "1"}, {"XYZ", "1"}, {"ABC", ""}, {"ABC", "12a"}, {"ABC", "123456789012345678901"}, {"0123456789ABCDEF0", "1"}} {
		if _, ok := parseRemote(c[0], c[1]); ok {
			t.Fatalf("accepted %q", c)
		}
	}
}

// fakeRemote is an Apple sender's DACP service.
type fakeRemote struct {
	mu       sync.Mutex
	requests []string
	tokens   []string
}

func (f *fakeRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.tokens = append(f.tokens, r.Header.Get("Active-Remote"))
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func TestRemoteControlReachesOnlyThePeer(t *testing.T) {
	fake := &fakeRemote{}
	server := httptest.NewServer(fake)
	defer server.Close()
	endpoint := netip.MustParseAddrPort(strings.TrimPrefix(server.URL, "http://"))
	var asked []string
	control := &remoteControl{remote: remote{id: "ABC", token: "42"}, peer: endpoint.Addr(),
		resolve: func(_ context.Context, name string, _ netip.Addr) (netip.AddrPort, error) {
			asked = append(asked, name)
			return endpoint, nil
		}}
	for _, op := range []string{"toggle", "next", "prev"} {
		if err := control.send(context.Background(), op); err != nil {
			t.Fatal(op, err)
		}
	}
	if strings.Join(fake.requests, " ") != "/ctrl-int/1/playpause /ctrl-int/1/nextitem /ctrl-int/1/previtem" || fake.tokens[0] != "42" {
		t.Fatal(fake.requests, fake.tokens)
	}
	if len(asked) != 1 || asked[0] != "iTunes_Ctrl_ABC" {
		t.Fatal("resolution was not cached:", asked)
	}
	if err := control.send(context.Background(), "shuffle"); err == nil {
		t.Fatal("accepted an unknown command")
	}

	// Another host answering for the sender's name is never contacted.
	impostor := &remoteControl{remote: remote{id: "ABC", token: "42"}, peer: netip.MustParseAddr("192.0.2.7"),
		resolve: func(context.Context, string, netip.Addr) (netip.AddrPort, error) { return endpoint, nil }}
	before := len(fake.requests)
	if err := impostor.send(context.Background(), "toggle"); err == nil || len(fake.requests) != before {
		t.Fatal("sent a command to a host other than the peer:", err)
	}
	if err := (&remoteControl{}).send(context.Background(), "toggle"); !errors.Is(err, ErrNoRemote) {
		t.Fatal(err)
	}
}

func TestRemoteControlRefusesRedirects(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirect.Close()
	endpoint := netip.MustParseAddrPort(strings.TrimPrefix(redirect.URL, "http://"))
	control := &remoteControl{remote: remote{id: "ABC", token: "1"}, peer: endpoint.Addr(),
		resolve: func(context.Context, string, netip.Addr) (netip.AddrPort, error) { return endpoint, nil }}
	if err := control.send(context.Background(), "toggle"); err == nil || followed {
		t.Fatal("followed a redirect:", err)
	}
}

func TestSessionCarriesMetadataAndRemote(t *testing.T) {
	fake := &fakeRemote{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dacp := &http.Server{Handler: fake}
	go dacp.Serve(listener)
	defer dacp.Close()
	endpoint := netip.MustParseAddrPort(listener.Addr().String())
	address, streams := startServer(t, Config{BufferMillis: 20, Resolve: func(context.Context, string, netip.Addr) (netip.AddrPort, error) { return endpoint, nil }})
	s := connect(t, address)
	s.request("OPTIONS", map[string]string{"DACP-ID": "ABCDEF", "Active-Remote": "777"}, "")
	s.negotiate("")
	stream := <-streams
	body := string(dmapItem("mlit", append(dmapItem("minm", []byte("Title")), dmapItem("asar", []byte("Artist"))...)))
	s.request("SET_PARAMETER", map[string]string{"Content-Type": "application/x-dmap-tagged"}, body)
	if m := stream.Metadata(); m.Title != "Title" || m.Artist != "Artist" {
		t.Fatal(m)
	}
	if !stream.Controllable() {
		t.Fatal("remote not offered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stream.Control(ctx, "next"); err != nil || fake.tokens[len(fake.tokens)-1] != "777" {
		t.Fatal(err, fake.tokens)
	}
}
