package cleantext

import "testing"

func TestLine(t *testing.T) {
	for in, want := range map[string]string{
		" a\x07\xffb\n ": "ab",
		"Ki\u200btchen":  "Kitch", // A zero-width space is dropped before the cap.
		"ääääää":         "äääää",
		"abcd e":         "abcd",
	} {
		if got := Line(in, 5); got != want {
			t.Fatalf("%q: %q, want %q", in, got, want)
		}
	}
}
