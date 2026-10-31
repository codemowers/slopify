package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/codemowers/slopify/internal/volume"
	"github.com/codemowers/slopify/player/internal/alsa"
)

type mixer struct {
	mu              sync.Mutex
	card            string
	control         volume.Control
	mapping         mixerMapping
	device          mixerDevice
	switchAvailable bool
	switchMuted     bool
	unmuteLevel     int
}

// mixerDevice is the card's control interface.
type mixerDevice interface {
	Volume() (mixerMapping, int, error)
	SetVolume(raw int) error
	Muted() (bool, error)
	SetMuted(bool) error
}

// alsaMixer talks to the kernel's control device directly; no alsa-utils.
type alsaMixer struct {
	mixer  *alsa.Mixer
	volume string
	sw     string
}

func (a *alsaMixer) Volume() (mixerMapping, int, error) {
	c, err := a.mixer.Integer(a.volume)
	if err != nil {
		return mixerMapping{}, 0, err
	}
	values, err := a.mixer.Values(a.volume)
	if err != nil {
		return mixerMapping{}, 0, err
	}
	if c.Max <= c.Min {
		return mixerMapping{}, 0, errors.New("invalid ALSA playback range")
	}
	m := mixerMapping{min: c.Min, max: c.Max}
	if c.Scale != nil {
		m.dbBase, m.dbStep = float64(c.Scale.Min)/100, float64(c.Scale.Step)/100
		m.hasDB, m.mute = m.dbStep > 0, c.Scale.Mute
	}
	level := c.Min
	for _, v := range values {
		if v < c.Min || v > c.Max {
			return m, 0, errors.New("invalid ALSA playback value")
		}
		level = max(level, v)
	}
	return m, level, nil
}

func (a *alsaMixer) SetVolume(raw int) error { return a.mixer.SetValue(a.volume, raw) }

func (a *alsaMixer) Muted() (bool, error) {
	values, err := a.mixer.Switches(a.sw)
	if err != nil {
		return false, err
	}
	for _, v := range values {
		if v != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (a *alsaMixer) SetMuted(muted bool) error { return a.mixer.SetSwitch(a.sw, !muted) }

func newMixer(ctx context.Context, card, name string, min, max, initial int) (*mixer, error) {
	if initial < min || initial > max || name == "" || len(name) > 24 {
		return nil, errors.New("initial volume must be within configured range; control name is required")
	}
	device, err := alsa.OpenMixer(card)
	if err != nil {
		return nil, err
	}
	return startMixer(ctx, &alsaMixer{mixer: device, volume: name + " Playback Volume", sw: name + " Playback Switch"}, card, name, min, max, initial)
}

func startMixer(ctx context.Context, device mixerDevice, card, name string, min, max, initial int) (*mixer, error) {
	mapping, raw, err := device.Volume()
	if err != nil {
		return nil, err
	}
	c, err := parseMixer(mapping, raw, name, min, max)
	if err != nil {
		return nil, err
	}
	m := &mixer{card: card, control: c, mapping: mapping, device: device}
	// A separate ALSA playback switch mutes the amplifier without changing gain.
	if muted, e := device.Muted(); e == nil {
		m.switchAvailable, m.switchMuted = true, muted
	}
	if err := m.write(ctx, initial); err != nil {
		return nil, err
	}
	_, err = m.read(ctx)
	return m, err
}

func (m *mixer) snapshot() volume.Control {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.control
}

// read also observes alsamixer or physical-knob changes. Opt-in bounds apply
// locally as well as to web commands, so another mixer cannot bypass the cap.
func (m *mixer) read(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapping, raw, err := m.device.Volume()
	if err != nil {
		return false, err
	}
	c, err := parseMixer(mapping, raw, m.control.Name, m.control.Min, m.control.Max)
	if err != nil {
		return false, err
	}
	m.mapping = mapping
	previous := m.control.Level
	clamped := c.Level < c.Min || c.Level > c.Max
	if clamped {
		level := min(max(c.Level, c.Min), c.Max)
		if err := m.write(ctx, level); err != nil {
			return false, err
		}
		c.Level = m.control.Level
	}
	previousDB, previousMuted := m.control.DB, m.control.Muted
	changed := previous != c.Level || m.control.Error != ""
	if m.switchAvailable {
		muted, e := m.device.Muted()
		if e != nil {
			return false, e
		}
		changed = changed || muted != m.switchMuted
		m.switchMuted = muted
	}
	m.control.Level, m.control.Error = c.Level, ""
	if !clamped { // Otherwise write has already recorded what was set.
		m.observe(raw)
	}
	sameDB := previousDB == nil && m.control.DB == nil || previousDB != nil && m.control.DB != nil && *previousDB == *m.control.DB
	return changed || !sameDB || previousMuted != m.control.Muted, nil
}

func (m *mixer) setMute(ctx context.Context, muted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.switchAvailable {
		return errors.New("ALSA playback switch unavailable")
	}
	if err := m.device.SetMuted(muted); err != nil {
		return err
	}
	m.switchMuted = muted
	m.control.Muted = muted || m.mapping.mute && m.mapping.raw(m.control.Level) == m.mapping.min
	return nil
}

// toggleMute uses the playback switch. Without one, it parks the level at the
// minimum and returns to the level it left.
func (m *mixer) toggleMute(ctx context.Context) error {
	m.mu.Lock()
	hasSwitch, muted, level, least, previous := m.switchAvailable, m.switchMuted, m.control.Level, m.control.Min, m.unmuteLevel
	if !hasSwitch && level > least {
		m.unmuteLevel = level
	}
	m.mu.Unlock()
	switch {
	case hasSwitch:
		return m.setMute(ctx, !muted)
	case level > least:
		return m.setLevel(ctx, least)
	case previous > least:
		return m.setLevel(ctx, previous)
	}
	return nil
}

func (m *mixer) apply(ctx context.Context, c *volume.Control) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c == nil || c.Revision == m.control.Revision {
		return nil
	}
	if c.Target < m.control.Min || c.Target > m.control.Max {
		return errors.New("volume target outside local bounds")
	}
	m.control.Revision = c.Revision
	if c.Target != m.control.Level {
		if err := m.write(ctx, c.Target); err != nil {
			m.control.Error = err.Error()
			return err
		}
	}
	return nil
}

func (m *mixer) registration() string {
	c := m.snapshot()
	b, _ := json.Marshal(c)
	return string(b)
}

func (m *mixer) adjust(ctx context.Context, delta int) error {
	if _, err := m.read(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	level := min(max(m.control.Level+delta, m.control.Min), m.control.Max)
	if level == m.control.Level {
		return nil
	}
	if err := m.write(ctx, level); err != nil {
		return err
	}
	m.control.Error = ""
	return nil
}

func (m *mixer) setLevel(ctx context.Context, level int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	level = min(max(level, m.control.Min), m.control.Max)
	if level == m.control.Level {
		return nil
	}
	if err := m.write(ctx, level); err != nil {
		return err
	}
	m.control.Error = ""
	return nil
}

// write is called with the mixer lock held, or before the mixer is published.
func (m *mixer) write(ctx context.Context, level int) error {
	raw := m.mapping.raw(level)
	// Quantized hardware levels must still respect the displayed cap.
	for raw > m.mapping.min && m.mapping.display(raw) > m.control.Max {
		raw--
	}
	for raw < m.mapping.max && m.mapping.display(raw) < m.control.Min {
		raw++
	}
	actual := m.mapping.display(raw)
	if actual < m.control.Min || actual > m.control.Max {
		return errors.New("hardware has no level within configured display bounds")
	}
	if err := m.device.SetVolume(raw); err != nil {
		return err
	}
	m.control.Level = actual
	m.observe(raw)
	return nil
}

// observe records the hardware's gain at raw, called with the mixer lock held.
func (m *mixer) observe(raw int) {
	m.control.DB = nil
	if m.mapping.hasDB {
		db := m.mapping.db(raw)
		m.control.DB = &db
	}
	m.control.Muted = m.switchMuted || m.mapping.mute && raw == m.mapping.min
}
