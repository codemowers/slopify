package main

import "testing"

func TestPlaybackStatusBeforeFormatAndAfterFailure(t *testing.T) {
	l := &localStatus{}
	l.decodedState("loading")
	if l.playback.State != "loading" || l.playback.Since.IsZero() {
		t.Fatal("startup without format was lost")
	}
	since := l.playback.Since
	l.decodedState("loading")
	if l.playback.Since != since {
		t.Fatal("repeated state reset elapsed time")
	}
	l.nativeFormats(localPCM{Format: "S32_LE", Rate: 44100, Channels: 2}, localPCM{Format: "S32_LE", Rate: 44100, Channels: 2}, nil)
	l.decodedState("running")
	l.decodedState("error")
	if l.playback.State != "error" || l.decoded.State != "error" {
		t.Fatal("failed session left running status")
	}
}

func TestStartupGuardEndsOnPlaybackOrFailure(t *testing.T) {
	for _, state := range []string{"starting", "loading", "buffering"} {
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
