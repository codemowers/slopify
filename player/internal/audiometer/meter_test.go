package audiometer

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestObserve(t *testing.T) {
	m := New()
	m.Observe(1<<30, time.Now())
	w := m.Snapshot()
	if w.Sequence != 1 || w.Peak != 1<<30 || w.DB != -6 {
		t.Fatalf("wrong window: %+v", w)
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["peak"]; ok {
		t.Fatal("raw peak exposed to browser")
	}
	if wire["db"] != float64(-6) {
		t.Fatalf("wire level: %s", data)
	}
	m.Observe(0, time.Now())
	if m.Snapshot().Peak != 0 || m.Snapshot().DB != -128 {
		t.Fatal("silence must be represented separately")
	}
}

func TestIntegerDBMatchesRoundedReference(t *testing.T) {
	check := func(peak uint32) {
		t.Helper()
		want := int8(-128)
		if peak > 0 {
			want = int8(max(-127, min(0, int(math.Round(20*math.Log10(float64(peak)/(1<<31)))))))
		}
		if got := peakDB(peak); got != want {
			t.Fatalf("peak %d: %d dBFS, want %d", peak, got, want)
		}
	}
	for _, peak := range []uint32{0, 1, 1 << 30, 1 << 31, 2147483, 2147484} {
		check(peak)
	}
	for _, threshold := range dbThresholds {
		check(threshold - 1)
		check(threshold)
		check(threshold + 1)
	}
	rng := rand.New(rand.NewSource(14))
	for range 10000 {
		check(rng.Uint32() >> 1)
	}
}
