package main

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/codemowers/slopify/internal/volume"
)

// senderVolumeRequest is a LAN sender's request to move the speaker's hardware level.
// Each protocol maps its own scale onto the configured range at apply time, when
// the mixer's current range is known. Pulse, AirPlay and future receivers such as
// Spotify or Chromecast share this one path.
type senderVolumeRequest struct {
	key   string
	level func(volume.Control) (level int, mute, unmute bool)
}

// requestSenderVolume coalesces to the latest request; the sender never waits on
// the mixer, so a slider drag cannot stall its protocol connection.
func (c *controller) requestSenderVolume(key string, level func(volume.Control) (int, bool, bool)) {
	if c.mixer == nil || c.senderVolumes == nil {
		return
	}
	c.senderVolumeAt.Store(time.Now().UnixNano())
	select {
	case <-c.senderVolumes:
	default:
	}
	select {
	case c.senderVolumes <- senderVolumeRequest{key, level}:
	default:
	}
}

// senderVolumeRecent holds back pushing the hardware level to a sender while that
// sender is still moving it, so an echo never fights a drag.
func (c *controller) senderVolumeRecent() bool {
	return time.Since(time.Unix(0, c.senderVolumeAt.Load())) < time.Second
}

// runSenderVolumes applies at most one sender level every 100 ms, and only for the
// sender connected to the amplifier.
func (c *controller) runSenderVolumes(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var request senderVolumeRequest
		select {
		case <-ctx.Done():
			return
		case request = <-c.senderVolumes:
		}
		s := c.sources
		s.mu.Lock()
		selected := false
		if p := s.source(s.Selected); p != nil {
			selected = p.Key == request.key
		}
		s.mu.Unlock()
		if selected {
			c.applySenderVolume(ctx, request)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *controller) applySenderVolume(ctx context.Context, request senderVolumeRequest) {
	m := c.mixer
	level, mute, unmute := request.level(m.snapshot())
	m.mu.Lock()
	hasSwitch := m.switchAvailable
	m.mu.Unlock()
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var err error
	if mute && hasSwitch {
		err = m.setMute(call, true)
	} else if err = m.setLevel(call, level); err == nil && unmute && hasSwitch {
		err = m.setMute(call, false)
	}
	if err != nil {
		slog.Warn("sender volume", "err", err)
		return
	}
	c.reportVolume(ctx)
}

// pulseSenderLevel maps a Pulse stream volume; Pulse senders mute themselves.
func pulseSenderLevel(pulseVolume uint32) func(volume.Control) (int, bool, bool) {
	return func(control volume.Control) (int, bool, bool) { return senderLevel(pulseVolume, control), false, false }
}

// dbSenderLevel maps AirPlay's -30..0 dB onto the range, with -144 as mute: native
// mute where there is a playback switch, the minimum level otherwise.
func dbSenderLevel(db float64) func(volume.Control) (int, bool, bool) {
	return func(control volume.Control) (int, bool, bool) {
		if db == -144 {
			return control.Min, true, false
		}
		return control.Min + int(math.Round((db+30)/30*float64(control.Max-control.Min))), false, true
	}
}
