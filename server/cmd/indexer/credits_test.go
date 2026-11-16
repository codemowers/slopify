package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/codemowers/slopify/server/internal/sidecar"
)

func TestCreditsFoldFormatSpellings(t *testing.T) {
	const rec, rel, a1, a2 = "0a7d3f0e-0000-4000-8000-000000000001", "0a7d3f0e-0000-4000-8000-000000000002", "0a7d3f0e-0000-4000-8000-000000000003", "0a7d3f0e-0000-4000-8000-000000000004"
	// probe lowercases keys; spaces, underscores and hyphens still differ.
	got := credits(map[string]string{
		"musicbrainz_trackid":  strings.ToUpper(rec),
		"musicbrainz album id": rel,
		"musicbrainz_artistid": a1 + "; " + a2 + ";" + a1 + "; not-a-uuid",
		"musicbrainz work id":  "../../evil",
		"isrc":                 "us-rc1-76-07839",
		"license":              "https://creativecommons.org/licenses/by/4.0/",
		"publisher":            "XLR",
		"composer":             "  J.S. Bach\x00",
		"copyright":            strings.Repeat("é", maxCreditText),
		"acoustid_id":          "javascript:alert(1)",
		"comment":              "ignored",
	})
	want := &sidecar.Credits{
		Composer: "J.S. Bach", License: "https://creativecommons.org/licenses/by/4.0/", Label: "XLR",
		ISRC: "USRC17607839", Recording: rec, Release: rel, Artists: []string{a1, a2},
	}
	if got == nil || len(got.Copyright) > maxCreditText || !strings.HasSuffix(got.Copyright, "…") {
		t.Fatalf("copyright not truncated: %+v", got)
	}
	got.Copyright = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("credits:\n got %+v\nwant %+v", got, want)
	}
	if c := credits(map[string]string{"title": "x", "isrc": "bad"}); c != nil {
		t.Fatalf("expected no credits, got %+v", c)
	}
}

func TestTaggedFilesGauge(t *testing.T) {
	m := newIndexMetrics()
	entries := []sidecar.Entry{
		{Name: "a.flac", Tags: &sidecar.Info{Title: "A"}, Credits: &sidecar.Credits{Recording: "0a7d3f0e-0000-4000-8000-000000000001"}},
		{Name: "b.flac", Tags: &sidecar.Info{Title: "B"}},
		{Name: "c.mp3"},
	}
	m.remember("x", entries)
	for _, c := range []struct {
		mime, tag string
		want      float64
	}{{"audio/flac", "title", 2}, {"audio/flac", "musicbrainz_recording", 1}, {"audio/mpeg", "title", 0}} {
		if got := gauge(t, m, c.mime, c.tag); got != c.want {
			t.Fatalf("%s %s = %v, want %v", c.mime, c.tag, got, c.want)
		}
	}
	m.remember("x", entries[1:])
	m.forget("x")
	if got := gauge(t, m, "audio/flac", "title"); got != 0 {
		t.Fatalf("title after forget = %v", got)
	}
}

func gauge(t *testing.T, m *indexMetrics, mime, tag string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "slopify_indexer_tagged_files" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["mimetype"] == mime && labels["tag"] == tag {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}
