//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIRKeymapValidation(t *testing.T) {
	got, err := irKeymap("rc5", map[string]string{"playpause": "0x0a0c", "volume_up": "2577", "volume_down": "0x0a10"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`protocol = "rc5"`, `0xa0c = "KEY_PLAYPAUSE"`, `0xa11 = "KEY_VOLUMEUP"`, `0xa10 = "KEY_VOLUMEDOWN"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	for _, tc := range []struct {
		protocol string
		codes    map[string]string
	}{
		{"shell", map[string]string{"mute": "1"}},
		{"rc5", map[string]string{"mute": "0xfffffffff"}},
		{"rc5", map[string]string{"mute": "1\nmalicious"}},
		{"rc5", map[string]string{"mute": "+1"}},
		{"rc5", map[string]string{"mute": "1", "next": "0x1"}},
	} {
		if _, err := irKeymap(tc.protocol, tc.codes); err == nil {
			t.Fatalf("accepted %#v", tc)
		}
	}
	if _, err := irKeymap("nec", map[string]string{"mute": "0"}); err != nil {
		t.Fatal("zero is a valid scancode", err)
	}
}
func TestIRReceiverIdentitySurvivesProbeOrder(t *testing.T) {
	root := t.TempDir()
	class := filepath.Join(root, "class")
	physical := filepath.Join(root, "devices", "platform", "gpio-ir")
	if err := os.MkdirAll(class, 0755); err != nil {
		t.Fatal(err)
	}
	makeReceiver := func(id, event string) {
		dir := filepath.Join(physical, "rc", id)
		input := filepath.Join(dir, "input0")
		if err := os.MkdirAll(filepath.Join(input, event), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(input, "name"), []byte("gpio_ir_recv\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "protocols"), []byte("[rc-5] nec rc-6\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(class, id)); err != nil {
			t.Fatal(err)
		}
	}
	makeReceiver("rc0", "event3")
	first := irReceivers(class)
	if len(first) != 1 || first[0].Input != "/dev/input/event3" || first[0].Name != "gpio_ir_recv" {
		t.Fatalf("%+v", first)
	}
	if err := os.Remove(filepath.Join(class, "rc0")); err != nil {
		t.Fatal(err)
	}
	makeReceiver("rc7", "event9")
	next := irReceivers(class)
	if len(next) != 1 || first[0].Key != next[0].Key || next[0].Input != "/dev/input/event9" {
		t.Fatalf("identity changed: %+v -> %+v", first, next)
	}
}
func TestIRCarrierDoesNotPretendToTuneFixedHardware(t *testing.T) {
	r := irReceiver{LIRC: "/nonexistent-lirc-test-device"}
	if err := irCarrier(r, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := irCarrier(r, 36000, 38000); err == nil || !strings.Contains(err.Error(), "hardware-fixed") {
		t.Fatalf("unexpected error: %v", err)
	}
	r.Carrier = true
	if err := irCarrier(r, 36000, 38000); err == nil || !strings.Contains(err.Error(), "hardware-fixed") {
		t.Fatalf("accepted range without capability: %v", err)
	}
}

func TestIRBPFKeymapRangesAndToggle(t *testing.T) {
	for _, tc := range []struct{ protocol, code, other string }{
		{"recs80", "0x12", "0x212"},
		{"recs80ext", "0x812", "0xc12"},
	} {
		got, err := irKeymap(tc.protocol, map[string]string{"mute": tc.code})
		if err != nil {
			t.Fatal(err)
		}
		for _, code := range []string{tc.code, tc.other} {
			if !strings.Contains(string(got), code+` = "KEY_MUTE"`) {
				t.Fatalf("missing toggle variant %s: %s", code, got)
			}
		}
		if _, err := irKeymap(tc.protocol, map[string]string{"mute": tc.code, "next": tc.other}); err == nil {
			t.Fatal("accepted overlapping toggle variants")
		}
	}
	got, err := irKeymap("kaseikyo-denon", map[string]string{"mute": "0x110332203254"})
	if err != nil || !strings.Contains(string(got), `0x110332203254 = "KEY_MUTE"`) {
		t.Fatalf("48-bit code: %s %v", got, err)
	}
	for _, tc := range []struct{ protocol, code string }{
		{"recs80", "0x400"}, {"recs80ext", "0x1000"}, {"kaseikyo-denon", "0x1000000000000"},
	} {
		if _, err := irKeymap(tc.protocol, map[string]string{"mute": tc.code}); err == nil {
			t.Fatalf("accepted oversized %s code", tc.protocol)
		}
	}
}

func TestIRLearnScancodeProtocolAndToggle(t *testing.T) {
	for _, tc := range []struct {
		protocol   string
		flags, rc  uint16
		scan, want uint64
		ok         bool
	}{
		{"recs80", 0, 64, 0x323, 0x123, true},
		{"recs80", 2, 64, 0x323, 0, false},
		{"recs80", 0, 9, 0x123, 0, false},
		{"recs80ext", 0, 64, 0xc12, 0x812, true},
		{"kaseikyo-denon", 0, 64, 0x110332203254, 0x110332203254, true},
		{"kaseikyo-denon", 0, 64, 0x100332203254, 0, false},
		{"nec", 0, 9, 0x123, 0x123, true},
	} {
		got, ok := learnedIRScancode(tc.protocol, tc.flags, tc.rc, tc.scan)
		if ok != tc.ok || ok && got != tc.want {
			t.Fatalf("%+v => %x, %v", tc, got, ok)
		}
	}
}
