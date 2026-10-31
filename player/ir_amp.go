package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type irSender interface{ Send(command uint8) error }

// irAmpConfig describes an amplifier with a motorised volume knob and one-way IR.
type irAmpConfig struct {
	VolumeUp, VolumeDown, PowerOn, PowerOff uint8
	Sweep, IdleOff, Wake, Repeat            time.Duration
	InitialSteps, MaxSteps                  int
	StateFile                               string
}

type irAmpPhase string

const (
	// The knob's position and the amp's power are unknown, as after a first start.
	irAmpUnknown  irAmpPhase = "unknown"
	irAmpOff      irAmpPhase = "off"
	irAmpWaking   irAmpPhase = "waking"
	irAmpRaising  irAmpPhase = "raising"
	irAmpOn       irAmpPhase = "on"
	irAmpSweeping irAmpPhase = "sweeping"
)

// irAmp keeps a position it can only know by counting from the bottom of the knob.
// Going idle sweeps to zero and powers off, so that each session starts from a known
// place and a lost command leaves it quieter rather than louder.
type irAmp struct {
	config irAmpConfig
	tx     irSender
	active func() bool

	pending atomic.Int32 // Knob steps, so that a detent never waits for IR.

	mu       sync.Mutex
	phase    irAmpPhase
	target   int
	position int

	idleSince  time.Time
	until      time.Time
	sweepAfter bool // Waking only to reach the bottom of an unknown position.
	lastErr    string
}

type irAmpState struct {
	Steps  int  `json:"steps"`
	AtZero bool `json:"atZero"`
}

func newIRAmp(config irAmpConfig, tx irSender, active func() bool) *irAmp {
	a := &irAmp{config: config, tx: tx, active: active, phase: irAmpUnknown, target: min(config.InitialSteps, config.MaxSteps)}
	if b, err := os.ReadFile(config.StateFile); err == nil {
		var state irAmpState
		if json.Unmarshal(b, &state) == nil {
			a.target = max(0, min(state.Steps, config.MaxSteps))
			if state.AtZero {
				a.phase = irAmpOff
			}
		}
	}
	return a
}

func (a *irAmp) save(atZero bool) {
	if a.config.StateFile == "" {
		return
	}
	b, _ := json.Marshal(irAmpState{Steps: a.target, AtZero: atZero})
	tmp := a.config.StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err == nil {
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, a.config.StateFile)
		}
		if err != nil {
			slog.Warn("saving amp state", "err", err)
		}
	}
}

// adjust queues volume steps without waiting for any transmission.
func (a *irAmp) adjust(delta int) { a.pending.Add(int32(delta)) }

func (a *irAmp) status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{"phase": a.phase, "steps": a.target, "maxSteps": a.config.MaxSteps}
}

func (a *irAmp) run(ctx context.Context) {
	ticker := time.NewTicker(a.config.Repeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.step(now)
		}
	}
}

// send reports failure by falling back to an unknown position, because whatever was
// being counted is no longer true.
func (a *irAmp) send(command uint8) bool {
	if err := a.tx.Send(command); err != nil {
		if text := err.Error(); text != a.lastErr {
			a.lastErr = text
			slog.Warn("amp infrared", "err", err)
		}
		a.phase = irAmpUnknown
		return false
	}
	a.lastErr = ""
	return true
}

// step advances by at most one transmission, called once per repeat interval.
func (a *irAmp) step(now time.Time) {
	active := a.active()
	a.mu.Lock()
	defer a.mu.Unlock()
	delta := int(a.pending.Swap(0))
	if delta != 0 {
		switch a.phase {
		case irAmpOff, irAmpSweeping:
			// Nothing to hear: the saved level is what the next session restores.
		case irAmpUnknown:
			// No count to keep, so this is only a relative nudge.
			command, count := a.config.VolumeUp, delta
			if delta < 0 {
				command, count = a.config.VolumeDown, -delta
			}
			for range min(count, 5) {
				a.send(command)
			}
		}
		next := max(0, min(a.target+delta, a.config.MaxSteps))
		if next != a.target {
			a.target = next
			a.save(a.phase == irAmpOff)
		}
	}
	if active {
		a.idleSince = time.Time{}
	} else if a.idleSince.IsZero() {
		a.idleSince = now
	}
	idle := !active && now.Sub(a.idleSince) >= a.config.IdleOff

	switch a.phase {
	case irAmpUnknown:
		if idle && a.send(a.config.PowerOn) {
			a.phase, a.until, a.sweepAfter = irAmpWaking, now.Add(a.config.Wake), true
		}
	case irAmpOff:
		if active {
			a.save(false) // The count is about to leave zero.
			if a.send(a.config.PowerOn) {
				a.phase, a.until, a.sweepAfter = irAmpWaking, now.Add(a.config.Wake), false
			}
		}
	case irAmpWaking:
		if !now.Before(a.until) {
			if a.sweepAfter {
				a.phase, a.until = irAmpSweeping, now.Add(a.config.Sweep)
			} else {
				a.phase, a.position = irAmpRaising, 0
			}
		}
	case irAmpRaising, irAmpOn:
		switch {
		case idle:
			a.save(false)
			a.phase, a.until = irAmpSweeping, now.Add(a.config.Sweep)
		case a.position < a.target:
			a.phase = irAmpRaising
			if a.send(a.config.VolumeUp) {
				a.position++
			}
		case a.position > a.target:
			a.phase = irAmpRaising
			if a.send(a.config.VolumeDown) {
				a.position--
			}
		default:
			a.phase = irAmpOn
		}
	case irAmpSweeping:
		if now.Before(a.until) {
			a.send(a.config.VolumeDown)
			return
		}
		// A sweep always finishes, since anything less leaves the position unknown.
		a.position = 0
		if active {
			a.phase = irAmpRaising
			a.save(false)
		} else if a.send(a.config.PowerOff) {
			a.phase = irAmpOff
			a.save(true)
		}
	}
}

// audioActive reports whether the speaker is playing something audible.
func (c *controller) audioActive() bool {
	if s := c.sources; s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.source(s.Selected)
		return p != nil && p.Connected && !p.Paused && time.Since(p.LastAudible) < 2*time.Second
	}
	c.local.mu.Lock()
	defer c.local.mu.Unlock()
	return c.local.queuePlaying
}
