package main

import (
	"testing"
	"time"
)

func TestFixedSourcesShowSlopifyCardAndOutput(t *testing.T) {
	l := &localStatus{nativeSources: newFixedSources()}
	l.decodedState("running")
	l.nativeFormats(localPCM{Codec: "FLAC", Format: "S16_LE", Rate: 44100, Channels: 2}, localPCM{Format: "S32_LE", Rate: 44100, Channels: 2}, map[string]string{"title": "x"})
	l.nativePeak("slopify", 1<<30, time.Now())
	s := l.nativeSources.Sources[0]
	if !s.Active || s.Format.Codec != "FLAC" || s.Metadata["title"] != "x" || s.meter.Snapshot().Peak != 1<<30 || len(l.nativeSources.Output) != 1 {
		t.Fatalf("%+v", s)
	}
	if l.nativeOutput.Kind != "Hardware output" || l.nativeOutput.Format.Format != "S32_LE" {
		t.Fatalf("%+v", l.nativeOutput)
	}
	l.decodedState("buffering")
	if s.Format.Codec != "FLAC" || s.Metadata["title"] != "x" || l.nativeOutput.Format.Format != "S32_LE" {
		t.Fatal("playback state replaced source metadata or output format")
	}
	l.decodedState("idle")
	if s.Active || l.nativeOutput.State != "idle" {
		t.Fatal("idle source still shown as active")
	}
}
