package main

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// pinnedSetup has alice's queue paused with a speaker and her tab selected.
func pinnedSetup(t *testing.T) (*store, *Player, *Player) {
	t.Helper()
	s := queueStore()
	t.Cleanup(addBrowser(t, s, "alice", "web-alice"))
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "song.flac", Duration: 120}}, Play: true}); err != nil {
		t.Fatal(err)
	}
	amp := &Player{ID: "amp", Kind: Device, Owner: queueID("alice"), Connected: true, Adopted: true}
	s.players[amp.ID] = amp
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{"amp", "web-alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.userCommand("alice", command{Op: "pause"}); err != nil {
		t.Fatal(err)
	}
	return s, s.players[queueID("alice")], amp
}

func TestPinsLapseOnlyAfterTheirTimeoutWhileStopped(t *testing.T) {
	s, q, _ := pinnedSetup(t)
	if q.IdleSince.IsZero() {
		t.Fatal("pausing did not start the idle clock")
	}
	if err := s.userCommand("alice", command{Op: "keep", To: "web-alice", Minutes: 0}); err != nil {
		t.Fatal(err)
	}
	if s.expirePins(q, q.IdleSince.Add(29*time.Minute)) {
		t.Fatal("pin lapsed before its 30 minutes")
	}
	if !s.expirePins(q, q.IdleSince.Add(31*time.Minute)) {
		t.Fatal("default pin did not lapse after 30 minutes")
	}
	if got := queueOutputs(q); !slices.Equal(got, []string{"web-alice"}) {
		t.Fatalf("outputs %v, want only the always-pinned tab", got)
	}
}

func TestPlayingQueueKeepsItsPins(t *testing.T) {
	s, q, _ := pinnedSetup(t)
	if err := s.userCommand("alice", command{Op: "play"}); err != nil {
		t.Fatal(err)
	}
	if !q.IdleSince.IsZero() || s.expirePins(q, time.Now().Add(24*time.Hour)) {
		t.Fatal("pins lapsed while playing")
	}
}

// Play from a tab after everything lapsed plays there, not on the speaker.
func TestPlayAfterLapseClaimsThePressingBrowser(t *testing.T) {
	s, q, amp := pinnedSetup(t)
	q.IdleSince = time.Now().Add(-time.Hour)
	if err := s.userCommand("alice", command{Op: "play", From: "web-alice"}); err != nil {
		t.Fatal(err)
	}
	if got := queueOutputs(q); !slices.Equal(got, []string{"web-alice"}) || q.State != Playing {
		t.Fatalf("outputs %v, state %s; want only the tab, playing", got, q.State)
	}
	if amp.State == Playing {
		t.Fatal("lapsed speaker started playing")
	}
}

func TestChoosingOutputsRestartsTheIdleClock(t *testing.T) {
	s, q, _ := pinnedSetup(t)
	q.IdleSince = time.Now().Add(-time.Hour)
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{"amp"}}); err != nil {
		t.Fatal(err)
	}
	if s.expirePins(q, time.Now()) {
		t.Fatal("a just-chosen output lapsed at once")
	}
	if err := s.userCommand("alice", command{Op: "play", From: "web-alice"}); err != nil {
		t.Fatal(err)
	}
	if got := queueOutputs(q); !slices.Equal(got, []string{"amp"}) {
		t.Fatalf("outputs %v, want the speaker just chosen", got)
	}
}

func TestKeepValidatesOwnerAndRange(t *testing.T) {
	s, _, _ := pinnedSetup(t)
	s.players["other"] = &Player{ID: "other", Kind: Device, Owner: queueID("mallory"), Adopted: true}
	for _, c := range []command{
		{Op: "keep", To: "other", Minutes: 0},
		{Op: "keep", To: "amp", Minutes: -1},
		{Op: "keep", To: "amp", Minutes: maxKeepMinutes + 1},
		{Op: "keep", To: "missing", Minutes: 60},
	} {
		if err := s.userCommand("alice", c); !errors.Is(err, errBadRequest) {
			t.Errorf("%+v: %v, want bad request", c, err)
		}
	}
	if err := s.userCommand("alice", command{Op: "keep", To: "amp", Minutes: 120}); err != nil {
		t.Fatal(err)
	}
	if m := s.players[queueID("alice")].keepMinutes("amp"); m != 120 {
		t.Fatalf("keep %d, want 120", m)
	}
}
