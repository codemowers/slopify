package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/audiometer"
)

func TestOutputHistoryDeltas(t *testing.T) {
	s := newFixedSources()
	point := func(id string, seq uint64, at time.Time) sourceOutputWindow {
		return sourceOutputWindow{SourceID: id, Window: audiometer.Window{Sequence: seq, At: at, Peak: 1 << 30}}
	}
	now := time.Now()
	s.Output = []sourceOutputWindow{point("slopify", 1, now), point("slopify", 2, now.Add(time.Second))}
	var cursor outputHistoryCursor
	check := func(snapshot any, count int, delta bool) {
		t.Helper()
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Output      []sourceOutputWindow
			OutputDelta bool
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Output) != count || got.OutputDelta != delta {
			t.Fatalf("got %d points delta=%v, want %d delta=%v", len(got.Output), got.OutputDelta, count, delta)
		}
	}
	check(s.snapshot(&cursor), 2, false)
	check(s.snapshot(&cursor), 0, true)
	// Another source can restart its sequence and move timestamps backwards.
	s.Output = append(s.Output, point("lan_1", 1, now.Add(-time.Second)))
	check(s.snapshot(&cursor), 1, true)
	check(s.snapshot(), 3, false) // /api/status remains self-contained.
	var newViewer outputHistoryCursor
	check(s.snapshot(&newViewer), 3, false)
	// A slow viewer missing the retained cursor receives a fresh snapshot.
	s.Output = []sourceOutputWindow{point("lan_1", 200, now.Add(40*time.Second))}
	check(s.snapshot(&cursor), 1, false)
	check(s.snapshot(&cursor), 0, true)
	s.Output = nil
	check(s.snapshot(&cursor), 0, false)
	s.Output = []sourceOutputWindow{point("slopify", 1, now)}
	check(s.snapshot(&cursor), 1, false)
}

func TestSourceHistoryBatchesEveryWindow(t *testing.T) {
	s := newFixedSources()
	start := time.Now().Add(-3 * time.Second)
	appendWindows := func(first, last uint64) {
		for seq := first; seq <= last; seq++ {
			s.Sources[0].history = append(s.Sources[0].history, audiometer.Window{Sequence: seq, At: start.Add(time.Duration(seq) * 200 * time.Millisecond), DB: -6})
		}
	}
	var cursor outputHistoryCursor
	check := func(cursor *outputHistoryCursor, count int, delta bool) {
		t.Helper()
		data, err := json.Marshal(s.snapshot(cursor))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Sources []struct {
				Windows      []audiometer.Window
				WindowsDelta bool
			}
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		windows := got.Sources[0].Windows
		if len(windows) != count || got.Sources[0].WindowsDelta != delta {
			t.Fatalf("windows %d delta=%v, want %d delta=%v", len(windows), got.Sources[0].WindowsDelta, count, delta)
		}
		for i := 1; i < len(windows); i++ {
			if windows[i].At.Sub(windows[i-1].At) != 200*time.Millisecond {
				t.Fatal("window dropped")
			}
		}
	}
	appendWindows(1, 5)
	check(&cursor, 5, false)
	appendWindows(6, 10) // One SSE interval contains five measured windows.
	check(&cursor, 5, true)
	check(&cursor, 0, true)
	check(&outputHistoryCursor{}, 10, false)        // Reconnect/new viewer gets history.
	s.Sources[0].history = s.Sources[0].history[:1] // Cursor no longer retained.
	check(&cursor, 1, false)
}
