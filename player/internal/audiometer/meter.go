// Package audiometer consumes full-scale peak envelopes from any PCM monitor.
// It has no playback, discovery or source-selection policy.
package audiometer

import (
	"sync"
	"time"
)

type Window struct {
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	// Keep the exact measured peak for source arbitration. Only the display
	// envelope is quantized on the wire: -127..0 dBFS, -128 for silence.
	Peak uint32 `json:"-"` // Absolute S32 PCM magnitude; full scale is 1<<31.
	DB   int8   `json:"db"`
}

type Meter struct {
	mu     sync.Mutex
	latest Window
}

func New() *Meter { return &Meter{} }

func (m *Meter) Observe(peak uint32, at time.Time) {
	peak = min(peak, 1<<31)
	w := Window{At: at, Peak: peak, DB: peakDB(peak)}
	m.mu.Lock()
	w.Sequence = m.latest.Sequence + 1
	m.latest = w
	m.mu.Unlock()
}

func (m *Meter) Snapshot() Window { m.mu.Lock(); defer m.mu.Unlock(); return m.latest }

//go:generate sh -c "go run meter_tables_gen.go > meter_tables.go"

// Rounded whole dBFS from precomputed amplitude boundaries. No logarithm or
// floating-point conversion runs on the speaker, even for the display value.
func peakDB(peak uint32) int8 {
	if peak == 0 {
		return -128
	}
	lo, hi := 0, len(dbThresholds)
	for lo < hi {
		mid := (lo + hi) / 2
		if peak >= dbThresholds[mid] {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return -int8(lo)
}
