package main

import (
	"context"
	"testing"
)

// The Amp2's Digital control, as its driver reports it.
var amp2Mapping = mixerMapping{min: 0, max: 207, dbBase: -103.5, dbStep: 0.5, hasDB: true, mute: true}

const amp2Raw = 123

// fakeMixer records writes without hardware.
type fakeMixer struct {
	mapping mixerMapping
	raw     int
	muted   bool
	onWrite func(int)
	writes  int
}

func (f *fakeMixer) Volume() (mixerMapping, int, error) { return f.mapping, f.raw, nil }
func (f *fakeMixer) SetVolume(raw int) error {
	f.raw = raw
	f.writes++
	if f.onWrite != nil {
		f.onWrite(raw)
	}
	return nil
}
func (f *fakeMixer) Muted() (bool, error) { return f.muted, nil }
func (f *fakeMixer) SetMuted(m bool) error {
	f.muted = m
	f.writes++
	return nil
}

func TestAmp2AlsamixerDisplayMapping(t *testing.T) {
	c, err := parseMixer(amp2Mapping, amp2Raw, "Digital", 0, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Valid() || c.Scale != "alsamixer" || c.Level != 20 || len(c.Steps) != 41 || !c.Steps[0].Muted || *c.Steps[20].DB != -42 || *c.Steps[40].DB != -24 {
		t.Fatalf("wrong alsamixer mapping: %+v", c)
	}
	m := amp2Mapping
	for _, tc := range []struct{ display, raw int }{{0, 0}, {20, 123}, {40, 159}, {100, 207}} {
		if m.raw(tc.display) != tc.raw || m.display(tc.raw) != tc.display {
			t.Fatalf("display %d / raw %d mismatch", tc.display, tc.raw)
		}
	}
	for n := 2; n <= 40; n++ {
		if got := m.display(m.raw(n)); got != n {
			t.Fatalf("display %d became %d", n, got)
		}
	}
	if m.display(m.raw(1)) <= 0 {
		t.Fatal("first rotary increase is stuck at mute")
	}
	for _, bounds := range [][2]int{{-1, 40}, {0, 101}, {40, 20}} {
		if _, err := parseMixer(amp2Mapping, amp2Raw, "Digital", bounds[0], bounds[1]); err == nil {
			t.Fatal("invalid display bounds accepted", bounds)
		}
	}
}

func TestMixerMappingLinearAndNoDB(t *testing.T) {
	for _, m := range []mixerMapping{
		{min: 0, max: 100, dbBase: -20, dbStep: 0.2, hasDB: true},
		{min: 0, max: 100},
	} {
		if m.display(20) != 20 || m.raw(40) != 40 {
			t.Fatalf("incorrect linear mapping %+v", m)
		}
	}
	// A finite, nonmuting minimum uses an offset perceptual curve.
	m := amp2Mapping
	m.mute = false
	if m.display(m.min) != 0 || m.raw(0) != m.min || m.display(m.max) != 100 {
		t.Fatal("nonmuting endpoints differ")
	}
}

func TestMixerReportsHardwareGainAndMute(t *testing.T) {
	device := &fakeMixer{mapping: amp2Mapping, raw: amp2Raw}
	m, err := startMixer(context.Background(), device, "card", "Digital", 0, 40, 20)
	if err != nil {
		t.Fatal(err)
	}
	c := m.snapshot()
	if c.DB == nil || *c.DB != -42 || c.Muted || c.Level != 20 {
		t.Fatalf("%+v", c)
	}
	if err := m.setMute(context.Background(), true); err != nil || !m.snapshot().Muted {
		t.Fatalf("mute: %v", err)
	}
	if *m.snapshot().DB != -42 {
		t.Fatal("mute changed the reported gain")
	}
	m.setMute(context.Background(), false)
	// Someone else changing the hardware is observed, as a change to report.
	device.raw = amp2Mapping.raw(30)
	changed, err := m.read(context.Background())
	if err != nil || !changed || *m.snapshot().DB == -42 || m.snapshot().Level != 30 {
		t.Fatalf("%v %v %+v", changed, err, m.snapshot())
	}
	if err := m.setLevel(context.Background(), 0); err != nil || !m.snapshot().Muted {
		t.Fatalf("level 0 is hardware mute: %v %+v", err, m.snapshot())
	}
}
