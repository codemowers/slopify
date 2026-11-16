package main

import (
	"fmt"
	"strings"
)

type inputAction int

const (
	actionNone inputAction = iota
	actionPlayPause
	actionPrevious
	actionNext
	actionMute
	actionVolumeUp
	actionVolumeDown
	actionPlay
	actionPause
)

// Linux input key codes used when a binding names only a device.
const (
	keyVolumeDown   = 114
	keyVolumeUp     = 115
	keyMute         = 113
	keyNextSong     = 163
	keyPlayPause    = 164
	keyPreviousSong = 165
	keyPlay         = 207
	keyPlayCD       = 200
	keyPauseCD      = 201
	keyPause        = 119
)

// buttonOp is the server control for an action; mute stays on the device.
func (a inputAction) buttonOp() string {
	switch a {
	case actionPlay:
		return "play"
	case actionPause:
		return "pause"
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

// Selected event sources share fixed Linux media keys. Rotary movement is opt-in.
// No per-device key remapping belongs in the playback process.
func inputDevices(specs []string) (map[string]*inputDevice, error) {
	devices := map[string]*inputDevice{}
	for _, spec := range specs {
		if strings.TrimSpace(spec) == "" || len(spec) > 512 || strings.ContainsAny(spec, "\r\n\x00") {
			return nil, fmt.Errorf("invalid input device")
		}
		if devices[spec] != nil {
			continue
		}
		if len(devices) >= 128 {
			return nil, fmt.Errorf("at most 128 input devices can be selected")
		}
		devices[spec] = &inputDevice{keys: map[uint16]inputAction{
			keyPlayPause: actionPlayPause, keyPlay: actionPlay, keyPlayCD: actionPlay,
			keyPause: actionPause, keyPauseCD: actionPause,
			keyPreviousSong: actionPrevious, keyNextSong: actionNext, keyMute: actionMute,
			keyVolumeUp: actionVolumeUp, keyVolumeDown: actionVolumeDown,
		}}
	}
	return devices, nil
}
