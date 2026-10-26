// Command players keeps the playback state of every output: devices running
// slopify-player and browser tabs. Any UI can control any output, and every
// output and UI sees every change as it happens. Everything is HTTP: outputs
// receive their state over Server-Sent Events and report back with POSTs.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/auth"
	"github.com/codemowers/slopify/internal/events"
	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/profiles"
	"github.com/codemowers/slopify/internal/signedurl"
)

var browserID = regexp.MustCompile(`^web-[a-z0-9]{8,40}$`)

const keepalive = 20 * time.Second

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()

	s := &store{
		players: map[string]*Player{},
		hub:     events.NewHub[message](),
		file:    httpx.EnvOr("STATE_FILE", "/data/players.json"),
		dirty:   make(chan struct{}, 1),
		signer:  signedurl.New(httpx.Env("STREAM_SIGNING_KEY")),
	}
	if err := s.load(); err != nil {
		slog.Error("loading players", "err", err)
		os.Exit(1)
	}
	go s.persist()
	go s.runIdleSinks(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})

	web := http.NewServeMux()
	// Browser APIs verify their own access tokens below.
	web.HandleFunc("GET /api/players", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, s.snapshotFor(auth.Subject(r)))
	})
	web.HandleFunc("GET /api/players/events", s.browserEvents)
	web.HandleFunc("GET /api/players/me/playlist.m3u", s.playlist)
	web.HandleFunc("GET /api/players/me/playlist.pls", s.playlist)
	web.HandleFunc("POST /api/players/{id}/command", func(w http.ResponseWriter, r *http.Request) {
		var c command
		if !decode(w, r, &c) {
			return
		}
		respond(w, s.webCommand(auth.Subject(r), r.PathValue("id"), c))
	})
	web.HandleFunc("POST /api/players/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		var rep report
		if !decode(w, r, &rep) {
			return
		}
		if !browserID.MatchString(r.PathValue("id")) {
			http.Error(w, "devices report through /device", http.StatusForbidden)
			return
		}
		respond(w, s.browserReport(auth.Subject(r), r.PathValue("id"), rep))
	})

	authorizer, err := auth.New(ctx)
	if err != nil {
		slog.Error("OIDC discovery", "err", err)
		os.Exit(1)
	}
	// Issuing media capabilities requires stream permission as well as the
	// outer players:write check. A control-only token must not mint streams.
	web.Handle("POST /api/players/links", authorizer.Require(auth.Scope("slopify:stream"), http.HandlerFunc(s.createLink)))
	web.Handle("POST /api/players/{id}/link", authorizer.Require(auth.Scope("slopify:stream"), http.HandlerFunc(s.rotateLink)))
	mux.Handle("/api/", authorizer.Require(func(r *http.Request) string {
		if r.URL.Path == "/api/stream" || (r.URL.Path == "/api/players/me/playlist.m3u" || r.URL.Path == "/api/players/me/playlist.pls") {
			return "slopify:stream"
		}
		if r.Method == http.MethodGet {
			return "slopify:players:read"
		}
		return "slopify:players:write"
	}, web))
	web.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		p, err := media.CleanPath(r.URL.Query().Get("path"))
		if err != nil || p == "." {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		format := profiles.Normalize(r.URL.Query().Get("format"))
		if format != "original" {
			if _, ok := profiles.Lookup(format); !ok {
				http.Error(w, "unsupported profile", http.StatusBadRequest)
				return
			}
		}
		q := s.signer.Query(p, format, 6*time.Hour)
		if r.URL.Query().Get("videoCodec") == "hevc" {
			q.Set("videoCodec", "hevc")
		}
		if start := r.URL.Query().Get("start"); start != "" {
			q.Set("start", start)
		}
		httpx.JSON(w, map[string]string{"url": profiles.Path(format) + "?" + q.Encode()})
	})

	s.registerLinkRoutes(mux)

	// For devices, authenticated by the token each device generated.
	mux.HandleFunc("GET /device/v1/events", s.deviceEvents)
	mux.HandleFunc("POST /device/v1/report", func(w http.ResponseWriter, r *http.Request) {
		id, _, ok := device(w, r)
		var rep report
		if !ok || !decode(w, r, &rep) {
			return
		}
		respond(w, s.report(id, rep))
	})
	mux.HandleFunc("GET /device/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		id, _, ok := device(w, r)
		if !ok {
			return
		}
		entry, err := strconv.ParseUint(r.URL.Query().Get("entry"), 10, 64)
		if err != nil {
			http.Error(w, "invalid entry", http.StatusBadRequest)
			return
		}
		u, err := s.streamURL(id, entry)
		if err != nil {
			respond(w, err)
			return
		}
		httpx.JSON(w, map[string]string{"url": u})
	})

	if err := httpx.Serve(ctx, mux); err != nil {
		slog.Error("serving", "err", err)
		os.Exit(1)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errNotAdopted):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		panic(err)
	}
}

var errForbidden = errors.New("token does not match this device")

// device identifies a device by the bearer token it generated on first
// start: the output ID derives from the token's hash, so only the holder of
// the token can act as that output.
func device(w http.ResponseWriter, r *http.Request) (id, hash string, ok bool) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || len(token) < 32 || len(token) > 256 {
		http.Error(w, "a device token of at least 32 characters is required", http.StatusUnauthorized)
		return "", "", false
	}
	sum := sha256.Sum256([]byte(token))
	hash = hex.EncodeToString(sum[:])
	return "dev-" + hash[:16], hash, true
}

// browserEvents streams a snapshot of all outputs and then every change. A
// tab passing its own browser output ID keeps that output online meanwhile.
func (s *store) browserEvents(w http.ResponseWriter, r *http.Request) {
	updates, cancel := s.hub.Subscribe()
	defer cancel()
	if id := r.URL.Query().Get("player"); id != "" {
		name := r.URL.Query().Get("name")
		if !browserID.MatchString(id) || name == "" || len(name) > 64 {
			http.Error(w, "invalid browser output", http.StatusBadRequest)
			return
		}
		detach, err := s.attach(id,
			func() *Player { return &Player{Kind: Browser, Name: name, Adopted: true} },
			func(p *Player) error {
				if p.Kind != Browser || (p.Owner != "" && p.Owner != queueID(auth.Subject(r))) {
					return errBadRequest
				}
				p.Owner = queueID(auth.Subject(r))
				if !p.NameSet {
					p.Name = name
				}
				return nil
			})
		if err != nil {
			respond(w, err)
			return
		}
		defer detach()
		s.attachUser(auth.Subject(r), id)
	}
	send := events.Stream(w)
	if !send("snapshot", s.snapshotFor(auth.Subject(r))) {
		return
	}
	ping := time.NewTicker(keepalive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if !send("ping", struct{}{}) {
				return
			}
		case m, ok := <-updates:
			if !ok {
				return
			}
			if m.Type == "player" {
				var p Player
				if json.Unmarshal(m.Player, &p) != nil || !s.visiblePlayer(auth.Subject(r), &p) {
					continue
				}
				if p.Kind == Device {
					p.Queue = nil
					p.CurrentEntry = nil
					m.Player, _ = json.Marshal(&p)
				}
			} else if strings.HasPrefix(m.ID, "user-") && m.ID != queueID(auth.Subject(r)) {
				continue
			}
			if !send(m.Type, m) {
				return
			}
		}
	}
}

// deviceEvents streams a device's own output state: once on connect and
// then on every change. New devices wait for adoption in the web app.
func (s *store) deviceEvents(w http.ResponseWriter, r *http.Request) {
	id, hash, ok := device(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	name, model := q.Get("name"), q.Get("model")
	if name == "" || len(name) > 64 || len(model) > 128 {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	var formats []string
	if f := q.Get("formats"); f != "" {
		formats = strings.Split(f, ",")
	}
	address := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		address, _, _ = strings.Cut(fwd, ",")
	}

	ctx, disconnect := context.WithCancel(r.Context())
	defer disconnect()
	updates, cancel := s.hub.Subscribe()
	defer cancel()
	detach, err := s.attach(id,
		func() *Player { return &Player{Kind: Device, Name: name, TokenHash: hash} },
		func(p *Player) error {
			if p.Kind != Device || subtle.ConstantTimeCompare([]byte(p.TokenHash), []byte(hash)) != 1 {
				return errForbidden
			}
			if p.disconnect != nil {
				p.disconnect() // a newer connection replaces an older one
			}
			if !p.NameSet {
				p.Name = name
			}
			p.Model, p.Formats, p.Address, p.disconnect = model, formats, address, disconnect
			return nil
		})
	if err != nil {
		respond(w, err)
		return
	}
	defer detach()

	send := events.Stream(w)
	if b := s.player(id); b == nil || !send("player", json.RawMessage(b)) {
		return
	}
	ping := time.NewTicker(keepalive)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if !send("ping", struct{}{}) {
				return
			}
		case m, ok := <-updates:
			if !ok {
				return
			}
			if m.ID != id {
				continue
			}
			if m.Type == "removed" || !send("player", m.Player) {
				return
			}
		}
	}
}

func (s *store) player(id string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.players[id]; ok {
		return s.marshal(p)
	}
	return nil
}
