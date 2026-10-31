package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSourcePauseCoalescesAndDiscardsReselectedSource(t *testing.T) {
	requests := make(chan sourcePauseIntent, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var intent sourcePauseIntent
		if r.URL.Path != "/device/v1/control" || json.NewDecoder(r.Body).Decode(&intent) != nil {
			w.WriteHeader(400)
			return
		}
		requests <- intent
		w.WriteHeader(204)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &controller{server: server.URL, sources: newSpeakerSources(""), sourcePause: make(chan sourcePauseIntent, 1), desired: &state{Adopted: true, State: "playing", Current: 7}}
	c.requestSourcePause()
	if len(c.sourcePause) != 0 {
		t.Fatal("Slopify selection requested pause")
	}
	c.sources.Selected = "lan_1"
	for i := 1; i <= 100; i++ {
		c.desired.Version = uint64(i)
		c.requestSourcePause()
	}
	if len(c.sourcePause) != 1 {
		t.Fatal("intents not coalesced")
	}
	c.sources.Selected = "slopify"
	c.sources.Revision++
	c.requestSourcePause()
	finished := make(chan struct{})
	go func() { c.runSourcePause(ctx); close(finished) }()
	defer func() { cancel(); <-finished }()
	select {
	case <-requests:
		t.Fatal("stale selection reached server")
	case <-time.After(50 * time.Millisecond):
	}
	c.sources.mu.Lock()
	c.sources.Selected = "lan_1"
	c.sources.Revision++
	c.sources.mu.Unlock()
	c.requestSourcePause()
	select {
	case r := <-requests:
		if r.Version != 100 || r.Entry != 7 || r.Op != "pause-if-sole-output" {
			t.Fatalf("wrong intent %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no conditional pause request")
	}
	for i := 0; i < 20; i++ {
		c.requestSourcePause()
	}
	select {
	case <-requests:
		t.Fatal("acknowledged intent repeated")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPinResumeIsExplicitAndOneShot(t *testing.T) {
	c := &controller{sources: newSpeakerSources(""), sourcePause: make(chan sourcePauseIntent, 1), desired: &state{Adopted: true, State: "paused", Current: 7, Version: 8}}
	c.requestSourcePause()
	if len(c.sourcePause) != 0 {
		t.Fatal("default selection resumed playback")
	}
	c.sources.resumeRevision = c.sources.Revision
	c.requestSourcePause()
	intent := <-c.sourcePause
	if intent.Op != "play-if-sole-output" || intent.Entry != 7 || intent.Version != 8 {
		t.Fatalf("wrong resume intent: %+v", intent)
	}
	c.requestSourcePause()
	if len(c.sourcePause) != 0 {
		t.Fatal("pin resumed repeatedly")
	}
}

func TestSourcePauseDoesNotOverrideLaterPlay(t *testing.T) {
	c := &controller{sources: newSpeakerSources(""), sourcePause: make(chan sourcePauseIntent, 1), desired: &state{Adopted: true, State: "playing", Current: 7, Version: 1}}
	c.requestSourcePause()
	c.sources.Selected = "lan_1"
	c.sources.Revision++
	c.requestSourcePause()
	select {
	case intent := <-c.sourcePause:
		if intent.Op != "pause-if-sole-output" {
			t.Fatalf("wrong switch intent: %+v", intent)
		}
	default:
		t.Fatal("switch away did not request pause")
	}
	c.desired.State = "paused"
	c.desired.Version++
	c.requestSourcePause()
	c.desired.State = "playing"
	c.desired.Version++
	c.requestSourcePause()
	c.desired.Current++ // Track changes also must not cause another pause.
	c.desired.Version++
	c.requestSourcePause()
	c.sources.Selected = "lan_2"
	c.sources.Revision++
	c.requestSourcePause()
	if len(c.sourcePause) != 0 {
		t.Fatal("later playback or LAN selection was overridden")
	}
}

func TestRestoredLANSelectionDoesNotPausePlayback(t *testing.T) {
	c := &controller{sources: newSpeakerSources(""), sourcePause: make(chan sourcePauseIntent, 1), desired: &state{Adopted: true, State: "playing", Current: 7}}
	c.sources.Selected = "lan_1"
	c.requestSourcePause()
	if len(c.sourcePause) != 0 {
		t.Fatal("restoring source selection was treated as a new switch")
	}
}
