package main

import (
	"time"

	"github.com/codemowers/slopify/player/internal/esound"
	"github.com/codemowers/slopify/player/internal/pulse"
)

// liveStream is one LAN sender's PCM stream, whatever protocol carried it.
type liveStream interface {
	Protocol() string
	Peer() string
	Props() map[string]string
	PCM() livePCM
	Read([]byte) int
	Decode(dst []int32, raw []byte) int
	Done() <-chan struct{}
	Stats() pulse.Stats
	ObserveReceive(func(time.Duration))
	SetDeviceDelay(usec uint64)
	Volume() uint32
	SetVolume(uint32)
	Close()
}

type livePCM struct {
	name           string
	rate, channels int
	bytes, bits    int
}

func (p livePCM) local() *localPCM {
	return &localPCM{Format: p.name, Rate: p.rate, Channels: p.channels}
}

type pulseLive struct{ *pulse.Stream }

func (s pulseLive) Protocol() string { return "pulse" }
func (s pulseLive) Peer() string     { return s.Stream.Peer }
func (s pulseLive) Close()           {} // The sender ends its own stream.
func (s pulseLive) PCM() livePCM {
	p := livePCM{rate: s.Rate, channels: s.Channels, bytes: pulse.BytesPerSample(s.Format), bits: 32}
	switch s.Format {
	case pulse.FormatS16LE:
		p.name, p.bits = "S16_LE", 16
	case pulse.FormatS24LE:
		p.name, p.bits = "S24_3LE", 24
	case pulse.FormatS2432LE:
		p.name, p.bits = "S24_LE", 24
	case pulse.FormatS32LE:
		p.name = "S32_LE"
	case pulse.FormatFloat32LE:
		p.name = "FLOAT_LE"
	}
	return p
}
func (s pulseLive) Decode(dst []int32, raw []byte) int { return s.Stream.Decode(dst, raw) }

type esoundLive struct{ *esound.Stream }

func (s esoundLive) Protocol() string { return "esound" }
func (s esoundLive) Peer() string     { return s.Stream.Peer }
func (s esoundLive) Volume() uint32   { return 0 }
func (s esoundLive) SetVolume(uint32) {} // ESound has no stream volume to report back to.
func (s esoundLive) Props() map[string]string {
	name := s.Name
	if name == "" {
		name = "ESound sender"
	}
	return map[string]string{"application.name": name}
}
func (s esoundLive) PCM() livePCM {
	// An 8-bit stream widens to 16 bits, which every device format can hold.
	p := livePCM{rate: s.Rate, channels: s.Channels, bytes: esound.BytesPerSample(s.Format), bits: 16}
	p.name = map[int]string{esound.FormatU8: "U8", esound.FormatS16LE: "S16_LE", esound.FormatS16BE: "S16_BE"}[s.Format]
	return p
}
func (s esoundLive) Decode(dst []int32, raw []byte) int { return esound.DecodeS32(dst, raw, s.Format) }
func (s esoundLive) Stats() pulse.Stats {
	stats := s.Stream.Stats()
	return pulse.Stats{BufferedUsec: stats.BufferedUsec, TargetUsec: stats.TargetUsec, Underflows: stats.Underflows}
}
