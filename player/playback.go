package main

import (
	"context"
	"time"
)

// Queue ownership, network reporting and hardware controls stay in controller.
// The decoder backend owns buffering and audio output. Starting playback
// follows closing the previous session, which releases its output resources.
type playbackBackend interface {
	Start(uint64, float64) playbackSession
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
	decodedState func(string)
	// formats reports what a backend reads and what the device actually receives.
	formats func(id uint64, input, output localPCM)
	// peak reports an absolute S32 PCM magnitude and when that audio was heard.
	peak       func(uint32, time.Time)
	inputStats func(sourceInput)
}

func (s *session) failure() bool      { return s.failed.Load() }
func (s *session) started() time.Time { return s.startedAt }
