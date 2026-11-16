package audiodec

import (
	"bytes"
	"testing"
)

// Damage inside a FLAC stream costs only the frames it touches: they become silence
// of the same length, the rest decodes exactly, and the hole is reported.
func TestFLACResyncFillsDamageWithSilence(t *testing.T) {
	if !flacEnabled {
		t.Skip("decoder disabled")
	}
	clean := fixture(t, "clean.flac", 44100, "-sample_fmt", "s16")
	want := decodeAll(t, mustOpen(t, clean, nil))
	damaged := bytes.Clone(clean)
	hole := len(damaged) / 2
	clear(damaged[hole : hole+20000])
	var issues []Issue
	got := decodeAll(t, mustOpen(t, damaged, &issues))
	if len(got) != len(want) {
		t.Fatalf("decoded %d samples, want %d: damage must not change the length", len(got), len(want))
	}
	if len(issues) != 1 || issues[0].Kind != IssueResync || issues[0].Bytes < 20000 || issues[0].Missing <= 0 {
		t.Fatalf("issues %v, want one resync covering the hole", issues)
	}
	from := int(issues[0].At*44100) * 2
	to := from + int(issues[0].Missing*44100+0.5)*2
	for i := range got {
		inHole := i >= from && i < to
		if inHole && got[i] != 0 || !inHole && got[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d (hole %d..%d)", i, got[i], want[i], from, to)
		}
	}
	// Seeking past the hole lands exactly; seeking into it plays the rest as silence.
	for _, at := range []float64{0.5, issues[0].At + issues[0].Missing/2, issues[0].At + issues[0].Missing + 0.2} {
		d := mustOpen(t, damaged, nil)
		landed, err := d.Seek(at)
		if err != nil {
			t.Fatalf("seek %v: %v", at, err)
		}
		rest := decodeAll(t, d)
		start := int(landed*44100+0.5) * 2
		if len(rest) != len(got)-start {
			t.Fatalf("seek %v: %d samples remain, want %d", at, len(rest), len(got)-start)
		}
		for i := range rest {
			if rest[i] != got[start+i] {
				t.Fatalf("seek %v: sample %d differs", at, i)
			}
		}
	}
}

func TestFLACTruncationEndsTrack(t *testing.T) {
	if !flacEnabled {
		t.Skip("decoder disabled")
	}
	clean := fixture(t, "cut.flac", 44100, "-sample_fmt", "s16")
	want := decodeAll(t, mustOpen(t, clean, nil))
	var issues []Issue
	got := decodeAll(t, mustOpen(t, clean[:len(clean)*7/10], &issues))
	if len(got) == 0 || len(got) >= len(want) {
		t.Fatalf("decoded %d of %d samples", len(got), len(want))
	}
	if len(issues) != 1 || issues[0].Kind != IssueTruncated || issues[0].Missing <= 0 {
		t.Fatalf("issues %v, want one truncation", issues)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sample %d differs", i)
		}
	}
}

func mustOpen(t *testing.T, data []byte, issues *[]Issue) Decoder {
	t.Helper()
	var opts Options
	if issues != nil {
		opts.Report = func(i Issue) { *issues = append(*issues, i) }
	}
	d, err := OpenWith(bytes.NewReader(data), opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
