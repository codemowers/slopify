package main

import "testing"

func TestSeekEventIsDistinctFromProgressReports(t *testing.T) {
	s := queueStore()
	detach := addBrowser(t, s, "alice", "browser")
	defer detach()
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "one.flac"}, {Path: "two.flac"}}}); err != nil {
		t.Fatal(err)
	}
	p := s.players["browser"]
	q := s.players[queueID("alice")]
	if p.Seek != nil || q.Seek != nil {
		t.Fatal("initial playback invented a seek")
	}
	if err := s.userCommand("alice", command{Op: "seek", Position: 0.25}); err != nil {
		t.Fatal(err)
	}
	seek := p.Seek
	if seek == nil || seek.ID == "" || seek.Entry != p.Current || seek.Position != 0.25 || q.Seek != seek {
		t.Fatalf("seek not delivered to output: %+v", seek)
	}
	if err := s.report(p.ID, report{Version: p.Version, Entry: p.Current, State: Playing, Position: 3}); err != nil {
		t.Fatal(err)
	}
	if p.Seek != seek || q.Seek != seek || seek.Position != 0.25 {
		t.Fatal("progress changed or replayed the seek intent")
	}
	if err := s.userCommand("alice", command{Op: "seek", Position: 0.25}); err != nil {
		t.Fatal(err)
	}
	if p.Seek == nil || p.Seek.ID == seek.ID {
		t.Fatal("second seek to the same position did not get a new identity")
	}
	if err := s.report(p.ID, report{Version: p.Version, Entry: p.Current, State: Playing, Ended: true}); err != nil {
		t.Fatal(err)
	}
	if p.Seek != nil || q.Seek != nil {
		t.Fatal("natural track advance retained the previous track's seek")
	}
}
