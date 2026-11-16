package main

import (
	"github.com/codemowers/slopify/player/internal/pcmring"
	"time"

	"github.com/codemowers/slopify/player/internal/esound"
	"github.com/codemowers/slopify/player/internal/pulse"
	"github.com/codemowers/slopify/player/internal/rtp"
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
	Stats() pcmring.Stats
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

type rtpLive struct{ *rtp.Stream }

func (s rtpLive) Protocol() string { return "rtp" }
func (s rtpLive) Peer() string     { return s.Stream.Peer }
func (s rtpLive) Volume() uint32   { return 0 }
func (s rtpLive) SetVolume(uint32) {} // Multicast has no way back to the sender.

func (s rtpLive) SetDeviceDelay(uint64) {}

// Props names the card after the group too: it is part of the source's identity.
func (s rtpLive) Props() map[string]string {
	meta := s.Metadata()
	return map[string]string{"application.name": s.Name + " · " + s.Group.String(), "media.title": meta.Title, "media.artist": meta.Artist, "media.album": meta.Album}
}
func (s rtpLive) PCM() livePCM {
	switch s.Encoding {
	case rtp.EncodingL24:
		return livePCM{name: "S24_3BE", rate: s.Rate, channels: s.Channels, bytes: 3, bits: 24}
	case rtp.EncodingL32:
		return livePCM{name: "S32_BE", rate: s.Rate, channels: s.Channels, bytes: 4, bits: 32}
	}
	return livePCM{name: "S16_BE", rate: s.Rate, channels: s.Channels, bytes: 2, bits: 16}
}
