package main

import "testing"

func TestInputDevicesFixedMediaBindings(t *testing.T) {
	devices, err := inputDevices([]string{"rotary@17", "USB Keyboard Consumer Control", "rotary@17"})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatal(devices)
	}
	for _, d := range devices {
		if d.rotary || d.keys[keyVolumeUp] != actionVolumeUp || d.keys[keyPlayPause] != actionPlayPause || d.keys[30] != actionNone || d.keys[keyPlay] != actionPlay || d.keys[keyPauseCD] != actionPause {
			t.Fatal(d)
		}
	}
	if _, err := inputDevices([]string{"\n"}); err == nil {
		t.Fatal("accepted invalid input")
	}
}

func TestWithoutServerReleasesOnlyAPinOnSlopify(t *testing.T) {
	s := newSpeakerSources("")
	s.withoutServer()
	if s.Mode != "recent" {
		t.Fatalf("mode %q", s.Mode)
	}
	s = newSpeakerSources("")
	s.Selected = "lan_1"
	s.withoutServer()
	if s.Mode != "off" || s.Selected != "lan_1" {
		t.Fatalf("pin on a LAN source changed: %q %q", s.Mode, s.Selected)
	}
}
