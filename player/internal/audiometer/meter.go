// Package audiometer consumes full-scale peak envelopes from any PCM monitor.
// It has no playback, discovery or source-selection policy.
package audiometer

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"time"
)

type Window struct {
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	Peak     float64   `json:"peak"`
	DBFS     *float64  `json:"dbfs"`
}

type Meter struct {
	mu     sync.Mutex
	latest Window
}

func New() *Meter { return &Meter{} }

func (m *Meter) Observe(peak float64, at time.Time) {
	w := Window{At: at, Peak: peak}
	if peak > 0 {
		db := 20 * math.Log10(peak)
		w.DBFS = &db
	}
	m.mu.Lock()
	w.Sequence = m.latest.Sequence + 1
	m.latest = w
	m.mu.Unlock()
}

func (m *Meter) Snapshot() Window { m.mu.Lock(); defer m.mu.Unlock(); return m.latest }

// ReadPeaks accepts little-endian interleaved float32 peak amplitudes, such as
// PipeWire stream.monitor=true produces. Sampling here affects observation only.
func (m *Meter) ReadPeaks(r io.Reader, rate, channels int) error {
	if rate < 5 || rate > 1000 || channels < 1 || channels > 64 {
		return errors.New("invalid peak monitor format")
	}
	data := make([]byte, (rate/5)*channels*4)
	for {
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		peak := 0.0
		for i := 0; i < len(data); i += 4 {
			v := math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(data[i:]))))
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("nonfinite peak monitor value")
			}
			if v > 16 {
				return errors.New("invalid peak monitor amplitude")
			}
			peak = max(peak, v)
		}
		m.Observe(peak, time.Now())
	}
}
