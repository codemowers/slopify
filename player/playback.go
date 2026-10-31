package main

import (
	"context"
	"time"
)

// Queue ownership, network reporting and hardware controls stay in controller.
// A backend owns decoding, buffering and its output's process lifecycle. Start
// follows close of the previous session; Close releases persistent resources.
// mpv is the only engine today; a native Go engine would implement this too.
type playbackBackend interface {
	Start(uint64, float64) playbackSession
	Close()
}

type playbackSession interface {
	current() uint64
	position() float64
	finished() bool
	failure() bool
	started() time.Time
	close()
}

type playbackServices struct {
	queue        *queueView
	streamURL    func(context.Context, uint64) (string, error)
	events       chan<- event
	decodedState func(format, string)
	// formats reports what a backend reads and what the device actually receives.
	formats func(id uint64, input, output localPCM)
	// peak reports a full-scale peak amplitude and when that audio was heard.
	peak func(float64, time.Time)
}

func (s *session) failure() bool      { return s.failed.Load() }
func (s *session) started() time.Time { return s.startedAt }
