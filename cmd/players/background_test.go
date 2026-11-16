package main

import (
	"encoding/json"
	"testing"
	"time"
)

func addBackgroundBrowser(t *testing.T, s *store, user, id string) func() {
	t.Helper()
	detach, err := s.attach(id, func() *Player { return &Player{Kind: Browser, Name: id, Adopted: true} },
		func(p *Player) error { p.Owner, p.Background = queueID(user), true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.attachUser(user, id)
	return detach
}

// An installed app whose event stream drops keeps playing, advances through its
// own reports, and learns the next track from the report reply.
func TestBackgroundAppKeepsPlayingWithoutEventStream(t *testing.T) {
	s := queueStore()
	closePhone := addBackgroundBrowser(t, s, "alice", "web-phone")
	tracks := []Entry{{Path: "one.flac", Title: "One", Duration: 180}, {Path: "two.flac", Title: "Two", Duration: 200}}
	if err := s.userCommand("alice", command{Op: "append", Tracks: tracks}); err != nil {
		t.Fatal(err)
	}
	closePhone() // the screen locks; Android drops the stream
	q, phone := s.players[queueID("alice")], s.players["web-phone"]
	if q.State != Playing || !q.Connected || !phone.available() || phone.disconnectTimer != nil {
		t.Fatalf("background app paused or became unavailable on disconnect: queue %s connected %v", q.State, q.Connected)
	}
	if left := time.Until(phone.lingerUntil); left < 3*time.Minute || left > 4*time.Minute+time.Second {
		t.Fatalf("allowance %v, want the rest of a three-minute track plus slack", left)
	}
	first := phone.Current
	state, err := s.browserReport("alice", phone.ID, report{Version: phone.Version, Entry: first, Ended: true})
	if err != nil {
		t.Fatal(err)
	}
	var reply Player
	if err := json.Unmarshal(state, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Current == first || reply.State != Playing || reply.Version != phone.Version {
		t.Fatalf("reply %+v does not carry the next track", reply)
	}
	if q.State != Playing || q.Current != reply.Current {
		t.Fatal("ended report from the background app did not advance the queue")
	}
	if left := time.Until(phone.lingerUntil); left < 3*time.Minute+19*time.Second {
		t.Fatalf("allowance %v not renewed for the 200 s track", left)
	}
	// Silence past the allowance pauses: a killed app does not play forever.
	phone.lingerTimer.Stop()
	s.finishLinger(phone, phone.connectionGeneration, phone.lingerUntil)
	if q.State != Paused || phone.available() {
		t.Fatalf("expired allowance left queue %s, available %v", q.State, phone.available())
	}
}

// A tab is not an installed app: it keeps the reconnect grace and then pauses.
func TestOrdinaryTabStillUsesReconnectGrace(t *testing.T) {
	s := queueStore()
	closeTab := addBrowser(t, s, "alice", "web-tab")
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac", Duration: 180}}}); err != nil {
		t.Fatal(err)
	}
	closeTab()
	tab := s.players["web-tab"]
	if tab.disconnectTimer == nil || !tab.lingerUntil.IsZero() || tab.available() {
		t.Fatal("ordinary tab got a background allowance")
	}
	tab.disconnectTimer.Stop()
}

// Reconnecting ends the allowance; the stream is back.
func TestBackgroundAllowanceEndsOnReconnect(t *testing.T) {
	s := queueStore()
	closePhone := addBackgroundBrowser(t, s, "alice", "web-phone")
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac", Duration: 180}}}); err != nil {
		t.Fatal(err)
	}
	closePhone()
	defer addBackgroundBrowser(t, s, "alice", "web-phone")()
	phone := s.players["web-phone"]
	if phone.lingerTimer != nil || !phone.lingerUntil.IsZero() || !phone.Connected {
		t.Fatal("reconnect kept the background allowance")
	}
}

// A closing tab's report shortens the grace, whichever arrives first.
func TestGoneReportShortensGrace(t *testing.T) {
	for _, reportFirst := range []bool{true, false} {
		s := queueStore()
		closeTab := addBackgroundBrowser(t, s, "alice", "web-tab")
		if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac", Duration: 180}}}); err != nil {
			t.Fatal(err)
		}
		tab := s.players["web-tab"]
		if reportFirst {
			if err := s.report(tab.ID, report{Gone: true}); err != nil {
				t.Fatal(err)
			}
			closeTab()
		} else {
			closeTab()
			if err := s.report(tab.ID, report{Gone: true}); err != nil {
				t.Fatal(err)
			}
		}
		if tab.disconnectTimer == nil || tab.available() {
			t.Fatalf("reportFirst=%v: closing app kept its background allowance", reportFirst)
		}
		tab.disconnectTimer.Stop()
		s.finishBrowserDisconnect(tab, tab.connectionGeneration)
		if s.players[queueID("alice")].State != Paused {
			t.Fatalf("reportFirst=%v: closed tab did not pause", reportFirst)
		}
	}
}

// A reload reconnects within the short grace and keeps playing.
func TestReloadAfterGoneKeepsPlaying(t *testing.T) {
	s := queueStore()
	closeTab := addBrowser(t, s, "alice", "web-tab")
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac", Duration: 180}}}); err != nil {
		t.Fatal(err)
	}
	tab := s.players["web-tab"]
	if err := s.report(tab.ID, report{Gone: true}); err != nil {
		t.Fatal(err)
	}
	closeTab()
	defer addBrowser(t, s, "alice", "web-tab")()
	if tab.disconnectTimer != nil || tab.leaving || s.players[queueID("alice")].State != Playing {
		t.Fatal("reload after a gone report did not resume as the same output")
	}
}

func TestRenewedBackgroundAllowanceIgnoresOldCallback(t *testing.T) {
	s := queueStore()
	detach := addBackgroundBrowser(t, s, "alice", "web-phone")
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac", Duration: 180}}}); err != nil {
		t.Fatal(err)
	}
	detach()
	phone := s.players["web-phone"]
	generation, expiredDeadline := phone.connectionGeneration, phone.lingerUntil
	if err := s.report(phone.ID, report{Version: phone.Version, Entry: phone.Current, State: Playing, Position: 1}); err != nil {
		t.Fatal(err)
	}
	defer phone.lingerTimer.Stop()
	renewed := phone.lingerUntil
	// The previous timer can already be waiting for the store lock when renewed.
	s.finishLinger(phone, generation, expiredDeadline)
	if phone.State != Playing || phone.lingerUntil != renewed || s.players[queueID("alice")].State != Playing {
		t.Fatal("old timer cancelled the renewed background allowance")
	}
}

func TestPausedBackgroundOutputLosesItsPlaybackAllowance(t *testing.T) {
	s := queueStore()
	detach := addBackgroundBrowser(t, s, "alice", "web-phone")
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac", Duration: 180}}}); err != nil {
		t.Fatal(err)
	}
	detach()
	phone := s.players["web-phone"]
	defer func() {
		if phone.lingerTimer != nil {
			phone.lingerTimer.Stop()
		}
	}()
	if err := s.userCommand("alice", command{Op: "pause"}); err != nil {
		t.Fatal(err)
	}
	if phone.State != Paused || phone.available() || phone.lingerTimer != nil {
		t.Fatal("paused background output retained its playback allowance")
	}
}
