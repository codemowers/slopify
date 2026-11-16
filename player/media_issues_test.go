package main

import (
	"strings"
	"testing"
)

func TestMediaPathOmitsSignature(t *testing.T) {
	got := mediaPath("https://slopify.example/stream/original?exp=1793647045&format=original&path=audio%2FEesti%2F28.+Mida+keegi+kannab+%28k%29.flac&sig=c2VjcmV0")
	if got != "audio/Eesti/28. Mida keegi kannab (k).flac" {
		t.Fatalf("path %q", got)
	}
	if strings.Contains(got, "sig") || strings.Contains(got, "1793647045") {
		t.Fatalf("path %q carries signing parameters", got)
	}
	if mediaPath("%zz") != "" {
		t.Fatal("unparseable URL must give no path")
	}
}

func TestMetricCodecIsBounded(t *testing.T) {
	for in, want := range map[string]string{"FLAC": "FLAC", "flac": "FLAC", "Vorbis": "VORBIS", "opus": "other", "": "other"} {
		if got := metricCodec(in); got != want {
			t.Errorf("metricCodec(%q) = %q, want %q", in, got, want)
		}
	}
}
