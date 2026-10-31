package main

import (
	"fmt"
	"strconv"
	"strings"
)

type inputAction int

const (
	actionNone inputAction = iota
	actionPlayPause
	actionPrevious
	actionNext
	actionMute
)

// Linux input key codes used when a binding names only a device.
const (
	keyMute         = 113
	keyNextSong     = 163
	keyPlayPause    = 164
	keyPreviousSong = 165
	keyMax          = 0x2ff
)

// buttonOp is the server control for an action; mute stays on the device.
func (a inputAction) buttonOp() string {
	switch a {
	case actionPlayPause:
		return "toggle"
	case actionPrevious:
		return "prev"
	case actionNext:
		return "next"
	}
	return ""
}

type knobEvent struct {
	delta  int
	action inputAction
}

// inputDevice is what to read from one explicitly configured device.
type inputDevice struct {
	rotary bool
	keys   map[uint16]inputAction
}

func isKeyCode(s string) bool {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return false
	}
	return true
}

type inputBinding struct {
	flag, spec string
	action     inputAction
	key        uint16
}

// inputDevices merges bindings given as device[:keycode], so that one device can
// carry several buttons. A device is a /dev path or a kernel input device name;
// a name may itself contain a colon unless what follows the last one is a number. Two actions on the same key are a configuration error.
func inputDevices(rotary string, bindings []inputBinding) (map[string]*inputDevice, error) {
	devices := map[string]*inputDevice{}
	device := func(path string) *inputDevice {
		if devices[path] == nil {
			devices[path] = &inputDevice{keys: map[uint16]inputAction{}}
		}
		return devices[path]
	}
	if rotary != "" {
		device(rotary).rotary = true
	}
	for _, b := range bindings {
		if b.spec == "" {
			continue
		}
		path, key := b.spec, b.key
		if i := strings.LastIndexByte(b.spec, ':'); i >= 0 && isKeyCode(b.spec[i+1:]) {
			parsed, err := strconv.ParseUint(b.spec[i+1:], 0, 16)
			if err != nil || parsed == 0 || parsed > keyMax {
				return nil, fmt.Errorf("%s: invalid key code %q", b.flag, b.spec[i+1:])
			}
			path, key = b.spec[:i], uint16(parsed)
		}
		if path == "" {
			return nil, fmt.Errorf("%s: missing device", b.flag)
		}
		d := device(path)
		if _, taken := d.keys[key]; taken {
			return nil, fmt.Errorf("%s: key %d of %s already has an action", b.flag, key, path)
		}
		d.keys[key] = b.action
	}
	return devices, nil
}
