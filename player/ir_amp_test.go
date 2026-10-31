package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const (
	testUp, testDown, testOn, testOff uint8 = 1, 2, 3, 4
)

type fakeIR struct {
	sent []uint8
	err  error
}

func (f *fakeIR) Send(c uint8) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, c)
	return nil
}

func (f *fakeIR) count(c uint8) int {
	n := 0
	for _, v := range f.sent {
		if v == c {
			n++
		}
	}
	return n
}

type ampHarness struct {
	amp    *irAmp
	ir     *fakeIR
	active bool
	now    time.Time
}

func newAmpHarness(t *testing.T, file string) *ampHarness {
	t.Helper()
	h := &ampHarness{ir: &fakeIR{}, now: time.Unix(1000, 0)}
	if file == "" {
		file = filepath.Join(t.TempDir(), "amp.json")
	}
	h.amp = newIRAmp(irAmpConfig{VolumeUp: testUp, VolumeDown: testDown, PowerOn: testOn, PowerOff: testOff,
		Sweep: time.Second, IdleOff: 5 * time.Second, Wake: 500 * time.Millisecond, Repeat: 100 * time.Millisecond,
		InitialSteps: 3, MaxSteps: 5, StateFile: file}, h.ir, func() bool { return h.active })
	return h
}

func (h *ampHarness) run(d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); h.now = h.now.Add(100 * time.Millisecond) {
		h.amp.step(h.now)
	}
}

func TestAmpIdleSweepsThenPowersOffOnce(t *testing.T) {
	h := newAmpHarness(t, "")
	h.run(4 * time.Second)
	if len(h.ir.sent) != 0 {
		t.Fatal("acted before the idle time", h.ir.sent)
	}
	h.run(8 * time.Second)
	// An unknown amp may be in standby, where it ignores volume: wake it first.
	if h.ir.sent[0] != testOn || h.ir.count(testDown) < 9 || h.ir.sent[len(h.ir.sent)-1] != testOff || h.ir.count(testOff) != 1 {
		t.Fatal(h.ir.sent)
	}
	if h.amp.phase != irAmpOff {
		t.Fatal(h.amp.phase)
	}
	sent := len(h.ir.sent)
	h.run(time.Minute)
	if len(h.ir.sent) != sent {
		t.Fatal("kept transmitting while off")
	}
}

func TestAmpRestoresSavedStepsFromZero(t *testing.T) {
	h := newAmpHarness(t, "")
	h.run(12 * time.Second)
	h.ir.sent = nil
	h.active = true
	h.run(2 * time.Second)
	if h.ir.sent[0] != testOn || h.ir.count(testUp) != 3 || h.ir.count(testDown) != 0 || h.amp.phase != irAmpOn {
		t.Fatal(h.ir.sent, h.amp.phase)
	}
	h.ir.sent = nil
	h.run(time.Minute)
	if len(h.ir.sent) != 0 {
		t.Fatal("transmitted while playing", h.ir.sent)
	}
}

func TestAmpKnobIsCappedAndRemembered(t *testing.T) {
	file := filepath.Join(t.TempDir(), "amp.json")
	h := newAmpHarness(t, file)
	h.run(12 * time.Second)
	h.active = true
	h.run(2 * time.Second)
	h.ir.sent = nil
	h.amp.adjust(50)
	h.run(2 * time.Second)
	if h.ir.count(testUp) != 2 || h.amp.target != 5 {
		t.Fatal(h.ir.sent, h.amp.target)
	}
	h.amp.adjust(-1)
	h.run(time.Second)
	if h.ir.count(testDown) != 1 || h.amp.position != 4 {
		t.Fatal(h.ir.sent, h.amp.position)
	}
	h.active = false
	h.run(12 * time.Second)
	if h.amp.phase != irAmpOff {
		t.Fatal(h.amp.phase)
	}
	again := newAmpHarness(t, file)
	if again.amp.phase != irAmpOff || again.amp.target != 4 {
		t.Fatal(again.amp.phase, again.amp.target)
	}
	again.active = true
	again.run(2 * time.Second)
	if again.ir.count(testUp) != 4 {
		t.Fatal(again.ir.sent)
	}
}

func TestAmpSweepFinishesWhenMusicReturns(t *testing.T) {
	h := newAmpHarness(t, "")
	h.run(12 * time.Second)
	h.active = true
	h.run(2 * time.Second)
	h.active = false
	h.run(5200 * time.Millisecond)
	if h.amp.phase != irAmpSweeping {
		t.Fatal(h.amp.phase)
	}
	h.ir.sent = nil
	h.active = true
	h.run(3 * time.Second)
	if h.ir.count(testOff) != 0 || h.ir.count(testDown) < 5 || h.ir.count(testUp) != 3 {
		t.Fatal(h.ir.sent)
	}
	if h.ir.sent[0] != testDown || h.ir.sent[len(h.ir.sent)-1] != testUp {
		t.Fatal("did not reach the bottom before raising", h.ir.sent)
	}
}

func TestAmpFailureForgetsPosition(t *testing.T) {
	h := newAmpHarness(t, "")
	h.run(12 * time.Second)
	h.active = true
	h.ir.err = errors.New("no device")
	h.run(time.Second)
	if h.amp.phase != irAmpUnknown {
		t.Fatal(h.amp.phase)
	}
	// Recovery never counts up from a position it no longer knows.
	h.ir.err = nil
	h.ir.sent = nil
	h.run(10 * time.Second)
	if h.ir.count(testUp) != 0 {
		t.Fatal(h.ir.sent)
	}
}

func TestAmpMissingDeviceDoesNotPanicOrSpin(t *testing.T) {
	h := newAmpHarness(t, "")
	h.ir.err = errors.New("no device")
	h.run(time.Minute)
	if h.amp.phase != irAmpUnknown {
		t.Fatal(h.amp.phase)
	}
}
