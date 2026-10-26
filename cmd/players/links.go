package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/auth"
	"github.com/codemowers/slopify/internal/events"
	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/profiles"
)

// Link capabilities authorize one pre-created output, never the owner's APIs.
func linkToken() (token, hash string, err error) {
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(secret[:])
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func (s *store) createLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 64 {
		http.Error(w, "name must be 1–64 bytes", http.StatusBadRequest)
		return
	}
	token, hash, err := linkToken()
	if err != nil {
		http.Error(w, "cannot create output link", 500)
		return
	}
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		http.Error(w, "cannot create output link", 500)
		return
	}
	id := "link-" + hex.EncodeToString(randomID[:])
	s.mu.Lock()
	if len(s.players) >= maxPlayers {
		s.mu.Unlock()
		http.Error(w, "too many outputs", http.StatusConflict)
		return
	}
	p := &Player{ID: id, Kind: WebSink, LinkHash: hash,
		Owner: queueID(auth.Subject(r)), Name: body.Name, NameSet: true,
		Adopted: true, State: Stopped, UpdatedAt: time.Now()}
	s.players[id] = p
	s.changed(p)
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	httpx.JSON(w, map[string]string{"id": id, "url": "/output/#" + id + "." + token})
}

func (s *store) rotateLink(w http.ResponseWriter, r *http.Request) {
	token, hash, err := linkToken()
	if err != nil {
		http.Error(w, "cannot create output link", 500)
		return
	}
	s.mu.Lock()
	p := s.players[r.PathValue("id")]
	if p == nil || p.Kind != WebSink || p.Owner != queueID(auth.Subject(r)) {
		s.mu.Unlock()
		http.Error(w, "no such output", http.StatusNotFound)
		return
	}
	p.LinkHash = hash
	if p.disconnect != nil {
		p.disconnect()
	}
	s.changed(p)
	id := p.ID
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, map[string]string{"id": id, "url": "/output/#" + id + "." + token})
}

func linkHash(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) != 43 {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Callers hold s.mu.
func (s *store) linked(id, hash string) *Player {
	p := s.players[id]
	if p == nil || p.Kind != WebSink || len(hash) != 64 ||
		subtle.ConstantTimeCompare([]byte(p.LinkHash), []byte(hash)) != 1 {
		return nil
	}
	return p
}

func (s *store) registerLinkRoutes(mux *http.ServeMux) {
	protected := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			ok := s.linked(r.PathValue("id"), linkHash(r)) != nil
			s.mu.Unlock()
			w.Header().Set("Cache-Control", "no-store")
			if !ok {
				http.Error(w, "invalid or revoked output link", http.StatusUnauthorized)
				return
			}
			handler(w, r)
		}
	}
	mux.HandleFunc("GET /output-api/{id}/events", protected(s.linkEvents))
	mux.HandleFunc("GET /output-api/{id}/stream", protected(s.linkStream))
	mux.HandleFunc("POST /output-api/{id}/report", protected(s.linkReport))
}

type linkReceiver struct {
	cancel  context.CancelFunc
	blocked bool
}

func receiverID(r *http.Request) string {
	id := r.Header.Get("X-Slopify-Receiver")
	if len(id) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	return id
}

type linkView struct {
	Receivers    int     `json:"receivers"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Position     float64 `json:"position"`
	Version      uint64  `json:"version"`
	Current      uint64  `json:"current"`
	CurrentEntry *Entry  `json:"currentEntry,omitempty"`
	Queue        []Entry `json:"queue"`
}

func (s *store) linkSnapshot(id, hash string) *linkView {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.linked(id, hash)
	if p == nil {
		return nil
	}
	var entry *Entry
	if e := p.entry(p.Current); e != nil {
		copy := *e
		entry = &copy
	}
	return &linkView{Receivers: len(p.receivers), Name: p.Name, State: p.State, Position: s.pos(p),
		Version: p.Version, Current: p.Current, CurrentEntry: entry,
		Queue: append([]Entry{}, p.Queue...)}
}

func (s *store) linkEvents(w http.ResponseWriter, r *http.Request) {
	id, hash := r.PathValue("id"), linkHash(r)
	ctx, disconnect := context.WithCancel(r.Context())
	defer disconnect()
	updates, cancel := s.hub.Subscribe()
	defer cancel()
	receiver := receiverID(r)
	if receiver == "" {
		http.Error(w, "invalid browser receiver", 400)
		return
	}
	session := &linkReceiver{cancel: disconnect}
	var output *Player
	detach, err := s.attach(id, func() *Player { return &Player{} }, func(p *Player) error {
		if s.linked(id, hash) == nil {
			return errForbidden
		}
		if p.receivers == nil {
			p.receivers = map[string]*linkReceiver{}
		}
		if previous := p.receivers[receiver]; previous != nil {
			previous.cancel()
		} else if len(p.receivers) >= 16 {
			return errBadRequest
		}
		p.receivers[receiver] = session
		if p.reportReceiver == "" {
			p.reportReceiver = receiver
		}
		p.disconnect = func() {
			for _, active := range p.receivers {
				active.cancel()
			}
		}
		output = p
		return nil
	})
	if err != nil {
		http.Error(w, "invalid output link or too many receivers", http.StatusConflict)
		return
	}
	defer func() {
		s.mu.Lock()
		if output.receivers[receiver] == session {
			delete(output.receivers, receiver)
			if output.reportReceiver == receiver {
				output.reportReceiver = ""
				for next, active := range output.receivers {
					if !active.blocked {
						output.reportReceiver = next
						break
					}
				}
			}
		}
		output.Blocked = len(output.receivers) > 0 && output.reportReceiver == ""
		if s.players[id] == output && len(output.receivers) > 0 {
			s.publish(output)
		}
		s.mu.Unlock()
		detach()
	}()
	send := events.Stream(w)
	sendState := func() bool {
		view := s.linkSnapshot(id, hash)
		return view != nil && send("player", view)
	}
	if !sendState() {
		return
	}
	ping := time.NewTicker(keepalive)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.Context().Err() == nil {
				send("closed", struct{}{})
			}
			return
		case <-ping.C:
			if !send("ping", struct{}{}) {
				return
			}
		case m, ok := <-updates:
			if !ok {
				return
			}
			if m.ID == id && (m.Type == "removed" || !sendState()) {
				send("closed", struct{}{})
				return
			}
		}
	}
}

func (s *store) linkStream(w http.ResponseWriter, r *http.Request) {
	entry, err := strconv.ParseUint(r.URL.Query().Get("entry"), 10, 64)
	if err != nil {
		http.Error(w, "invalid entry", 400)
		return
	}
	s.mu.Lock()
	p := s.linked(r.PathValue("id"), linkHash(r))
	if p == nil || p.Current != entry || p.entry(entry) == nil {
		s.mu.Unlock()
		http.Error(w, "entry is not currently assigned to this output", http.StatusForbidden)
		return
	}
	e := *p.entry(entry)
	s.mu.Unlock()
	format := profiles.Normalize(r.URL.Query().Get("format"))
	profile, known := profiles.Lookup(format)
	if format != "original" && (!known || profile.Video != (e.Kind == media.Video)) {
		http.Error(w, "invalid output profile", 400)
		return
	}
	q := s.signer.Query(e.Path, format, 5*time.Minute)
	if start := r.URL.Query().Get("start"); start != "" {
		position, err := strconv.ParseFloat(start, 64)
		if err != nil || position < 0 || math.IsNaN(position) || math.IsInf(position, 0) {
			http.Error(w, "invalid start", 400)
			return
		}
		q.Set("start", start)
	}
	if e.VideoCodec == "hevc" {
		q.Set("videoCodec", "hevc")
	}
	httpx.JSON(w, map[string]string{"url": profiles.Path(format) + "?" + q.Encode()})
}

// Only playback telemetry is accepted: client state cannot pause/start a queue
// and arbitrary position changes are rejected. Natural ends may advance it.
func (s *store) linkReport(w http.ResponseWriter, r *http.Request) {
	var rep report
	if !decode(w, r, &rep) {
		return
	}
	s.mu.Lock()
	p := s.linked(r.PathValue("id"), linkHash(r))
	if p == nil {
		s.mu.Unlock()
		http.Error(w, "invalid or revoked output link", 401)
		return
	}
	receiver := receiverID(r)
	session := p.receivers[receiver]
	if session == nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	e := p.entry(p.Current)
	if rep.Version != p.Version || rep.Entry != p.Current || e == nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if math.IsNaN(rep.Position) || math.IsInf(rep.Position, 0) || rep.Position < 0 ||
		math.Abs(rep.Position-s.pos(p)) > 15 {
		s.mu.Unlock()
		http.Error(w, "report is outside current playback", 400)
		return
	}
	session.blocked = rep.Blocked
	if primary := p.receivers[p.reportReceiver]; primary == nil || primary.blocked {
		p.reportReceiver = ""
		for candidate, active := range p.receivers {
			if !active.blocked {
				p.reportReceiver = candidate
				break
			}
		}
	}
	allBlocked := len(p.receivers) > 0 && p.reportReceiver == ""
	if p.Blocked != allBlocked {
		p.Blocked = allBlocked
		s.publish(p)
	}
	if p.reportReceiver != receiver {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	expected := s.pos(p)
	if rep.Ended && (p.State != Playing || (e.Duration > 0 && (rep.Position < e.Duration-2 || expected < e.Duration-2))) {
		rep.Ended = false
	}
	// The capability reports natural playback, but never controls the clock.
	rep.Position = expected
	rep.State = p.State
	rep.Blocked = false
	id := p.ID
	s.mu.Unlock()
	respond(w, s.report(id, rep))
}

// A web sink is a persistent destination, not the browser sessions tuned to it.
// With no receivers, known durations still advance its queue on the server.
func (s *store) runIdleSinks(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.advanceIdleSinks()
		}
	}
}

func (s *store) advanceIdleSinks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.players {
		if q.Kind != User || q.State != Playing {
			continue
		}
		p := s.players[q.OutputID]
		if p == nil || p.Kind != WebSink || (len(p.receivers) > 0 && !p.Blocked) {
			continue
		}
		entry := q.entry(q.Current)
		if entry == nil || entry.Duration <= 0 || s.pos(q) < entry.Duration {
			continue
		}
		if next := q.after(q.Current); next != 0 {
			s.start(q, next, 0)
		} else {
			q.setPos(0, Stopped)
		}
		q.Version++
		s.sendQueue(q)
	}
}
