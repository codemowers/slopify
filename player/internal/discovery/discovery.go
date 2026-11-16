// Package discovery advertises and browses trusted-LAN player web interfaces
// using typed Avahi D-Bus calls. No subprocesses or media secrets are involved.
package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/codemowers/slopify/player/internal/cleantext"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

const service = "_slopify._tcp"
const avahi = "org.freedesktop.Avahi"
const maxRecords = 64

var errCollision = errors.New("service name collision")

func isCollision(err error) bool {
	return strings.Contains(err.Error(), "collision") || strings.Contains(err.Error(), "Collision")
}

type Peer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Endpoint uses Avahi's numeric address; automatic peer requests never dial
	// arbitrary hostnames or follow redirects.
	Endpoint string `json:"-"`
}
type Browser struct {
	mu      sync.Mutex
	id      string
	records map[string]Peer
}
type serviceKey struct {
	Interface, Protocol int32
	Name, Type, Domain  string
}

func (k serviceKey) key() string { return fmt.Sprintf("%d/%d/%s", k.Interface, k.Protocol, k.Name) }

type resolved struct {
	Key           serviceKey
	Host, Address string
	Port          uint16
	TXT           [][]byte
	Version       uint64
	Err           error
}
type resolveJob struct {
	Key     serviceKey
	Version uint64
}

// A bounded signal handler restarts discovery on overflow rather than losing
// removal events or allocating unbounded deferred-delivery goroutines.
type signals struct {
	events   chan *dbus.Signal
	overflow chan struct{}
}

func (s *signals) DeliverSignal(_, _ string, event *dbus.Signal) {
	select {
	case s.events <- event:
	default:
		select {
		case s.overflow <- struct{}{}:
		default:
		}
	}
}

func Start(ctx context.Context, name string, port int) *Browser {
	var id [16]byte
	rand.Read(id[:])
	b := &Browser{id: hex.EncodeToString(id[:]), records: map[string]Peer{}}
	name = cleanName(name)
	if name == "" {
		name = "Slopify"
	}
	name = label(name, 63)
	go func() {
		failed := false
		published := name
		collision := 1
		for ctx.Err() == nil {
			started := time.Now()
			err := b.session(ctx, published, port)
			b.clear()
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errCollision) {
				collision++
				published = fmt.Sprintf("%s (%d)", label(name, 48), collision)
			}
			if !failed || time.Since(started) > 30*time.Second {
				slog.Warn("player discovery reconnecting", "err", err)
				failed = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return b
}
func label(s string, maxBytes int) string {
	for len(s) > maxBytes {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}
func call(ctx context.Context, obj dbus.BusObject, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return obj.CallWithContext(ctx, method, 0, args...)
}
func (b *Browser) session(parent context.Context, name string, port int) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	sink := &signals{events: make(chan *dbus.Signal, 256), overflow: make(chan struct{}, 1)}
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx), dbus.WithSignalHandler(sink))
	if err != nil {
		return err
	}
	defer conn.Close()
	server := conn.Object(avahi, dbus.ObjectPath("/"))
	var state int32
	if err = call(ctx, server, avahi+".Server.GetState").Store(&state); err != nil {
		return err
	}
	if state != 2 {
		return errors.New("Avahi is not running")
	}
	var owner string
	if err = call(ctx, conn.BusObject(), "org.freedesktop.DBus.GetNameOwner", avahi).Store(&owner); err != nil {
		return err
	}
	for _, iface := range []string{avahi + ".ServiceBrowser", avahi + ".EntryGroup", avahi + ".Server"} {
		if err = conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(owner), dbus.WithMatchInterface(iface)); err != nil {
			return err
		}
	}
	if err = conn.AddMatchSignalContext(ctx, dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, avahi)); err != nil {
		return err
	}
	var groupPath, browserPath dbus.ObjectPath
	if err = call(ctx, server, avahi+".Server.EntryGroupNew").Store(&groupPath); err != nil {
		return err
	}
	group := conn.Object(avahi, groupPath)
	txt := [][]byte{[]byte("id=" + b.id), []byte("version=1")}
	err = call(ctx, group, avahi+".EntryGroup.AddService", int32(-1), int32(-1), uint32(0), name, service, "local", "", uint16(port), txt).Err
	if err != nil {
		if isCollision(err) {
			return errCollision
		}
		return err
	}
	if err = call(ctx, group, avahi+".EntryGroup.Commit").Err; err != nil {
		return err
	}
	if err = call(ctx, server, avahi+".Server.ServiceBrowserNew", int32(-1), int32(-1), service, "local", uint32(0)).Store(&browserPath); err != nil {
		return err
	}
	jobs := make(chan resolveJob, maxRecords)
	results := make(chan resolved, maxRecords)
	for range 2 {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-jobs:
					result := resolved{Key: job.Key, Version: job.Version}
					var protocol int32
					var flags uint32
					var key serviceKey
					result.Err = call(ctx, server, avahi+".Server.ResolveService", job.Key.Interface, job.Key.Protocol, job.Key.Name, service, "local", job.Key.Protocol, uint32(0)).Store(&key.Interface, &key.Protocol, &key.Name, &key.Type, &key.Domain, &result.Host, &protocol, &result.Address, &result.Port, &result.TXT, &flags)
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	known := map[serviceKey]uint64{}
	var version uint64
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return errors.New("D-Bus disconnected")
		case <-sink.overflow:
			return errors.New("discovery signal queue overflow")
		case <-ticker.C:
			if err = call(ctx, server, avahi+".Server.GetState").Store(&state); err != nil {
				return err
			}
			if state != 2 {
				return errors.New("Avahi is not running")
			}
		case result := <-results:
			if result.Err == nil && known[result.Key] == result.Version {
				b.record(result)
			}
		case signal := <-sink.events:
			if signal.Sender == "org.freedesktop.DBus" && signal.Name == "org.freedesktop.DBus.NameOwnerChanged" {
				return errors.New("Avahi owner changed")
			}
			if signal.Sender != owner {
				continue
			}
			if signal.Path == groupPath && signal.Name == avahi+".EntryGroup.StateChanged" {
				var value int32
				var message string
				if dbus.Store(signal.Body, &value, &message) == nil {
					if value == 3 {
						return errCollision
					}
					if value == 4 {
						return errors.New("Avahi publication failed")
					}
				}
			}
			if signal.Path != browserPath {
				continue
			}
			if signal.Name == avahi+".ServiceBrowser.Failure" {
				return errors.New("Avahi browsing failed")
			}
			var key serviceKey
			var flags uint32
			if dbus.Store(signal.Body, &key.Interface, &key.Protocol, &key.Name, &key.Type, &key.Domain, &flags) != nil || key.Type != service || key.Domain != "local" || len(key.Name) > 255 {
				continue
			}
			switch signal.Name {
			case avahi + ".ServiceBrowser.ItemRemove":
				delete(known, key)
				b.mu.Lock()
				delete(b.records, key.key())
				b.mu.Unlock()
			case avahi + ".ServiceBrowser.ItemNew":
				if _, exists := known[key]; exists || len(known) >= maxRecords {
					continue
				}
				version++
				known[key] = version
				select {
				case jobs <- resolveJob{key, version}:
				default:
					return errors.New("discovery resolve queue overflow")
				}
			}
		}
	}
}
func (b *Browser) record(r resolved) {
	if r.Port == 0 || len(r.TXT) > 128 {
		return
	}
	for _, entry := range r.TXT {
		if string(entry) == "id="+b.id {
			return
		}
	}
	name := cleanName(r.Key.Name)
	if name == "" {
		return
	}
	host := strings.ToLower(strings.TrimSuffix(r.Host, "."))
	if !localHost(host) {
		ip, err := netip.ParseAddr(r.Address)
		if err != nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) || ip.Zone() != "" || ip.Is6() && ip.IsLinkLocalUnicast() {
			return
		}
		host = ip.String()
	}
	authority := host
	if r.Port != 80 {
		authority = net.JoinHostPort(host, strconv.Itoa(int(r.Port)))
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := r.Key.key()
	if _, exists := b.records[key]; !exists && len(b.records) >= maxRecords {
		return
	}
	endpoint := ""
	if ip, err := netip.ParseAddr(r.Address); err == nil && ip.IsPrivate() && ip.Zone() == "" {
		endpoint = "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(r.Port)))
	}
	b.records[key] = Peer{Name: name, URL: (&url.URL{Scheme: "http", Host: authority, Path: "/"}).String(), Endpoint: endpoint}
}
func (b *Browser) clear() { b.mu.Lock(); clear(b.records); b.mu.Unlock() }
func (b *Browser) Snapshot() []Peer {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]Peer{}
	peers := make([]Peer, 0, len(b.records))
	for _, p := range b.records {
		if previous, ok := seen[p.URL]; !ok || p.Name < previous.Name || p.Name == previous.Name && preferEndpoint(p.Endpoint, previous.Endpoint) {
			seen[p.URL] = p
		}
	}
	for _, p := range seen {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name == peers[j].Name {
			return peers[i].URL < peers[j].URL
		}
		return peers[i].Name < peers[j].Name
	})
	return peers
}
func preferEndpoint(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil {
		return false
	}
	if eb != nil {
		return true
	}
	ipa, _ := netip.ParseAddr(ua.Hostname())
	ipb, _ := netip.ParseAddr(ub.Hostname())
	if ipa.Is4() != ipb.Is4() {
		return ipa.Is4()
	} // RTP uses IPv4.
	return a < b
}
func localHost(host string) bool {
	if !strings.HasSuffix(host, ".local") || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func cleanName(name string) string { return cleantext.Line(name, 96) }
