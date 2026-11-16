package audiodec

import (
	"bytes"
	"testing"
)

func TestVorbisContinuesPastBadPage(t *testing.T) {
	if !vorbisEnabled {
		t.Skip("decoder disabled")
	}
	// Short pages, as oggenc writes; ffmpeg's default is about a second each.
	clean := fixture(t, "clean.ogg", 44100, "-c:a", "libvorbis", "-q:a", "4", "-page_duration", "200000")
	want := decodeAll(t, mustOpen(t, clean, nil))
	damaged := bytes.Clone(clean)
	// Corrupt audio in the middle of a page, so only that page's CRC fails.
	mid := bytes.Index(damaged[len(damaged)/2:], []byte("OggS")) + len(damaged)/2
	damaged[mid+400] ^= 0xFF
	var issues []Issue
	got := decodeAll(t, mustOpen(t, damaged, &issues))
	if len(issues) != 1 || issues[0].Kind != IssueResync || issues[0].At <= 0 {
		t.Fatalf("issues %v, want one resync", issues)
	}
	if len(got) >= len(want) || len(got) < len(want)*8/10 {
		t.Fatalf("decoded %d of %d samples: only the bad page may be lost", len(got), len(want))
	}
	before := int(issues[0].At*44100) * 2
	if !equalPrefix(got[:before], want) {
		t.Fatal("audio before the damage differs")
	}
}

func TestWAVTruncationReported(t *testing.T) {
	if !wavEnabled {
		t.Skip("decoder disabled")
	}
	clean := fixture(t, "cut.wav", 44100, "-c:a", "pcm_s16le")
	want := decodeAll(t, mustOpen(t, clean, nil))
	var issues []Issue
	got := decodeAll(t, mustOpen(t, clean[:len(clean)*7/10], &issues))
	if len(issues) != 1 || issues[0].Kind != IssueTruncated || issues[0].Missing <= 0 {
		t.Fatalf("issues %v, want one truncation", issues)
	}
	if len(got) >= len(want) || !equalPrefix(got, want) {
		t.Fatal("truncated WAV must decode what is present exactly")
	}
}

func equalPrefix(a, b []int32) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
