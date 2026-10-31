//go:build !linux

// Package alsa is available on Linux only.
package alsa

import "errors"

const (
	FormatS16LE  = 2
	FormatS24LE  = 6
	FormatS32LE  = 10
	FormatS243LE = 32
)

type Config struct {
	Format, Rate, Channels, PeriodFrames, BufferFrames int
}

type PCM struct{}

type Device struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func Describe(string) (Device, error) { return Device{}, errUnavailable }

var errUnavailable = errors.New("ALSA is available on Linux only")

func Open(string, bool, int, int, int) (*PCM, error)     { return nil, errUnavailable }
func OpenLowLatency(string, int, int, int) (*PCM, error) { return nil, errUnavailable }
func (*PCM) Config() Config                              { return Config{} }
func (*PCM) WriteFrames([]byte) (int, error)             { return 0, errUnavailable }
func (*PCM) ReadFrames([]byte) (int, error)              { return 0, errUnavailable }
func (*PCM) Delay() int                                  { return 0 }
func (*PCM) Drain() error                                { return errUnavailable }
func (*PCM) Drop() error                                 { return errUnavailable }
func (*PCM) Close() error                                { return nil }

type Scale struct {
	Min, Step int
	Mute      bool
}

type IntegerControl struct {
	Min, Max, Channels int
	Scale              *Scale
}

type NamedControl struct {
	Name     string `json:"name"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Channels int    `json:"channels"`
	DB       bool   `json:"db"`
}

type Card struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Controls []NamedControl `json:"controls"`
}

func Cards() ([]Card, error) { return nil, errUnavailable }

type Mixer struct{}

func OpenMixer(string) (*Mixer, error)                { return nil, errUnavailable }
func (*Mixer) Integer(string) (IntegerControl, error) { return IntegerControl{}, errUnavailable }
func (*Mixer) Values(string) ([]int, error)           { return nil, errUnavailable }
func (*Mixer) SetValue(string, int) error             { return errUnavailable }
func (*Mixer) Switches(string) ([]int, error)         { return nil, errUnavailable }
func (*Mixer) SetSwitch(string, bool) error           { return errUnavailable }
