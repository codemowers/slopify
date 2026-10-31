package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/codemowers/slopify/internal/auth"
	"slices"
	"time"
)

func queueID(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "user-" + hex.EncodeToString(sum[:])
}

func (s *store) userQueue(subject string) *Player {
	id := queueID(subject)
	if p := s.players[id]; p != nil {
		return p
	}
	p := &Player{ID: id, Kind: User, Name: "Your queue", Adopted: true, State: Stopped, UpdatedAt: time.Now()}
	s.players[id] = p
	s.changed(p)
	return p
}

// copyPlayback keeps the canonical user queue and its current output in sync.
// Output versions stay independent, so delayed device reports remain stale.
func (s *store) copyPlayback(to, from *Player) {
	to.Queue = slices.Clone(from.Queue)
	to.CurrentEntry = nil
	if e := from.entry(from.Current); e != nil {
		copy := *e
		to.CurrentEntry = &copy
	}
	to.Current, to.NextID = from.Current, from.NextID
	to.Blocked, to.Repeat = from.Blocked, from.Repeat
	to.setPos(s.pos(from), from.State)
}

func (s *store) outputQueue(id string) *Player {
	for _, p := range s.players {
		if p.Kind == User && slices.Contains(queueOutputs(p), id) {
			return p
		}
	}
	return nil
}

func queueOutputs(q *Player) []string {
	if q.OutputIDs != nil {
		return q.OutputIDs
	}
	if q.OutputID != "" {
		return []string{q.OutputID}
	}
	return nil
}

func (s *store) outputsAvailable(q *Player) bool {
	for _, id := range queueOutputs(q) {
		if p := s.players[id]; p != nil && p.available() {
			return true
		}
	}
	return false
}

func (s *store) selectPrimary(q *Player) {
	ids := queueOutputs(q)
	if slices.Contains(ids, q.OutputID) {
		if p := s.players[q.OutputID]; p != nil && p.Connected {
			return
		}
	}
	for _, id := range ids {
		if p := s.players[id]; p != nil && p.Connected {
			q.OutputID = id
			return
		}
	}
	// A persistent web sink is available even with no receiver sessions.
	for _, id := range ids {
		if p := s.players[id]; p != nil && p.Kind == WebSink {
			q.OutputID = id
			return
		}
	}
	if len(ids) > 0 {
		q.OutputID = ids[0]
	} else {
		q.OutputID = ""
	}
}

func (s *store) sendQueue(q *Player) {
	s.selectPrimary(q)
	q.Connected = s.outputsAvailable(q)
	if !q.Connected {
		s.pause(q)
	}
	for _, id := range queueOutputs(q) {
		if p := s.players[id]; p != nil {
			s.copyPlayback(p, q)
			p.Version++
			// Publish without feeding this output's snapshot back into q.
			s.publish(p)
		}
	}
	s.changed(q)
}

func (s *store) setOutputs(q *Player, ids []string) error {
	if len(ids) > maxPlayers {
		return errBadRequest
	}
	next := []string{}
	for _, id := range ids {
		if slices.Contains(next, id) {
			continue
		}
		p := s.players[id]
		if p == nil && slices.Contains(queueOutputs(q), id) {
			// A previously selected empty browser can disappear on restart.
			// It must not prevent another output from joining this queue.
			continue
		}
		if p == nil || p.Kind == User {
			return fmt.Errorf("%w: output %q is no longer registered", errNotFound, id)
		}
		if !p.Adopted {
			return errNotAdopted
		}
		if !p.available() && !slices.Contains(queueOutputs(q), id) {
			return fmt.Errorf("%w: output is offline", errBadRequest)
		}
		if (p.Kind == Browser || p.Kind == WebSink || p.Kind == Device) && p.Owner != q.ID {
			return errForbidden
		}
		if other := s.outputQueue(id); other != nil && other != q {
			return fmt.Errorf("%w: output is in use by another user", errBadRequest)
		}
		next = append(next, id)
	}
	old := slices.Clone(queueOutputs(q))
	q.OutputIDs = next
	s.selectPrimary(q)
	for _, id := range old {
		if !slices.Contains(next, id) {
			if p := s.players[id]; p != nil {
				s.pause(p)
				p.Version++
				s.publish(p)
			}
		}
	}
	return nil
}

func (s *store) userCommand(subject string, c command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.userQueue(subject)
	switch c.Op {
	case "transfer":
		if err := s.setOutputs(q, []string{c.To}); err != nil {
			return err
		}
	case "outputs":
		if err := s.setOutputs(q, c.Outputs); err != nil {
			return err
		}
	default:
		if c.Op == "adopt" || c.Op == "forget" || c.Op == "rename" {
			return errBadRequest
		}
		if err := s.playback(q, c); err != nil {
			return err
		}
	}
	q.Version++
	s.sendQueue(q)
	return nil
}

// attachUser selects a browser only for a new queue, never when another tab
// or phone opens. Existing account state always wins over tab-local state.
func (s *store) attachUser(subject, browser string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.players[queueID(subject)]
	q := s.userQueue(subject)
	if !exists || q.OutputID == "" {
		if p := s.players[browser]; p != nil {
			// Retain this browser's pre-upgrade playlist on first use only.
			if !exists {
				s.copyPlayback(q, p)
			}
			q.OutputID = browser
			q.OutputIDs = []string{browser}
			s.sendQueue(q)
		}
	}
}

func (s *store) webCommand(subject, id string, c command) error {
	return s.webCommandFor(auth.Principal{Subject: subject, Name: subject}, id, c)
}
func (s *store) webCommandFor(user auth.Principal, id string, c command) error {
	if user.Subject == "" {
		return errForbidden
	}
	if id == "me" {
		return s.userCommand(user.Subject, c)
	}
	if c.Op != "adopt" && c.Op != "rename" && c.Op != "forget" && c.Op != "volume" {
		return errBadRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.players[id]
	owner := queueID(user.Subject)
	if p == nil || p.Kind == User || (p.OwnerIssuer != "" && p.OwnerIssuer != user.Issuer) {
		return errForbidden
	}
	if c.Op == "adopt" {
		if p.Kind != Device || !p.Connected {
			return errBadRequest
		}
		if p.Owner != "" && p.Owner != owner {
			return errForbidden
		}
		if p.Adopted && p.Owner == owner {
			return nil
		}
		p.Owner = owner
		p.OwnerIssuer = user.Issuer
		p.OwnerSubject = user.Subject
		p.OwnerName = ownerDisplayName(user.Name)
		p.Adopted = true
		refreshDeviceName(p)
		p.Version++
		s.changed(p)
		return nil
	}
	if p.Owner != owner {
		return errForbidden
	}
	if q := s.outputQueue(id); q != nil && q.ID != owner {
		return errForbidden
	}
	if c.Op == "volume" {
		q := s.outputQueue(id)
		if q == nil || q.ID != owner {
			return errForbidden
		}
		return s.setVolume(p, c.Level)
	}
	return s.commandLocked(id, c)
}

func (s *store) browserReport(subject, id string, r report) error {
	s.mu.Lock()
	p := s.players[id]
	allowed := p != nil && p.Kind == Browser && p.Owner == queueID(subject)
	s.mu.Unlock()
	if !allowed {
		return errForbidden
	}
	return s.report(id, r)
}

func (s *store) visiblePlayer(subject string, p *Player) bool {
	return (p.Kind != User || p.ID == queueID(subject)) &&
		((p.Kind != Browser && p.Kind != WebSink && p.Kind != Device) || p.Owner == queueID(subject) || (p.Kind == Device && p.Owner == "" && !p.Adopted))
}

func (s *store) snapshotFor(subject string) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []json.RawMessage{}
	for _, p := range s.players {
		if s.visiblePlayer(subject, p) {
			out = append(out, s.webMarshal(p))
		}
	}
	return out
}

func (s *store) webMarshal(p *Player) []byte {
	if p.Kind != Device {
		return s.marshal(p)
	}
	// A device advertises availability; its queue belongs to its user's record.
	cp := *p
	cp.Queue = nil
	cp.CurrentEntry = nil
	return s.marshal(&cp)
}
