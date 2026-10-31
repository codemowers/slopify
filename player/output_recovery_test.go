package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestControllerIgnoresCompletionFromReplacedSession(t *testing.T) {
	reports := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reports <- struct{}{}
		w.WriteHeader(204)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	close(done)
	old := &session{first: 7, done: done, cancel: func() {}}
	current := &session{first: 7, done: done, cancel: func() {}}
	c := &controller{server: server.URL, sess: current, desired: &state{Version: 2, Current: 7}, events: make(chan event, 2)}
	c.events <- event{entry: 7, ended: true, session: old}
	c.events <- event{entry: 7, ended: true, session: current}
	stopped := make(chan struct{})
	go func() { c.run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	select {
	case <-reports:
	case <-time.After(time.Second):
		t.Fatal("current completion not reported")
	}
	select {
	case <-reports:
		t.Fatal("old completion reported with new playback version")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRecoveryWaitsAndDoesNotResumePausedOrCompletedPlayback(t *testing.T) {
	now := time.Now()
	done := make(chan struct{})
	close(done)
	s := &session{done: done, first: 7}
	s.failed.Store(true)
	c := &controller{sess: s, desired: &state{Adopted: true, State: "playing", Current: 7}}
	c.recoverOutput(now)
	if c.outputRetryAt.IsZero() || !c.outputRetryAt.After(now) {
		t.Fatal("retry was not delayed")
	}
	c.recoverOutput(now.Add(100 * time.Millisecond))
	if c.sess != s {
		t.Fatal("retry ignored backoff")
	}
	c.desired.State = "paused"
	c.recoverOutput(now.Add(time.Minute))
	if c.sess != s {
		t.Fatal("paused output resumed")
	}
	s.failed.Store(false)
	c.desired.State = "playing"
	c.recoverOutput(now.Add(time.Minute))
	if c.sess != s {
		t.Fatal("normal completion retried as an output failure")
	}
}
