package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codemowers/slopify/internal/volume"
)

func TestKnobActuatesWhileHTTPReportIsBlockedAndCoalescesLatest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	control, err := parseMixer(amp2Mapping, amp2Raw, "Digital", 0, 40)
	if err != nil {
		t.Fatal(err)
	}
	writes := make(chan int, 16)
	device := &fakeMixer{mapping: amp2Mapping, raw: amp2Raw, onWrite: func(raw int) { writes <- raw }}
	m := &mixer{control: control, mapping: amp2Mapping, device: device}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	reports := make(chan volume.Report, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v volume.Report
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Error(err)
		}
		reports <- v
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	c := &controller{server: srv.URL, mixer: m, knob: make(chan knobEvent, 16), buttons: make(chan string, 1), volumeWake: make(chan struct{}, 1)}
	knobDone, reportDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(knobDone); c.runKnob(ctx) }()
	go func() { defer close(reportDone); c.volumeReports(ctx) }()
	t.Cleanup(func() { unblock(); cancel(); <-knobDone; <-reportDone; srv.Close() })
	c.knob <- knobEvent{delta: 1}
	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("first detent not applied")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("report not started")
	}
	// Playback is not running and HTTP is blocked, but local actuation must
	// continue. Button delivery also must not block rotation when its queue fills.
	c.buttons <- "toggle"
	c.knob <- knobEvent{action: actionPlayPause}
	for range 5 {
		c.knob <- knobEvent{delta: 1}
	}
	for range 5 {
		select {
		case <-writes:
		case <-time.After(time.Second):
			t.Fatal("HTTP or button delivery blocked local ALSA actuation")
		}
	}
	if got := m.snapshot().Level; got != 26 {
		t.Fatalf("volume=%d, want 26", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent HTTP reports: %d", got)
	}
	unblock()
	first := <-reports
	if first.Level != 21 {
		t.Fatalf("first report=%d", first.Level)
	}
	select {
	case last := <-reports:
		if last.Level != 26 {
			t.Fatalf("stale intermediate report=%d, want latest 26", last.Level)
		}
	case <-time.After(time.Second):
		t.Fatal("latest volume never reported")
	}
}
