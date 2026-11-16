package audiodec

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCodecBuildSelection(t *testing.T) {
	for _, tc := range []struct{ name, header string }{
		{"FLAC", "fLaC00000000"}, {"WAV", "RIFF0000WAVE"},
		{"Vorbis", "OggS00000000"}, {"MP3", "\xff\xfb\x90\x0000000000"},
	} {
		if Enabled(tc.name) {
			continue
		}
		_, err := Open(bytes.NewReader([]byte(tc.header)))
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "disabled at build time") {
			t.Fatalf("disabled %s: %v", tc.name, err)
		}
		if tc.name == "WAV" || tc.name == "FLAC" {
			_, err = OpenStream(bytes.NewReader([]byte(tc.header)), "application/octet-stream")
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("disabled stream %s: %v", tc.name, err)
			}
		}
	}
	if !Enabled("ALAC") {
		if _, err := NewALAC(ALACConfig{}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("disabled ALAC: %v", err)
		}
	}
	// Disabling file codecs must not remove raw PCM reception.
	d, err := OpenStream(bytes.NewReader([]byte{0x12, 0x34}), "audio/L16;rate=44100;channels=1")
	if err != nil {
		t.Fatal(err)
	}
	var sample [1]int32
	if n, err := d.Read(sample[:]); n != 1 || err != nil || sample[0] != 0x12340000 {
		t.Fatalf("PCM: %d %v %#x", n, err, sample[0])
	}
}
