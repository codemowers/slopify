package main

import (
	"testing"
	"time"
)

func TestFixedSourcesShowSlopifyCardAndOutput(t *testing.T) {
	l := &localStatus{nativeSources: newFixedSources()}
	l.decodedState(format{rate: 44100, channels: 2, bits: 16}, "running")
	l.nativeFormats(localPCM{Codec: "FLAC", Format: "S16_LE", Rate: 44100, Channels: 2}, localPCM{Format: "S32_LE", Rate: 44100, Channels: 2}, map[string]string{"title": "x"})
	l.nativePeak("slopify", 0.5, time.Now())
	s := l.nativeSources.Sources[0]
	if !s.Active || s.Format.Codec != "FLAC" || s.Metadata["title"] != "x" || s.meter.Snapshot().Peak != 0.5 || len(l.nativeSources.Output) != 1 {
		t.Fatalf("%+v", s)
	}
	if l.nativeOutput.Kind != "Hardware output" || l.nativeOutput.Format.Format != "S32_LE" {
		t.Fatalf("%+v", l.nativeOutput)
	}
	l.decodedState(format{}, "idle")
	if s.Active || l.nativeOutput.State != "idle" {
		t.Fatal("idle source still shown as active")
	}
}
