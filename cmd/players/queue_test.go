package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codemowers/slopify/internal/events"
)

func queueStore() *store {
	return &store{players: map[string]*Player{}, hub: events.NewHub[message](), dirty: make(chan struct{}, 1)}
}

func addBrowser(t *testing.T, s *store, user, id string) func() {
	t.Helper()
	detach, err := s.attach(id, func() *Player { return &Player{Kind: Browser, Name: id, Adopted: true} }, func(p *Player) error { p.Owner = queueID(user); return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.attachUser(user, id)
	return detach
}

func TestQueueFollowsUserAcrossBrowsersAndDevices(t *testing.T) {
	s := queueStore()
	closeDesktop := addBrowser(t, s, "alice", "web-desktop")
	defer closeDesktop()
	tracks := []Entry{{Path: "Album/one.flac", Title: "One", Duration: 180}, {Path: "Album/two.flac", Title: "Two"}}
	if err := s.userCommand("alice", command{Op: "append", Tracks: tracks}); err != nil {
		t.Fatal(err)
	}
	desktop := s.players["web-desktop"]
	if err := s.report(desktop.ID, report{Entry: desktop.Current, Version: desktop.Version, State: Playing, Position: 42}); err != nil {
		t.Fatal(err)
	}
	closeMobile := addBrowser(t, s, "alice", "web-mobile")
	defer closeMobile()
	q := s.players[queueID("alice")]
	if q.OutputID != "web-desktop" || len(q.Queue) != 2 || q.Position < 42 {
		t.Fatalf("opening mobile changed playback: %+v", q)
	}
	snapshots := s.snapshotFor("alice")
	found := false
	for _, b := range snapshots {
		var p Player
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if p.Kind == User {
			found = true
			if len(p.Queue) != 2 || p.OutputID != "web-desktop" {
				t.Fatalf("wrong account snapshot: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("no account queue in mobile snapshot")
	}
	if err := s.userCommand("alice", command{Op: "pause"}); err != nil {
		t.Fatal(err)
	}
	s.players["dac"] = &Player{ID: "dac", Kind: Device, Owner: queueID("alice"), Adopted: true, Connected: true, State: Stopped, UpdatedAt: time.Now()}
	if err := s.userCommand("alice", command{Op: "transfer", To: "dac"}); err != nil {
		t.Fatal(err)
	}
	if q.OutputID != "dac" || q.State != Paused || len(q.Queue) != 2 || q.Queue[0].ID != desktop.Queue[0].ID {
		t.Fatalf("transfer lost queue/state: %+v", q)
	}
	if s.players["dac"].Position < 42 || s.players["dac"].State != Paused {
		t.Fatal("transfer lost position or resumed paused playback")
	}
	if err := s.userCommand("alice", command{Op: "play"}); err != nil {
		t.Fatal(err)
	}
	dac := s.players["dac"]
	if err := s.report("dac", report{Entry: dac.Current, Version: dac.Version, Ended: true}); err != nil {
		t.Fatal(err)
	}
	if q.Current != q.Queue[1].ID {
		t.Fatal("device track end did not update user's queue")
	}
}

func TestQueueIsolationAndStaleReports(t *testing.T) {
	s := queueStore()
	defer addBrowser(t, s, "alice", "web-alice")()
	defer addBrowser(t, s, "bob", "web-bob")()
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "private.flac"}}}); err != nil {
		t.Fatal(err)
	}
	if len(s.players[queueID("bob")].Queue) != 0 {
		t.Fatal("queues shared between users")
	}
	for _, b := range s.snapshotFor("bob") {
		var p Player
		_ = json.Unmarshal(b, &p)
		if p.ID == queueID("alice") || p.ID == "web-alice" {
			t.Fatal("another user's queue leaked")
		}
	}
	if err := s.userCommand("bob", command{Op: "transfer", To: "web-alice"}); !errors.Is(err, errForbidden) {
		t.Fatalf("foreign browser allowed: %v", err)
	}
	if err := s.browserReport("bob", "web-alice", report{}); !errors.Is(err, errForbidden) {
		t.Fatalf("foreign report allowed: %v", err)
	}
	if err := s.webCommand("bob", queueID("alice"), command{Op: "forget"}); !errors.Is(err, errForbidden) {
		t.Fatalf("foreign queue mutation allowed: %v", err)
	}
	p := s.players["web-alice"]
	v, entry := p.Version, p.Current
	if err := s.userCommand("alice", command{Op: "pause"}); err != nil {
		t.Fatal(err)
	}
	if err := s.report(p.ID, report{Version: v, Entry: entry, Ended: true}); err != nil {
		t.Fatal(err)
	}
	if s.players[queueID("alice")].State != Paused {
		t.Fatal("stale report changed user state")
	}
}

func TestQueueSurvivesRestartAndOutputRemoval(t *testing.T) {
	s := queueStore()
	closeBrowser := addBrowser(t, s, "alice", "web-desktop")
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac"}}}); err != nil {
		t.Fatal(err)
	}
	closeBrowser()
	q := s.players[queueID("alice")]
	if q.State != Playing || q.Connected {
		t.Fatal("browser disconnect did not enter reconnect grace")
	}
	output := s.players["web-desktop"]
	output.disconnectTimer.Stop()
	s.finishBrowserDisconnect(output, output.connectionGeneration)
	if q.State != Paused {
		t.Fatal("browser did not pause after reconnect grace")
	}

	ps := []*Player{q, s.players["web-desktop"]}
	b, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "players.json")
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	restored := queueStore()
	restored.file = file
	if err := restored.load(); err != nil {
		t.Fatal(err)
	}
	defer addBrowser(t, restored, "alice", "web-phone")()
	q = restored.players[queueID("alice")]
	if len(q.Queue) != 1 || q.OutputID != "web-desktop" {
		t.Fatalf("restart/new phone lost queue: %+v", q)
	}
	if err := restored.webCommand("alice", "web-desktop", command{Op: "forget"}); err != nil {
		t.Fatal(err)
	}
	if len(q.Queue) != 1 || q.OutputID != "" {
		t.Fatal("forget output deleted user queue")
	}
	if err := restored.userCommand("alice", command{Op: "transfer", To: "web-phone"}); err != nil {
		t.Fatal(err)
	}
	if len(restored.players["web-phone"].Queue) != 1 {
		t.Fatal("could not reattach queue")
	}
}

func TestReturningSelectedOutputJoinsCurrentPlayback(t *testing.T) {
	s := queueStore()
	closePrimary := addBrowser(t, s, "alice", "web-primary")
	defer closePrimary()
	closeSecondary := addBrowser(t, s, "alice", "web-secondary")
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{"web-primary", "web-secondary"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "movie.mp4", Title: "Movie", Duration: 120}}}); err != nil {
		t.Fatal(err)
	}
	closeSecondary()
	primary := s.players["web-primary"]
	if err := s.report(primary.ID, report{Entry: primary.Current, Version: primary.Version, State: Playing, Position: 42}); err != nil {
		t.Fatal(err)
	}
	closeSecondary = addBrowser(t, s, "alice", "web-secondary")
	defer closeSecondary()
	secondary := s.players["web-secondary"]
	if secondary.State != Playing || secondary.Current != primary.Current || s.pos(secondary) < 42 || s.pos(secondary) > 43 {
		t.Fatalf("returning output did not join current playback: state=%s current=%d position=%f", secondary.State, secondary.Current, s.pos(secondary))
	}
	q := s.players[queueID("alice")]
	if q.OutputID != primary.ID || q.State != Playing || len(q.OutputIDs) != 2 {
		t.Fatalf("returning secondary changed primary or selections: %+v", q)
	}
}

func TestAppendVideoAlwaysGoesToQueueTail(t *testing.T) {
	s := queueStore()
	detach := addBrowser(t, s, "alice", "web-desktop")
	defer detach()
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{
		{Path: "first.mp3", Duration: 120},
		{Path: "second.mp3"},
		{Path: "third.mp3"},
	}}); err != nil {
		t.Fatal(err)
	}
	q := s.players[queueID("alice")]
	current := q.Current
	before := q.Queue[1].ID
	if err := s.userCommand("alice", command{Op: "append", Before: before, Tracks: []Entry{{Path: "movie.mp4"}}}); err != nil {
		t.Fatal(err)
	}
	if q.Current != current || q.State != Playing {
		t.Fatal("append interrupted current playback")
	}
	for i, want := range []string{"first.mp3", "second.mp3", "third.mp3", "movie.mp4"} {
		if got := q.Queue[i].Path; got != want {
			t.Fatalf("queue[%d]=%s, want %s", i, got, want)
		}
	}
	if err := s.userCommand("alice", command{Op: "insert", Before: before, Tracks: []Entry{{Path: "inserted.mp4"}}}); err != nil {
		t.Fatal(err)
	}
	if q.Queue[1].Path != "inserted.mp4" || q.Current != current {
		t.Fatal("explicit drag/drop insertion no longer works")
	}
}

func TestExistingQueueCorruptTagsUseFilename(t *testing.T) {
	entry := Entry{
		Path:  "audio/Eesti/Dagö/(2000) Dagö/Dagö -06- Härra Sannikov.mp3",
		Title: "H�rra Sannikov", Artist: "Dag�", Album: "Keep this valid album",
	}
	cleanEntryText(&entry)
	if entry.Title != "Härra Sannikov" || entry.Artist != "Dagö" || entry.Album != "Keep this valid album" {
		t.Fatalf("incorrect queue metadata fallback: %+v", entry)
	}
}

func TestRepeatWrapsQueueAndPersistsOnOutputs(t *testing.T) {
	s := queueStore()
	defer addBrowser(t, s, "alice", "web")()
	tracks := []Entry{{Path: "a.flac", Title: "A"}, {Path: "b.flac", Title: "B"}}
	if err := s.userCommand("alice", command{Op: "replace", Tracks: tracks}); err != nil {
		t.Fatal(err)
	}
	web := s.players["web"]
	first, last := web.Queue[0].ID, web.Queue[1].ID
	end := func() {
		t.Helper()
		if err := s.report("web", report{Version: web.Version, Entry: web.Current, Ended: true}); err != nil {
			t.Fatal(err)
		}
	}
	end()
	if web.Current != last || web.State != Playing {
		t.Fatalf("did not advance: %+v", web)
	}
	if err := s.userCommand("alice", command{Op: "repeat", Repeat: true}); err != nil {
		t.Fatal(err)
	}
	if !web.Repeat || web.Current != last || web.State != Playing {
		t.Fatalf("repeat changed playback or did not reach the output: %+v", web)
	}
	end()
	q := s.players[queueID("alice")]
	if web.Current != first || web.State != Playing || q.Current != first || !q.Repeat {
		t.Fatalf("did not wrap: output %+v queue %+v", web, q)
	}
	if err := s.userCommand("alice", command{Op: "prev"}); err != nil {
		t.Fatal(err)
	}
	if q.Current != last {
		t.Fatalf("previous did not wrap: %+v", q)
	}
	if err := s.userCommand("alice", command{Op: "repeat", Repeat: false}); err != nil {
		t.Fatal(err)
	}
	end()
	if web.State != Stopped {
		t.Fatalf("stopped repeating but kept playing: %+v", web)
	}
}
