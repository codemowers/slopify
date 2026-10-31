package main

import "testing"

func TestInputDevicesMergeButtonsOnOneDevice(t *testing.T) {
	devices, err := inputDevices("/dev/input/knob", []inputBinding{
		{"button-input", "/dev/input/knob", actionPlayPause, keyPlayPause},
		{"next-input", "/dev/input/keys", actionNext, keyNextSong},
		{"mute-input", "/dev/input/keys:30", actionMute, keyMute},
		{"previous-input", "", actionPrevious, keyPreviousSong},
	})
	if err != nil {
		t.Fatal(err)
	}
	knob, keys := devices["/dev/input/knob"], devices["/dev/input/keys"]
	if len(devices) != 2 || !knob.rotary || knob.keys[keyPlayPause] != actionPlayPause {
		t.Fatalf("%+v", devices)
	}
	if keys.rotary || keys.keys[keyNextSong] != actionNext || keys.keys[30] != actionMute {
		t.Fatalf("%+v", keys)
	}
}

func TestInputDevicesTakeKernelNames(t *testing.T) {
	devices, err := inputDevices("rotary@17", []inputBinding{
		{"mute-input", "Primax HP USB Keyboard Consumer Control:113", actionMute, keyPlayPause},
		{"next-input", "odd:name", actionNext, keyNextSong},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !devices["rotary@17"].rotary || devices["Primax HP USB Keyboard Consumer Control"].keys[keyMute] != actionMute || devices["odd:name"].keys[keyNextSong] != actionNext {
		t.Fatalf("%+v", devices)
	}
}

func TestInputDevicesRejectAmbiguousBindings(t *testing.T) {
	for name, b := range map[string][]inputBinding{
		"same key": {{"button-input", "/dev/a", actionPlayPause, keyPlayPause}, {"next-input", "/dev/a:164", actionNext, keyNextSong}},
		"bad code": {{"next-input", "/dev/a:99999", actionNext, keyNextSong}},
		"zero":     {{"next-input", "/dev/a:0", actionNext, keyNextSong}},
		"no path":  {{"next-input", ":164", actionNext, keyNextSong}},
	} {
		if _, err := inputDevices("", b); err == nil {
			t.Fatal(name)
		}
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
