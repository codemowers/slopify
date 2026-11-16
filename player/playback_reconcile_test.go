package main

import "testing"

type reconcileSession struct {
	queueTestSession
	id uint64
	at float64
}

func (s *reconcileSession) current() uint64   { return s.id }
func (s *reconcileSession) position() float64 { return s.at }

type reconcileBackend struct {
	starts int
	at     float64
}

func (b *reconcileBackend) Start(id uint64, at float64) playbackSession {
	b.starts++
	b.at = at
	return &reconcileSession{id: id, at: at}
}

func TestReconcileSeparatesProgressFromSeekEvents(t *testing.T) {
	for _, tc := range []struct {
		name       string
		previousID uint64
		command    string
		seek       *seekEvent
		starts     int
	}{
		{"delayed natural advance", 7, "continuous", nil, 0},
		{"duplicate advance acknowledgment", 8, "continuous", nil, 0},
		{"unrelated queue command", 8, "repeat-setting", nil, 0},
		{"explicit seek to start", 8, "seek", &seekEvent{ID: "seek-1", Entry: 8, Position: 0}, 1},
		{"small explicit seek", 8, "seek", &seekEvent{ID: "seek-1", Entry: 8, Position: 11.75}, 1},
		{"seek for another entry", 8, "seek", &seekEvent{ID: "seek-1", Entry: 7}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &reconcileBackend{}
			// Playback has advanced, while the server snapshot still reports zero.
			session := &reconcileSession{id: 8, at: 12}
			c := &controller{backend: backend, sess: session, applied: 1,
				desired: &state{Adopted: true, State: "playing", Current: tc.previousID,
					Version: 1, QueueCommand: "continuous"}}
			next := &state{Adopted: true, State: "playing", Current: 8, Version: 2,
				QueueCommand: tc.command, Seek: tc.seek, Queue: []entry{{ID: 8}}}
			c.reconcile(next)
			// A newer progress revision carrying the same event must not replay it.
			ack := *next
			ack.Version++
			ack.Position = 42
			c.reconcile(&ack)
			if backend.starts != tc.starts || session.closed != (tc.starts != 0) {
				t.Fatalf("starts=%d closed=%v, want %d", backend.starts, session.closed, tc.starts)
			}
			if tc.starts != 0 && backend.at != tc.seek.Position {
				t.Fatalf("seek target=%v, want %v", backend.at, tc.seek.Position)
			}
		})
	}
}
