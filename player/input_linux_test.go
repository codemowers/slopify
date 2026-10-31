//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestKnobInputIgnoresReleaseRepeatAndUnrelatedKeys(t *testing.T) {
	for _, tc := range []struct {
		typ, code  uint16
		value      int32
		button, ok bool
		delta      int
	}{
		{2, 0, 1, false, true, 1}, {2, 0, -1, false, true, -1}, {2, 0, 0, false, false, 0},
		{1, 164, 1, true, true, 0}, {1, 164, 0, true, false, 0}, {1, 164, 2, true, false, 0},
		{1, 30, 1, true, false, 0}, {1, 164, 1, false, false, 0},
	} {
		b := make([]byte, 8)
		binary.NativeEndian.PutUint16(b, tc.typ)
		binary.NativeEndian.PutUint16(b[2:], tc.code)
		binary.NativeEndian.PutUint32(b[4:], uint32(tc.value))
		device := &inputDevice{rotary: true}
		if tc.button {
			device = &inputDevice{keys: map[uint16]inputAction{keyPlayPause: actionPlayPause}}
		}
		e, ok := decodeKnobEvent(b, device)
		if ok != tc.ok || ok && (e.delta != tc.delta || (e.action == actionPlayPause) != tc.button) {
			t.Fatalf("event %+v => %+v %v", tc, e, ok)
		}
	}
}

func TestInputPathFindsKernelNames(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "event7", "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "name"), []byte("rotary@17\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for device, want := range map[string]string{"rotary@17": "/dev/input/event7", "/dev/x": "/dev/x", "absent": ""} {
		if got := inputPath(root, device); got != want {
			t.Fatalf("%s: %q", device, got)
		}
	}
}
