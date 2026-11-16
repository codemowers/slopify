package linuxioctl

import (
	"runtime"
	"strings"
	"testing"
)

func TestKernelRequestNumbers(t *testing.T) {
	mips := strings.HasPrefix(runtime.GOARCH, "mips")
	want := [4]uintptr{0x00004140, 0x400c4150, 0x80044121, 0xc25c4110}
	if mips {
		want = [4]uintptr{0x20004140, 0x800c4150, 0x40044121, 0xc25c4110}
	}
	got := [4]uintptr{Request(0, 'A', 0x40, 0), Request(1, 'A', 0x50, 12), Request(2, 'A', 0x21, 4), Request(3, 'A', 0x10, 604)}
	if got != want {
		t.Fatalf("%s ioctl encodings: %#x, want %#x", runtime.GOARCH, got, want)
	}
}
