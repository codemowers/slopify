package main

import "testing"

func TestPlaybackStatusBeforeFormatAndAfterFailure(t *testing.T) {
	l := &localStatus{}
	l.decodedState(format{}, "starting-mpv")
	if l.playback.State != "starting-mpv" || l.playback.Since.IsZero() {
		t.Fatal("startup without format was lost")
	}
	since := l.playback.Since
	l.decodedState(format{}, "starting-mpv")
	if l.playback.Since != since {
		t.Fatal("repeated state reset elapsed time")
	}
	l.decodedState(format{rate: 44100, bits: 32, channels: 2}, "running")
	l.decodedState(format{}, "error")
	if l.playback.State != "error" || l.decoded.State != "error" {
		t.Fatal("failed session left running status")
	}
}

func TestStartupGuardEndsOnPlaybackOrFailure(t *testing.T) {
	for _, state := range []string{"starting", "starting-mpv", "loading", "buffering"} {
		if !playbackStarting(state) {
			t.Fatalf("unguarded startup %s", state)
		}
	}
	for _, state := range []string{"", "running", "idle", "error"} {
		if playbackStarting(state) {
			t.Fatalf("startup guard retained for %s", state)
		}
	}
}
