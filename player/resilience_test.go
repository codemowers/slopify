package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEventStreamEndsWhenSilent(t *testing.T) {
	previous := eventStreamIdleTimeout
	eventStreamIdleTimeout = 100 * time.Millisecond
	defer func() { eventStreamIdleTimeout = previous }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Bytes arriving within the timeout must keep the stream alive.
		for range 4 {
			w.Write([]byte(": ping\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := &controller{server: srv.URL}
	started := time.Now()
	connected, err := c.stream(context.Background(), "/")
	if !connected || !errors.Is(err, errEventStreamIdle) {
		t.Fatalf("connected=%v err=%v", connected, err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("gave up after %v despite pings", elapsed)
	}
}

func TestRetryRTPReceiverUntilNetworkIsUp(t *testing.T) {
	attempts := 0
	retryRTPReceiver(context.Background(), func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("no such device")
		}
		return nil
	}, time.Millisecond)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}
