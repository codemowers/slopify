package main

import "github.com/codemowers/slopify/internal/volume"

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
	Op      string `json:"op"`
	Version uint64 `json:"version"`
	Entry   uint64 `json:"entry"`
}

// Local controls affect only the queue selecting this device.
// The device capability cannot edit queues, select outputs, or target another
// account. Stale presses cannot reverse a newer web command.
func (s *store) deviceControl(id string, c deviceControl) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.players[id]
	if p == nil || p.Kind != Device || !p.Adopted || !p.Connected {
		return errForbidden
	}
	q := s.outputQueue(id)
	if q == nil || (c.Op != "toggle" && c.Op != "next" && c.Op != "prev" && c.Op != "pause-if-sole-output" && c.Op != "play-if-sole-output") {
		return errBadRequest
	}
	if c.Version != p.Version || c.Entry != p.Current {
		return nil
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
	if err := s.playback(q, command{Op: op}); err != nil {
		return err
	}
	q.Version++
	s.sendQueue(q)
	return nil
}
