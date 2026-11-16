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

// Reading every event node must not turn mouse motion into volume changes.
func TestMediaOnlyInputIgnoresRelativeMotion(t *testing.T) {
	devices, err := inputDevices([]string{"/dev/input/event2"})
	if err != nil {
		t.Fatal(err)
	}
	event := make([]byte, 8)
	binary.NativeEndian.PutUint16(event, 2)
	binary.NativeEndian.PutUint32(event[4:], 1)
	if _, ok := decodeKnobEvent(event, devices["/dev/input/event2"]); ok {
		t.Fatal("relative motion on a media-only device changed volume")
	}
	devices["/dev/input/event2"].rotary = true
	if got, ok := decodeKnobEvent(event, devices["/dev/input/event2"]); !ok || got.delta != 1 {
		t.Fatalf("selected rotary: %+v, %v", got, ok)
	}
}

func TestMediaVolumeRepeatsButTransportDoesNot(t *testing.T) {
	device := &inputDevice{keys: map[uint16]inputAction{keyVolumeUp: actionVolumeUp, keyVolumeDown: actionVolumeDown, keyPlayPause: actionPlayPause}}
	for _, tc := range []struct {
		key   uint16
		value uint32
		delta int
		ok    bool
	}{{keyVolumeUp, 1, 1, true}, {keyVolumeUp, 2, 1, true}, {keyVolumeDown, 2, -1, true}, {keyVolumeDown, 0, 0, false}, {keyPlayPause, 2, 0, false}} {
		b := make([]byte, 8)
		binary.NativeEndian.PutUint16(b, 1)
		binary.NativeEndian.PutUint16(b[2:], tc.key)
		binary.NativeEndian.PutUint32(b[4:], tc.value)
		got, ok := decodeKnobEvent(b, device)
		if ok != tc.ok || got.delta != tc.delta {
			t.Fatalf("%+v -> %+v %v", tc, got, ok)
		}
	}
}
