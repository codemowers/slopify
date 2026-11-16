package main

import (
	"slices"
	"time"

	"github.com/codemowers/slopify/internal/volume"
)

// Volume has its own revision. It never changes playback versions, positions
// or primary-output selection, including when a physical knob reports a change.
func (s *store) setVolume(p *Player, level *int) error {
	if p.Kind != Device || !p.Adopted || !p.Connected || p.Volume == nil || level == nil || *level < p.Volume.Min || *level > p.Volume.Max {
		return errBadRequest
	}
	cp := *p.Volume
	cp.Target, cp.Error = *level, ""
	cp.Revision++
	p.Volume = &cp
	s.publish(p)
	return nil
}

func (s *store) volumeReport(id string, r volume.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.players[id]
	if p == nil || p.Kind != Device || p.Volume == nil {
		return errNotFound
	}
	v := p.Volume
	if r.Level < v.Min || r.Level > v.Max || len(r.Error) > 256 || !volume.ValidDB(r.DB) {
		return errBadRequest
	}
	if r.Revision != v.Revision {
		return nil
	}
	sameDB := v.DB == nil && r.DB == nil || v.DB != nil && r.DB != nil && *v.DB == *r.DB
	if v.Level == r.Level && v.Target == r.Level && v.Error == r.Error && sameDB && v.Muted == r.Muted {
		return nil
	}
	cp := *v
	cp.Level, cp.Target, cp.Error = r.Level, r.Level, r.Error
	cp.DB, cp.Muted = r.DB, r.Muted
	p.Volume = &cp
	s.publish(p)
	return nil
}

type deviceControl struct {
	Source  string `json:"source,omitempty"`
	Op      string `json:"op"`
	Version uint64 `json:"version"`
	Entry   uint64 `json:"entry"`
}

// Local controls affect the selected queue, or explicitly rejoin the owner’s
// queue after detachment. They cannot select other outputs or target another
// account. Stale presses cannot reverse a newer web command.
func (s *store) deviceControl(id string, c deviceControl) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.players[id]
	if p == nil || p.Kind != Device || !p.Adopted || !p.reachable() {
		return errForbidden
	}
	if c.Version != p.Version || c.Entry != p.Current {
		return nil
	}
	q := s.outputQueue(id)
	explicitPlay := c.Op == "toggle" || c.Op == "play-if-sole-output"
	if q != nil && explicitPlay && q.State != Playing && s.expirePins(q, time.Now()) {
		q = s.outputQueue(id) // the press itself may follow its own pin lapsing
	}
	if q == nil && explicitPlay {
		// Someone at the speaker pressed Play after its pin lapsed: it rejoins its
		// owner's queue and plays, joining playback elsewhere rather than pausing it.
		owner := s.players[p.Owner]
		if owner == nil || owner.Kind != User {
			return errBadRequest
		}
		if err := s.setOutputs(owner, append(slices.Clone(queueOutputs(owner)), id)); err != nil {
			return err
		}
		wasPlaying := owner.State == Playing
		if !wasPlaying {
			if err := s.playback(owner, command{Op: "play"}); err != nil {
				return err
			}
		}
		owner.Version++
		if wasPlaying {
			s.sendQueueContinuous(owner)
		} else {
			s.sendQueue(owner)
		}
		return nil
	}
	if q == nil || (c.Op != "toggle" && c.Op != "next" && c.Op != "prev" && c.Op != "pause-if-sole-output" && c.Op != "play-if-sole-output") {
		return errBadRequest
	}
	op := c.Op
	if op == "pause-if-sole-output" || op == "play-if-sole-output" {
		// Evaluate under the same lock as output selection. Never let a stale
		// device-side count pause another selected output's shared queue.
		if len(queueOutputs(q)) != 1 {
			return nil
		}
		if op == "pause-if-sole-output" {
			if q.State != Playing {
				return nil
			}
			op = "pause"
		} else {
			if q.State != Paused || q.Current == 0 {
				return nil
			}
			op = "play"
		}
	}
	previousState := q.State
	if err := s.playback(q, command{Op: op}); err != nil {
		return err
	}
	if previousState == Playing && q.State == Paused {
		source := "Physical play/pause button"
		if c.Op == "pause-if-sole-output" {
			source = "Source selection away from Slopify"
		} else if c.Source == "local-controls" {
			source = "Local player controls"
		}
		q.Pause = &PauseEvent{Name: p.Name, Source: source, At: time.Now()}
	}
	q.Version++
	s.sendQueue(q)
	return nil
}
