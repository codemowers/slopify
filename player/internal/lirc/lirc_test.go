package lirc

import (
	"bytes"
	"testing"
)

func TestNECXFrameMatchesKernelLayout(t *testing.T) {
	want := []byte{
		0, 0, 0, 0, 0, 0, 0, 0, // timestamp
		0, 0, // flags
		10, 0, // RC_PROTO_NECX
		0, 0, 0, 0, // keycode
		0x88, 0x7c, 0x87, 0, 0, 0, 0, 0, // scancode 0x877c88
	}
	if got := necxFrame(0x877c, 0x88); !bytes.Equal(got, want) {
		t.Fatalf("%x", got)
	}
}
