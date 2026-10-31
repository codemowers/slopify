//go:build !linux

package main

import "errors"

func newMPVBackend(playbackServices, string, string) (playbackBackend, error) {
	return nil, errors.New("experimental mpv backend currently requires Linux")
}
