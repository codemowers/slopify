package main

import (
	"errors"
	"github.com/codemowers/slopify/internal/volume"
	"math"
)

// Match alsamixer's perceptual 0..100 scale: linear dB for ranges <=24 dB,
// otherwise the cube root of amplitude; raw-linear only without dB metadata.
// See alsa-utils/alsamixer/volume_mapping.c. No software audio gain is applied.
type mixerMapping struct {
	min, max       int
	dbBase, dbStep float64
	hasDB, mute    bool
}

func (m mixerMapping) db(raw int) float64 { return m.dbBase + float64(raw-m.min)*m.dbStep }
func (m mixerMapping) linearDB() bool     { return !m.mute && float64(m.max-m.min)*m.dbStep <= 24 }
func (m mixerMapping) display(raw int) int {
	if raw <= m.min {
		return 0
	}
	if raw >= m.max {
		return 100
	}
	normalized := float64(raw-m.min) / float64(m.max-m.min)
	if m.hasDB && !m.linearDB() {
		normalized = math.Pow(10, (m.db(raw)-m.db(m.max))/60)
		if !m.mute {
			floor := math.Pow(10, (m.dbBase-m.db(m.max))/60)
			normalized = (normalized - floor) / (1 - floor)
		}
	}
	return min(max(int(math.RoundToEven(normalized*100)), 0), 100)
}

func (m mixerMapping) raw(level int) int {
	if level <= 0 {
		return m.min
	}
	if level >= 100 {
		return m.max
	}
	normalized := float64(level) / 100
	value := float64(m.min) + normalized*float64(m.max-m.min)
	if m.hasDB && !m.linearDB() {
		if !m.mute {
			floor := math.Pow(10, (m.dbBase-m.db(m.max))/60)
			normalized = normalized*(1-floor) + floor
		}
		db := m.db(m.max) + 60*math.Log10(normalized)
		value = float64(m.min) + (db-m.dbBase)/m.dbStep
	}
	raw := min(max(int(math.RoundToEven(value)), m.min), m.max)
	// Hardware may have no representation for displayed 1. Moving upward from
	// mute must still reach the first audible hardware step, as alsamixer does.
	if m.mute && raw == m.min {
		raw++
	}
	return raw
}

func parseMixer(m mixerMapping, raw int, name string, min, max int) (volume.Control, error) {
	if min < 0 || max > 100 || max < min {
		return volume.Control{}, errors.New("volume bounds must be within alsamixer's 0..100 scale")
	}
	c := volume.Control{Name: name, Scale: "alsamixer", Min: min, Max: max, Level: m.display(raw), Target: m.display(raw)}
	for n := min; n <= max; n++ {
		r := m.raw(n)
		step := volume.Step{Level: n}
		if m.hasDB {
			db := m.db(r)
			step.DB = &db
			step.Muted = m.mute && r == m.min
		}
		c.Steps = append(c.Steps, step)
	}
	return c, nil
}
