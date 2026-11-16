package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/audiometer"
)

func TestStatusValueMatchesJSON(t *testing.T) {
	s := newFixedSources()
	s.Output = []sourceOutputWindow{{SourceID: "slopify", Window: audiometer.Window{Sequence: 1, At: time.Now(), Peak: 1 << 30, DB: -6}}}
	s.Sources[0].Metadata = map[string]string{"title": "Title", "artist": "Artist"}
	c := &controller{local: &localStatus{nativeSources: s}, sources: s}
	for _, value := range []any{c.localSnapshot(), s.snapshot(), map[string]any{"nil": (*int)(nil), "empty": []string{}, "nilSlice": []string(nil)}} {
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(statusValue(reflect.ValueOf(value)))
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		json.Unmarshal(want, &a)
		json.Unmarshal(got, &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("capture changed JSON:\n%s\n%s", want, got)
		}
	}
}

func TestStatusDiffLatchesAndDeletes(t *testing.T) {
	before := map[string]any{"now": "old", "sources": []any{map[string]any{"id": "slopify", "title": "same", "level": 1, "error": "gone"}}, "a/b~": true}
	after := map[string]any{"now": "new", "sources": []any{map[string]any{"id": "slopify", "title": "same", "level": 2}}, "a/b~": false}
	var changes [][]any
	statusDiff(before, after, "", &changes)
	got := map[string][]any{}
	for _, change := range changes {
		got[change[0].(string)] = change[1:]
	}
	want := map[string][]any{"/now": {"new"}, "/sources/0/level": {2}, "/sources/0/error": {}, "/a~1b~0": {false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patch: %#v", got)
	}
	changes = nil
	statusDiff(after, after, "", &changes)
	if len(changes) != 0 {
		t.Fatal("unchanged status produced a patch")
	}
	// Captured pointers must not change when a caller updates live state.
	peak := .25
	v := struct {
		Peak *float64 `json:"peak"`
	}{&peak}
	old := statusValue(reflect.ValueOf(v))
	peak = .75
	changes = nil
	statusDiff(old, statusValue(reflect.ValueOf(v)), "", &changes)
	if len(changes) != 1 {
		t.Fatal("pointer update was not detected")
	}
}
