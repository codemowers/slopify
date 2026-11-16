package raop

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/codemowers/slopify/player/internal/cleantext"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Metadata is the sender's description of the current track.
type Metadata struct {
	Title, Artist, Album, Genre string
}

// parseDMAP reads the track tags an Apple sender puts in a DMAP listing item.
// Unknown tags, artwork and anything nested deeper are ignored.
func parseDMAP(b []byte) Metadata {
	var m Metadata
	var walk func([]byte, int)
	walk = func(b []byte, depth int) {
		for len(b) >= 8 {
			tag := string(b[:4])
			size := binary.BigEndian.Uint32(b[4:])
			if uint64(size) > uint64(len(b)-8) {
				return
			}
			value := b[8 : 8+size]
			switch tag {
			case "mlit":
				if depth == 0 {
					walk(value, 1)
				}
			case "minm":
				m.Title = cleantext.Line(string(value), nameMax)
			case "asar":
				m.Artist = cleantext.Line(string(value), nameMax)
			case "asal":
				m.Album = cleantext.Line(string(value), nameMax)
			case "asgn":
				m.Genre = cleantext.Line(string(value), nameMax)
			}
			b = b[8+size:]
		}
	}
	walk(b, 0)
	return m
}

// remote is a sender's DACP identity. The Active-Remote token authorises commands
// to the sender, so it is kept in memory only: never logged, shown or persisted.
type remote struct {
	id, token string
}

func parseRemote(id, token string) (remote, bool) {
	if id == "" || len(id) > 16 || strings.Trim(strings.ToUpper(id), "0123456789ABCDEF") != "" {
		return remote{}, false
	}
	if token == "" || len(token) > 20 || strings.Trim(token, "0123456789") != "" {
		return remote{}, false
	}
	return remote{id: strings.ToUpper(id), token: token}, true
}

// Resolver finds a sender's DACP service, in the address family of peer.
type Resolver func(ctx context.Context, name string, peer netip.Addr) (netip.AddrPort, error)

// ErrNoRemote means the sender offered no remote control.
var ErrNoRemote = errors.New("sender offers no remote control")

var dacpCommands = map[string]string{
	"toggle": "playpause", "play": "play", "pause": "pause", "stop": "pause",
	"next": "nextitem", "prev": "previtem",
}

// dacpClient never follows redirects or uses a proxy: each URL is a literal address
// that was checked to be the session's own peer.
var dacpClient = &http.Client{
	Timeout:       3 * time.Second,
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 8 << 10},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type remoteControl struct {
	remote  remote
	peer    netip.Addr
	resolve Resolver

	mu       sync.Mutex
	endpoint netip.AddrPort
	expires  time.Time
}

func (r *remoteControl) available() bool { return r != nil && r.remote.id != "" && r.resolve != nil }

func (r *remoteControl) target(ctx context.Context) (netip.AddrPort, error) {
	r.mu.Lock()
	if r.endpoint.IsValid() && time.Now().Before(r.expires) {
		defer r.mu.Unlock()
		return r.endpoint, nil
	}
	r.mu.Unlock()
	endpoint, err := r.resolve(ctx, "iTunes_Ctrl_"+r.remote.id, r.peer)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("finding the sender's remote control: %w", err)
	}
	// A DACP name is only a claim. Commands go to the streaming device or nowhere.
	if endpoint.Addr() != r.peer {
		return netip.AddrPort{}, errors.New("remote control is advertised by another host")
	}
	r.mu.Lock()
	r.endpoint, r.expires = endpoint, time.Now().Add(time.Minute)
	r.mu.Unlock()
	return endpoint, nil
}

func (r *remoteControl) forget() {
	r.mu.Lock()
	r.endpoint = netip.AddrPort{}
	r.mu.Unlock()
}

func (r *remoteControl) send(ctx context.Context, op string) error {
	command, ok := dacpCommands[op]
	if !ok {
		return fmt.Errorf("unsupported remote command %q", op)
	}
	if !r.available() {
		return ErrNoRemote
	}
	endpoint, err := r.target(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint.String()+"/ctrl-int/1/"+command, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Active-Remote", r.remote.token)
	resp, err := dacpClient.Do(req)
	if err != nil {
		r.forget()
		return errors.New("sender's remote control is unreachable")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		r.forget()
		return fmt.Errorf("sender refused the command: HTTP %d", resp.StatusCode)
	}
	return nil
}
