package main

import (
	"errors"
	"testing"

	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/pulse"
)

func TestPulseSinkFormat(t *testing.T) {
	for _, c := range []struct {
		name    string
		device  string
		formats []int
		err     error
		want    int
	}{
		{"null output", "", nil, nil, pulse.FormatS32LE},
		{"32-bit DAC", "hw:0,0", []int{alsa.FormatS16LE, alsa.FormatS32LE}, nil, pulse.FormatS32LE},
		{"24-bit DAC", "hw:0,0", []int{alsa.FormatS16LE, alsa.FormatS243LE}, nil, pulse.FormatS24LE},
		{"16-bit DAC", "hw:0,0", []int{alsa.FormatS16LE}, nil, pulse.FormatS16LE},
		{"busy device", "hw:0,0", nil, errors.New("busy"), pulse.FormatS32LE},
		{"no known format", "hw:0,0", nil, nil, pulse.FormatS32LE},
	} {
		got := pulseSinkFormat(c.device, func(string) ([]int, error) { return c.formats, c.err })
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, pulse.FormatName(got), pulse.FormatName(c.want))
		}
	}
}
