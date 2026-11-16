package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalPlaybackVersionAndIdempotence(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var cmd localPlaybackCommand
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			t.Error(err)
		}
		if cmd.Op != "toggle" || cmd.Version != 4 || cmd.Entry != 7 {
			t.Errorf("unexpected command: %+v", cmd)
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing device credential")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	c := &controller{server: server.URL, token: "test", desired: &state{Adopted: true, Current: 7, Version: 4, State: "playing", CurrentEntry: &entry{ID: 7, Title: "Title"}}}
	for _, tt := range []struct {
		op      string
		version uint64
		status  int
		calls   int
	}{
		{"pause", 3, 409, 0}, {"play", 4, 204, 0}, {"pause", 4, 204, 1},
	} {
		req := localPlaybackRequest{ctx: context.Background(), command: &localPlaybackCommand{Op: tt.op, Version: tt.version, Entry: 7}, reply: make(chan localPlaybackResult, 1)}
		c.handleLocalPlayback(req)
		result := <-req.reply
		if result.status != tt.status || calls != tt.calls {
			t.Fatalf("%s: status=%d calls=%d", tt.op, result.status, calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := localPlaybackRequest{ctx: ctx, command: &localPlaybackCommand{Op: "pause", Version: 4, Entry: 7}, reply: make(chan localPlaybackResult, 1)}
	c.handleLocalPlayback(req)
	if (<-req.reply).status != 504 || calls != 1 {
		t.Fatal("cancelled request executed")
	}
}

func TestLocalPlaybackRejectsCrossOriginAndUnknownOperations(t *testing.T) {
	c := &controller{}
	for _, tt := range []struct {
		origin, body string
		status       int
	}{
		{"http://evil.invalid", `{"op":"next"}`, 403},
		{"http://speaker.local", `{"op":"delete"}`, 400},
		{"http://speaker.local", `{"op":"next","url":"secret"}`, 400},
	} {
		r := httptest.NewRequest("POST", "http://speaker.local/api/playback", strings.NewReader(tt.body))
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slopify-Control", "1")
		w := httptest.NewRecorder()
		c.localPlaybackControl(w, r)
		if w.Code != tt.status {
			t.Fatalf("got %d, want %d", w.Code, tt.status)
		}
	}
}
