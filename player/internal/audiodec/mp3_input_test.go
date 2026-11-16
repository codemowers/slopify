//go:build !no_mp3

package audiodec

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestMP3EncodedInputRateExcludesPrefetch(t *testing.T) {
	data := fixture(t, "320.mp3", 44100, "-c:a", "libmp3lame", "-b:a", "320k")
	d, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	input := d.(EncodedInput)
	if b, f := input.InputTotals(); b != 0 || f != 0 {
		t.Fatal("opening/prefetch counted as decoded audio")
	}
	buf := make([]int32, 8192)
	if _, err := d.Read(buf); err != nil {
		t.Fatal(err)
	}
	if b, f := input.InputTotals(); b == 0 || b >= 8192 || f == 0 {
		t.Fatalf("frame accounting includes reader buffering: %d bytes %d frames", b, f)
	}
	for {
		_, err := d.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	b, f := input.InputTotals()
	bps := b * 8 * uint64(d.Format().Rate) / f
	if bps < 319900 || bps > 320100 {
		t.Fatalf("320 kb/s MP3 measured %d bits/s", bps)
	}
}
