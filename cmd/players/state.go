package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/codemowers/slopify/internal/events"
	"github.com/codemowers/slopify/internal/heuristics"
	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/signedurl"
)

const (
	Device  = "device"
	Browser = "browser"
	WebSink = "web-sink"
	User    = "user"

	Stopped = "stopped"
	Playing = "playing"
	Paused  = "paused"

	maxQueue              = 20000
	maxPlayers            = 100
	streamTTL             = 12 * time.Hour
	browserReconnectGrace = 30 * time.Second
)

// Entry is one queue item. IDs are unique within a player, so edits to the
// queue never confuse which track is playing.
type Entry struct {
	Bitrate    int64 `json:"bitrate,omitempty"`
	SampleRate int   `json:"sampleRate,omitempty"`
	Channels   int   `json:"channels,omitempty"`
	Bits       int   `json:"bits,omitempty"`

	Kind       media.Kind `json:"kind,omitempty"`
	Size       int64      `json:"size,omitempty"`
	ID         uint64     `json:"id"`
	Path       string     `json:"path"`
	Title      string     `json:"title"`
	Artist     string     `json:"artist,omitempty"`
	Album      string     `json:"album,omitempty"`
	Duration   float64    `json:"duration,omitempty"`
	Codec      string     `json:"codec,omitempty"`
	VideoCodec string     `json:"videoCodec,omitempty"`
}

// Player is the authoritative state of one output. Devices running
// slopify-player and browser tabs follow it and report back what they do.
type Player struct {
	// User queues are durable; outputs are replaceable playback targets.
	OutputID     string   `json:"outputId,omitempty"`
	OutputIDs    []string `json:"outputIds,omitempty"`
	CurrentEntry *Entry   `json:"currentEntry,omitempty"`
	Owner        string   `json:"owner,omitempty"`
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	NameSet      bool     `json:"nameSet,omitempty"`
	Adopted      bool     `json:"adopted"`
	Connected    bool     `json:"connected"`
	Available    bool     `json:"available"`
	Model        string   `json:"model,omitempty"`
	Address      string   `json:"address,omitempty"`
	// Formats a device decodes itself; everything else is sent as FLAC.
	Formats []string `json:"formats,omitempty"`
	// TokenHash pairs a device with the secret it generated; never published.
	TokenHash string `json:"tokenHash,omitempty"`
	LinkHash  string `json:"linkHash,omitempty"`

	Queue   []Entry `json:"queue"`
	Current uint64  `json:"current"`
	State   string  `json:"state"`
	// Position in seconds at UpdatedAt; clients extrapolate while playing.
	Position  float64   `json:"position"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Version counts commands; reports about older versions are stale.
	Version uint64 `json:"version"`
	// Blocked means a browser tab could not start audio without a user gesture.
	Blocked bool   `json:"blocked,omitempty"`
	NextID  uint64 `json:"nextId"`

	Receivers            int `json:"receivers,omitempty"`
	receivers            map[string]*linkReceiver
	reportReceiver       string
	links                int // open event streams of this output
	disconnectTimer      *time.Timer
	connectionGeneration uint64
	disconnect           func() // ends a device's event stream
}

type message struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Player json.RawMessage `json:"player,omitempty"`
}

type store struct {
	mu      sync.Mutex
	players map[string]*Player
	hub     *events.Hub[message]
	file    string
	dirty   chan struct{}
	signer  *signedurl.Signer
}

var (
	errNotFound   = errors.New("no such player")
	errNotAdopted = errors.New("player has not been adopted")
	errBadRequest = errors.New("bad request")
)

func (s *store) load() error {
	b, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Migrate the former browser/shared representation in saved state once.
	var records []struct {
		Player
		Shared bool `json:"shared"`
	}
	if err := json.Unmarshal(b, &records); err != nil {
		return err
	}
	for _, record := range records {
		p := &record.Player
		if record.Shared && p.Kind == Browser {
			p.Kind = WebSink
		}
		if p.Kind != Device && p.Kind != Browser && p.Kind != WebSink && p.Kind != User {
			continue // from an earlier version
		}
		if p.Kind == Browser && len(p.Queue) == 0 && p.CurrentEntry == nil {
			continue // tabs come and go; keep only those with a playlist
		}
		for i := range p.Queue {
			cleanEntryText(&p.Queue[i])
		}
		if p.CurrentEntry != nil {
			cleanEntryText(p.CurrentEntry)
		}
		p.Connected = false
		// A browser output plays only while its tab is open. A device that
		// was playing resumes when it reconnects after a server restart.
		if p.Kind == Browser && p.State == Playing {
			p.State = Paused
		}
		p.UpdatedAt = time.Now()
		s.players[p.ID] = p
	}
	for _, q := range s.players {
		if q.Kind != User {
			continue
		}
		// Empty browser outputs are intentionally not persisted. Remove their
		// stale selections before a new tab tries to join the user's queue.
		q.OutputIDs = slices.DeleteFunc(slices.Clone(queueOutputs(q)), func(id string) bool {
			p := s.players[id]
			return p == nil || p.Kind == User || ((p.Kind == Browser || p.Kind == WebSink) && p.Owner != "" && p.Owner != q.ID)
		})
		s.selectPrimary(q)
		if p := s.players[q.OutputID]; p != nil {
			s.copyPlayback(q, p)
		} else {
			s.pause(q)
		}
		q.Connected = s.outputsAvailable(q)
	}
	return nil
}

// persist writes adopted players to disk shortly after changes.
func (s *store) persist() {
	for range s.dirty {
		time.Sleep(time.Second)
		s.mu.Lock()
		var ps []*Player
		for _, p := range s.players {
			if p.Adopted {
				cp := *p
				cp.Position = s.pos(p)
				ps = append(ps, &cp)
			}
		}
		b, err := json.Marshal(ps)
		s.mu.Unlock()
		if err != nil {
			panic(err)
		}
		tmp := s.file + ".tmp"
		if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil {
			slog.Error("saving players", "err", err)
			continue
		}
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			slog.Error("saving players", "err", err)
			continue
		}
		if err := os.Rename(tmp, s.file); err != nil {
			slog.Error("saving players", "err", err)
		}
	}
}

// marshal encodes p with its position brought up to now, so clients can
// extrapolate from when they receive it regardless of clock skew.
func (s *store) marshal(p *Player) []byte {
	p.Position, p.UpdatedAt = s.pos(p), time.Now()
	public := *p
	// Device clients need the detached current entry while the user queue is empty.
	if p.Kind != User && p.CurrentEntry != nil && p.index(p.Current) < 0 {
		public.Queue = append([]Entry{*p.CurrentEntry}, p.Queue...)
	}
	public.TokenHash = ""
	public.LinkHash = ""
	public.Receivers = len(p.receivers)
	public.Available = p.available()
	b, err := json.Marshal(&public)
	if err != nil {
		panic(err)
	}
	return b
}

// changed publishes p and schedules a save; callers hold s.mu.
func (s *store) changed(p *Player) {
	s.publish(p)
	if p.Kind != User {
		if q := s.outputQueue(p.ID); q != nil {
			s.selectPrimary(q)
			if q.OutputID == p.ID {
				s.copyPlayback(q, p)
			}
			q.Connected = s.outputsAvailable(q)
			q.Version++
			s.changed(q)
		}
	}
}

func (s *store) publish(p *Player) {
	b := s.marshal(p)
	s.hub.Publish(message{Type: "player", ID: p.ID, Player: b})
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *store) removed(id string) {
	s.hub.Publish(message{Type: "removed", ID: id})
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// pos is the current position, extrapolated while playing.
func (s *store) pos(p *Player) float64 {
	if p.State != Playing {
		return p.Position
	}
	pos := p.Position + time.Since(p.UpdatedAt).Seconds()
	if e := p.entry(p.Current); e != nil && e.Duration > 0 {
		pos = math.Min(pos, e.Duration)
	}
	return pos
}

func (p *Player) available() bool { return p.Kind == WebSink || p.Connected }

func (p *Player) setPos(pos float64, state string) {
	p.Position, p.State, p.UpdatedAt = pos, state, time.Now()
}

func (p *Player) index(id uint64) int {
	return slices.IndexFunc(p.Queue, func(e Entry) bool { return e.ID == id })
}

func (p *Player) entry(id uint64) *Entry {
	if i := p.index(id); i >= 0 {
		return &p.Queue[i]
	}
	if p.CurrentEntry != nil && p.CurrentEntry.ID == id {
		return p.CurrentEntry
	}
	return nil
}

// after returns the entry following id, or 0 at the end of the queue.
func (p *Player) after(id uint64) uint64 {
	if p.index(id) < 0 && p.CurrentEntry != nil && p.CurrentEntry.ID == id && len(p.Queue) > 0 {
		return p.Queue[0].ID
	}
	if i := p.index(id); i >= 0 && i+1 < len(p.Queue) {
		return p.Queue[i+1].ID
	}
	return 0
}

func (p *Player) before(id uint64) uint64 {
	if i := p.index(id); i > 0 {
		return p.Queue[i-1].ID
	}
	return 0
}

// start plays entry id from offset seconds.
func (s *store) start(p *Player, id uint64, offset float64) {
	e := p.entry(id)
	if e == nil {
		s.stop(p)
		return
	}
	entry := *e
	p.CurrentEntry = &entry
	p.Current, p.Blocked = id, false
	p.setPos(offset, Playing)
}

func (s *store) stop(p *Player) {
	p.setPos(0, Stopped)
}

func (s *store) pause(p *Player) {
	if p.State != Playing {
		return
	}
	p.setPos(s.pos(p), Paused)
}

func (s *store) resume(p *Player) {
	switch {
	case p.Current != 0 && p.entry(p.Current) != nil:
		s.start(p, p.Current, p.Position)
	case len(p.Queue) > 0:
		s.start(p, p.Queue[0].ID, 0)
	}
}

func (s *store) add(p *Player, tracks []Entry, at int) error {
	if len(p.Queue)+len(tracks) > maxQueue {
		return fmt.Errorf("%w: queue limit of %d tracks", errBadRequest, maxQueue)
	}
	added := make([]Entry, 0, len(tracks))
	for _, t := range tracks {
		clean, err := media.CleanPath(t.Path)
		if err != nil || clean == "." {
			return fmt.Errorf("%w: invalid path %q", errBadRequest, t.Path)
		}
		kind, _, ok := media.Classify(clean)
		if !ok {
			return fmt.Errorf("%w: unsupported media path", errBadRequest)
		}
		p.NextID++
		t.ID, t.Path, t.Kind = p.NextID, clean, kind
		cleanEntryText(&t)
		added = append(added, t)
	}
	p.Queue = slices.Insert(p.Queue, at, added...)
	return nil
}

// command is a request to change a player, from any UI.
type command struct {
	Op       string   `json:"op"`
	ID       uint64   `json:"id"`
	IDs      []uint64 `json:"ids"`
	Before   uint64   `json:"before"`
	Position float64  `json:"position"`
	Tracks   []Entry  `json:"tracks"`
	Play     bool     `json:"play"`
	To       string   `json:"to"`
	Outputs  []string `json:"outputs"`
	Name     string   `json:"name"`
}

func (s *store) command(id string, c command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[id]
	if !ok {
		return errNotFound
	}
	switch c.Op {
	case "adopt":
		p.Adopted = true
	case "rename":
		if c.Name == "" || len(c.Name) > 64 {
			return errBadRequest
		}
		p.Name, p.NameSet = c.Name, true
	case "forget":
		if p.disconnect != nil {
			p.disconnect()
		}
		if q := s.outputQueue(id); q != nil {
			if q.OutputID == id {
				s.copyPlayback(q, p)
			}
			q.OutputIDs = slices.DeleteFunc(slices.Clone(queueOutputs(q)), func(output string) bool { return output == id })
			q.OutputID = ""
			s.selectPrimary(q)
			q.Connected = s.outputsAvailable(q)
			if len(q.OutputIDs) == 0 {
				s.pause(q)
			}
			s.changed(q)
		}
		delete(s.players, id)
		s.removed(id)
		return nil
	default:
		if !p.Adopted {
			return errNotAdopted
		}
		if err := s.playback(p, c); err != nil {
			return err
		}
	}
	p.Version++
	s.changed(p)
	return nil
}

func (s *store) playback(p *Player, c command) error {
	switch c.Op {
	case "play":
		switch {
		case c.ID != 0:
			s.start(p, c.ID, 0)
		case p.State == Paused || p.Current != 0:
			s.resume(p)
		case len(p.Queue) > 0:
			s.start(p, p.Queue[0].ID, 0)
		}
	case "pause":
		s.pause(p)
	case "toggle":
		if p.State == Playing {
			s.pause(p)
		} else {
			s.resume(p)
		}
	case "stop":
		s.stop(p)
	case "next":
		if n := p.after(p.Current); n != 0 {
			s.start(p, n, 0)
		}
	case "prev":
		if b := p.before(p.Current); b != 0 && s.pos(p) < 3 {
			s.start(p, b, 0)
		} else if p.Current != 0 {
			s.start(p, p.Current, 0)
		}
	case "seek":
		if p.Current == 0 || c.Position < 0 {
			return errBadRequest
		}
		p.setPos(c.Position, p.State)
	case "append", "insert":
		at := len(p.Queue)
		// Append is tail-only. Explicit drag/drop insertion is a separate action.
		if i := p.index(c.Before); c.Op == "insert" && c.Before != 0 && i >= 0 {
			at = i
		}
		if err := s.add(p, c.Tracks, at); err != nil {
			return err
		}
		if len(c.Tracks) > 0 && (c.Play || (p.State == Stopped && p.Current == 0)) {
			s.start(p, p.Queue[at].ID, 0)
		}
	case "next-up":
		at := p.index(p.Current) + 1
		if err := s.add(p, c.Tracks, at); err != nil {
			return err
		}
		if c.Play {
			s.start(p, p.Queue[at].ID, 0)
		}
	case "replace":
		s.stop(p)
		p.Queue, p.Current = nil, 0
		if err := s.add(p, c.Tracks, 0); err != nil {
			return err
		}
		if len(p.Queue) > 0 {
			s.start(p, p.Queue[0].ID, 0)
		}
	case "remove":
		drop := map[uint64]bool{}
		for _, id := range c.IDs {
			drop[id] = true
		}
		if drop[p.Current] {
			if e := p.entry(p.Current); e != nil {
				copy := *e
				p.CurrentEntry = &copy
			}
		}
		p.Queue = slices.DeleteFunc(p.Queue, func(e Entry) bool { return drop[e.ID] })
	case "move":
		moving := map[uint64]bool{}
		for _, id := range c.IDs {
			moving[id] = true
		}
		var picked []Entry
		rest := p.Queue[:0:0]
		for _, e := range p.Queue {
			if moving[e.ID] {
				picked = append(picked, e)
			} else {
				rest = append(rest, e)
			}
		}
		at := len(rest)
		if i := slices.IndexFunc(rest, func(e Entry) bool { return e.ID == c.Before }); c.Before != 0 && i >= 0 {
			at = i
		}
		p.Queue = slices.Insert(rest, at, picked...)
	case "clear":
		if e := p.entry(p.Current); e != nil {
			copy := *e
			p.CurrentEntry = &copy
		}
		p.Queue = nil
	case "transfer":
		t, ok := s.players[c.To]
		if !ok || t == p {
			return errNotFound
		}
		if !t.Adopted {
			return errNotAdopted
		}
		pos, cur := s.pos(p), p.index(p.Current)
		s.stop(t)
		t.Queue, t.Current = nil, 0
		if err := s.add(t, p.Queue, 0); err != nil {
			return err
		}
		if cur >= 0 {
			s.start(t, t.Queue[cur].ID, pos)
		}
		t.Version++
		s.changed(t)
		s.pause(p)
	default:
		return fmt.Errorf("%w: unknown op %q", errBadRequest, c.Op)
	}
	return nil
}

// report is what a browser tab or device says it is doing.
type report struct {
	Version  uint64  `json:"version"`
	Entry    uint64  `json:"entry"`
	State    string  `json:"state"`
	Position float64 `json:"position"`
	Ended    bool    `json:"ended"`
	Blocked  bool    `json:"blocked"`
}

func (s *store) report(id string, r report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[id]
	if !ok || p.Kind == Device && !p.Adopted {
		return errNotFound
	}
	if r.Entry != p.Current || r.Version != p.Version {
		return nil // about a track or command the output has not caught up with
	}
	q := s.outputQueue(id)
	// Only the primary output advances the shared queue or corrects its clock.
	if q != nil && q.OutputID != id && r.Ended {
		return nil
	}
	oldState, oldCurrent := p.State, p.Current
	switch {
	case r.Ended:
		if n := p.after(p.Current); n != 0 {
			s.start(p, n, 0)
		} else {
			p.setPos(0, Stopped)
		}
		p.Version++
	case r.Blocked:
		p.setPos(r.Position, Paused)
		p.Blocked = true
	case r.State == Playing || r.State == Paused:
		// Pausing from the lock screen or headphones changes the
		// authoritative state too; positions only correct drift.
		if r.State != p.State {
			p.Version++
		}
		p.setPos(r.Position, r.State)
	default:
		return errBadRequest
	}
	s.changed(p)
	if q != nil && q.OutputID == id && (oldState != p.State || oldCurrent != p.Current) {
		s.sendQueue(q)
	}
	return nil
}

// attach registers an open event stream of an output, creating the output
// with init the first time. The returned function detaches it.
func (s *store) attach(id string, init func() *Player, check func(*Player) error) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[id]
	if !ok {
		if len(s.players) >= maxPlayers {
			return nil, fmt.Errorf("%w: too many outputs", errBadRequest)
		}
		p = init()
		p.ID, p.State, p.UpdatedAt = id, Stopped, time.Now()
		s.players[id] = p
	}
	if err := check(p); err != nil {
		if !ok {
			delete(s.players, id)
		}
		return nil, err
	}
	if p.disconnectTimer != nil {
		p.disconnectTimer.Stop()
		p.disconnectTimer = nil
	}
	p.connectionGeneration++
	p.links++
	p.Connected = true
	// Selected outputs resume the authoritative queue, not their stale state
	// from before disconnection. In particular, don't rewind a live primary.
	if q := s.outputQueue(id); q != nil {
		s.copyPlayback(p, q)
		p.Version++
	}
	s.changed(p)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.players[id] != p {
			return
		}
		p.links--
		if p.links > 0 {
			return
		}
		p.Connected = false
		if !p.Adopted {
			delete(s.players, id)
			s.removed(id)
			return
		}
		if p.Kind == Browser && p.State == Playing {
			// Network changes can interrupt SSE while buffered audio continues.
			// Reconnecting the same tab cancels this timer without issuing pause.
			generation := p.connectionGeneration
			p.disconnectTimer = time.AfterFunc(browserReconnectGrace, func() { s.finishBrowserDisconnect(p, generation) })
		} else if p.Kind != WebSink && p.State == Playing {
			p.setPos(s.pos(p), Paused)
		}
		s.changed(p)
	}, nil
}

// A generation check prevents a fired old timer from pausing a newer stream.
func (s *store) finishBrowserDisconnect(p *Player, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.players[p.ID] != p || p.links > 0 || p.connectionGeneration != generation {
		return
	}
	p.disconnectTimer = nil
	if p.State == Playing {
		s.pause(p)
		s.changed(p)
	}
}

// streamURL returns a signed URL of the original file of a device's entry.
func (s *store) streamURL(id string, entry uint64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.players[id]
	if !ok || !p.Adopted {
		return "", errNotFound
	}
	e := p.entry(entry)
	if e == nil {
		return "", errNotFound
	}
	return "/stream/original?" + s.signer.Query(e.Path, "original", streamTTL).Encode(), nil
}

func cleanEntryText(entry *Entry) {
	kind, _, _ := media.Classify(entry.Path)
	technicalTitle := kind == media.Video && media.TechnicalVideoTitle(entry.Title)
	if !technicalTitle && !media.CorruptText(entry.Title) && !media.CorruptText(entry.Artist) && !media.CorruptText(entry.Album) {
		return
	}
	guess, _ := heuristics.Guess(entry.Path)
	if technicalTitle || media.CorruptText(entry.Title) {
		entry.Title = guess.Title
	}
	if media.CorruptText(entry.Artist) {
		entry.Artist = guess.Artist
	}
	if media.CorruptText(entry.Album) {
		entry.Album = guess.Album
	}
}
