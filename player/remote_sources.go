package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/codemowers/slopify/player/internal/raop"
)

// Remote sources: while a LAN source whose sender accepts transport commands is
// selected, the speaker's play/pause, next and previous drive that sender instead of
// the Slopify queue. AirPlay implements this through DACP; Spotify or Chromecast
// sources would implement the same interface. Source selection and queue state stay
// separate, and nothing here pins sources.

// remoteSource is a live stream whose sender takes transport commands. Track text
// travels through Props as allowlisted media.* fields.
type remoteSource interface {
	liveStream
	Controllable() bool
	Control(ctx context.Context, op string) error
}

type remoteCommand struct {
	stream remoteSource
	op     string
}

// selectedRemote returns the selected source if its sender can be controlled.
func (c *controller) selectedRemote() (remoteSource, bool) {
	lan, s := c.nativeLAN, c.sources
	if lan == nil || s == nil {
		return nil, false
	}
	s.mu.Lock()
	key := ""
	if p := s.source(s.Selected); p != nil && p.ID != "slopify" {
		key = p.Key
	}
	s.mu.Unlock()
	if key == "" {
		return nil, false
	}
	lan.mu.Lock()
	consumer := lan.consumers[key]
	lan.mu.Unlock()
	if consumer == nil {
		return nil, false
	}
	stream, ok := consumer.stream.(remoteSource)
	return stream, ok && stream.Controllable()
}

// remoteButton hands a hardware button to the selected source's sender, if it takes one.
// One command waits at most; a button never blocks rotation or other input.
func (c *controller) remoteButton(op string) bool {
	stream, ok := c.selectedRemote()
	if !ok {
		return false
	}
	select {
	case c.remoteCommands <- remoteCommand{stream, op}:
	default:
	}
	return true
}

func (c *controller) runRemoteCommands(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case command := <-c.remoteCommands:
			call, cancel := context.WithTimeout(ctx, 4*time.Second)
			if err := command.stream.Control(call, command.op); err != nil {
				slog.Warn("sender remote control", "protocol", command.stream.Protocol(), "op", command.op, "err", err)
			}
			cancel()
		}
	}
}

// remoteTransport answers /api/playback for a controllable sender. Senders have no
// queue versions, so a command's version and entry are not checked; whether it is
// playing is inferred from recent sound, since sender state is not polled.
func (c *controller) remoteTransport(stream remoteSource) localTransport {
	meta := stream.Props()
	state := "paused"
	s := c.sources
	s.mu.Lock()
	if p := s.source(s.Selected); p != nil && time.Since(p.LastAudible) < 3*time.Second {
		state = "playing"
	}
	s.mu.Unlock()
	return localTransport{Available: true, Source: stream.Protocol(), State: state, Title: meta["media.title"], Artist: meta["media.artist"], Album: meta["media.album"]}
}

func (c *controller) remotePlayback(w http.ResponseWriter, r *http.Request, stream remoteSource, cmd *localPlaybackCommand) {
	if cmd == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(c.remoteTransport(stream))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if err := stream.Control(ctx, cmd.Op); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, raop.ErrNoRemote) {
			status = http.StatusConflict
		}
		http.Error(w, "Sender did not accept the command: "+err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
