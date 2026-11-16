package main

// Selected outputs are pins that lapse once the queue has been stopped for long
// enough, so that a later Play cannot start a speaker in a room where people are
// asleep. Each output keeps its own timeout; "always" pins never lapse. While the
// queue plays nothing lapses, and choosing outputs counts as activity.

import (
	"context"
	"slices"
	"time"
)

const (
	// defaultKeepMinutes applies to outputs without a setting of their own.
	defaultKeepMinutes = 30
	// maxKeepMinutes bounds a timeout; zero means always.
	maxKeepMinutes = 7 * 24 * 60
	pinSweepEvery  = 30 * time.Second
)

// keepMinutes is how long output id stays selected after playback stops; zero
// means indefinitely.
func (q *Player) keepMinutes(id string) int {
	if m, ok := q.OutputKeep[id]; ok {
		return m
	}
	return defaultKeepMinutes
}

// touch restarts the idle clock of a stopped queue after an explicit choice.
func (q *Player) touch() {
	if q.State != Playing {
		q.IdleSince = time.Now()
	}
}

// expirePins drops selected outputs whose timeout has passed while the queue was
// stopped. Callers hold s.mu and publish the queue if it reports a change.
func (s *store) expirePins(q *Player, now time.Time) bool {
	if q.Kind != User || q.State == Playing || q.IdleSince.IsZero() {
		return false
	}
	idle := now.Sub(q.IdleSince)
	ids := queueOutputs(q)
	kept := slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
		m := q.keepMinutes(id)
		return m > 0 && idle >= time.Duration(m)*time.Minute
	})
	if len(kept) == len(ids) {
		return false
	}
	q.OutputIDs = kept
	s.selectPrimary(q)
	for _, id := range ids {
		if slices.Contains(kept, id) {
			continue
		}
		if p := s.players[id]; p != nil {
			s.pause(p)
			p.Version++
			s.publish(p)
		}
	}
	return true
}

// sweepPins lets the output list show lapsed pins without waiting for a Play.
func (s *store) sweepPins(ctx context.Context) {
	tick := time.NewTicker(pinSweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			for _, q := range s.players {
				if s.expirePins(q, now) {
					q.Version++
					s.sendQueue(q)
				}
			}
			s.mu.Unlock()
		}
	}
}

// setKeep stores how long an owned output stays selected after playback stops.
func (s *store) setKeep(q *Player, id string, minutes int) error {
	p := s.players[id]
	if p == nil || p.Kind == User || p.Owner != q.ID || minutes < 0 || minutes > maxKeepMinutes {
		return errBadRequest
	}
	if q.OutputKeep == nil {
		q.OutputKeep = map[string]int{}
	}
	// Forget settings of outputs that no longer exist, keeping the map bounded.
	for known := range q.OutputKeep {
		if s.players[known] == nil {
			delete(q.OutputKeep, known)
		}
	}
	if minutes == defaultKeepMinutes {
		delete(q.OutputKeep, id)
	} else {
		q.OutputKeep[id] = minutes
	}
	q.touch()
	return nil
}

// startsPlayback reports whether a command can start audio on the queue's outputs.
func startsPlayback(q *Player, c command) bool {
	switch c.Op {
	case "play", "replace", "next", "prev":
		return true
	case "toggle":
		return q.State != Playing
	case "append", "insert", "next-up":
		return c.Play || (c.Op == "append" && q.State == Stopped && q.Current == 0)
	}
	return false
}

// claimForBrowser lets the browser that pressed Play take over a queue whose pins
// have all lapsed, instead of starting playback with no output at all.
func (s *store) claimForBrowser(q *Player, from string) {
	if from == "" || len(queueOutputs(q)) > 0 {
		return
	}
	p := s.players[from]
	if p == nil || p.Kind != Browser || p.Owner != q.ID || !p.available() {
		return
	}
	q.OutputIDs = []string{from}
	s.selectPrimary(q)
}
