// Package volume describes opt-in hardware mixer controls shared by devices
// and the players service. Levels use the perceptual 0..100 alsamixer scale.
package volume

import "math"

type Step struct {
	Level int      `json:"level"`
	DB    *float64 `json:"db,omitempty"`
	Muted bool     `json:"muted,omitempty"`
}

type Control struct {
	Scale    string `json:"scale"`
	Name     string `json:"name"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Level    int    `json:"level"`
	Target   int    `json:"target"`
	Revision uint64 `json:"revision"`
	Steps    []Step `json:"steps"`
	Error    string `json:"error,omitempty"`
	// DB and Muted are what the hardware actually reports, including its own
	// playback switch. Levels stay the configuration and command scale.
	DB    *float64 `json:"db,omitempty"`
	Muted bool     `json:"muted,omitempty"`
}

func (c *Control) Valid() bool {
	if c == nil || c.Name == "" || len(c.Name) > 64 || c.Scale != "alsamixer" || c.Min < 0 || c.Max > 100 || c.Max < c.Min || len(c.Steps) != c.Max-c.Min+1 || c.Level < c.Min || c.Level > c.Max || len(c.Error) > 256 {
		return false
	}
	for i, s := range c.Steps {
		if s.Level != c.Min+i || s.DB != nil && (math.IsNaN(*s.DB) || math.IsInf(*s.DB, 0)) {
			return false
		}
	}
	return true
}

type Report struct {
	Revision uint64 `json:"revision"`
	Level    int    `json:"level"`
	Error    string `json:"error,omitempty"`
	// DB is the hardware gain, absent when the control has no dB metadata.
	DB    *float64 `json:"db,omitempty"`
	Muted bool     `json:"muted,omitempty"`
}

// ValidDB bounds a reported gain to what any real control could produce.
func ValidDB(db *float64) bool {
	return db == nil || !math.IsNaN(*db) && !math.IsInf(*db, 0) && *db >= -200 && *db <= 60
}
