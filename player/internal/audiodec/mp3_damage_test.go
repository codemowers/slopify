//go:build !no_mp3

package audiodec

import (
	"testing"
)

func TestMP3CRCCheckValue(t *testing.T) {
	// CRC-16 with polynomial 0x8005, initial 0xFFFF, MSB first (CRC-16/CMS).
	if got := mp3CRC(0xFFFF, []byte("123456789")); got != 0xAEE7 {
		t.Fatalf("crc %#04x, want 0xaee7", got)
	}
}

// protectedSilence builds MPEG-1 Layer III mono 44.1 kHz 128 kbit/s frames with
// CRC protection and all-zero side information, which decode to silence.
func protectedSilence(frames int, badCRC func(int) bool) []byte {
	const size = 417 // 144 * 128000 / 44100
	var out []byte
	for i := range frames {
		f := make([]byte, size)
		f[0], f[1], f[2], f[3] = 0xFF, 0xFA, 0x90, 0xC0
		crc := mp3CRC(mp3CRC(0xFFFF, f[2:4]), f[6:6+17])
		if badCRC(i) {
			crc ^= 0x5A5A
		}
		f[4], f[5] = byte(crc>>8), byte(crc)
		out = append(out, f...)
	}
	return out
}

func TestMP3ConcealsFrameFailingCRC(t *testing.T) {
	clean := protectedSilence(40, func(int) bool { return false })
	want := len(decodeAll(t, mustOpen(t, clean, nil)))
	var issues []Issue
	got := decodeAll(t, mustOpen(t, protectedSilence(40, func(i int) bool { return i == 20 || i == 21 }), &issues))
	if len(got) != want {
		t.Fatalf("decoded %d samples, want %d: concealment keeps the length", len(got), want)
	}
	if len(issues) != 1 || issues[0].Kind != IssueChecksum || issues[0].Offset != 20*417 || issues[0].Bytes != 2*417 {
		t.Fatalf("issues %v, want one checksum issue over frames 20 and 21", issues)
	}
	if missing := issues[0].Missing * 44100; missing < 2*1152-1 || missing > 2*1152+1 {
		t.Fatalf("missing %.0f samples, want two frames", missing)
	}
}

// An encoder that writes wrong CRCs everywhere must not silence the whole file.
func TestMP3IgnoresCRCsThatNeverMatch(t *testing.T) {
	var issues []Issue
	d := mustOpen(t, protectedSilence(40, func(int) bool { return true }), &issues)
	decodeAll(t, d)
	if len(issues) != 0 || !d.(*mp3Decoder).crcIgnored {
		t.Fatalf("issues %v, ignored %v: invalid encoder CRCs must be ignored", issues, d.(*mp3Decoder).crcIgnored)
	}
}

// A bad page is dropped and decoding goes on; the length lost is unknown.
