//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strings"
	"time"
)

type mpvBackend struct {
	playbackServices
	binary    string
	args, env []string
	client    *mpvClient // sessions are serialized by controller
}

func newMPVBackend(services playbackServices, binary, output string) (playbackBackend, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("mpv executable unavailable: %w", err)
	}
	args := []string{"--no-config", "--idle=yes", "--terminal=no", "--msg-level=all=no", "--input-terminal=no", "--load-scripts=no", "--ytdl=no", "--vid=no", "--sid=no", "--aid=1", "--audio-display=no", "--audio-client-name=Slopify", "--audio-format=s32", "--audio-channels=auto", "--audio-samplerate=0", "--volume=100", "--replaygain=no", "--gapless-audio=weak", "--audio-fallback-to-null=no", "--resume-playback=no", "--save-position-on-quit=no"}
	kind, target, _ := strings.Cut(output, ":")
	b := &mpvBackend{playbackServices: services, binary: path}
	switch kind {
	case "aplay", "alsa":
		if target == "" {
			target = "hw:0,0"
		}
		if !strings.HasPrefix(target, "hw:") {
			return nil, errors.New("mpv ALSA output requires a hw: device")
		}
		args = append(args, "--ao=alsa", "--audio-device=alsa/"+target, "--alsa-resample=no")
	case "null":
		args = append(args, "--ao=null")
	default:
		return nil, fmt.Errorf("unsupported mpv output %q", kind)
	}
	b.args = args
	return b, nil
}

func (b *mpvBackend) Close() {
	if b.client != nil {
		b.client.close()
		b.client = nil
	}
}

func (b *mpvBackend) Start(id uint64, at float64) playbackSession {
	b.decodedState(format{}, "loading")
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{cancel: cancel, done: make(chan struct{}), first: id, startedAt: time.Now()}
	s.pos.Store(math.Float64bits(at))
	go func() {
		defer close(s.done)
		defer cancel()
		if err := b.play(ctx, s, id, at); err != nil && ctx.Err() == nil {
			s.failed.Store(true)
			b.decodedState(format{}, "error")
			slog.Warn("mpv playback failed", "entry", s.current(), "err", err)
			b.Close()
		}
	}()
	return s
}

type mpvPlaylistEntry struct {
	ID      int64 `json:"id"`
	Current bool  `json:"current"`
}

// No signed URLs appear in command-line arguments, events sent to the cloud,
// or logs. Only the current and one upcoming signed original are handed to mpv.
func (b *mpvBackend) play(ctx context.Context, s *session, id uint64, at float64) error {
	defer b.decodedState(format{}, "idle")
	startupAt := time.Now()
	awaitingAudio := true
	if b.client != nil {
		select {
		case <-b.client.done:
			b.Close()
		default:
		}
	}
	if b.client == nil {
		b.decodedState(format{}, "starting-mpv")
		var err error
		b.client, err = startMPV(b.binary, b.args, b.env)
		if err != nil {
			return err
		}
	}
	p := b.client
	// Previous session events cannot be interpreted as new queue completion.
	for len(p.events) > 0 {
		<-p.events
	}
	call := func(cmd ...any) error { _, err := p.call(ctx, cmd...); return err }
	var output format
	var cachePaused bool
	defer func() {
		b.decodedState(output, "idle")
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := p.call(stopCtx, "stop"); err != nil {
			b.Close()
		}
	}()
	for i, prop := range []string{"time-pos", "duration", "paused-for-cache"} {
		if err := call("observe_property", i+1, prop); err != nil {
			return err
		}
	}
	if err := call("set_property", "start", fmt.Sprintf("%.6f", at)); err != nil {
		return err
	}
	b.decodedState(format{}, "loading")
	u, err := b.original(ctx, id)
	if err != nil {
		return errors.New("cannot obtain original for mpv")
	}
	if err := call("loadfile", u, "replace"); err != nil {
		return err
	}
	b.decodedState(format{}, "buffering")
	entries := map[int64]uint64{}
	register := func(track uint64) error {
		data, err := p.call(ctx, "get_property", "playlist")
		if err != nil {
			return err
		}
		var list []mpvPlaylistEntry
		if json.Unmarshal(data, &list) != nil || len(list) == 0 || len(list) > 3 {
			return errors.New("unexpected mpv playlist")
		}
		entries[list[len(list)-1].ID] = track
		return nil
	}
	if err := register(id); err != nil {
		return err
	}
	var next uint64
	type upcoming struct {
		id  uint64
		url string
		err error
	}
	upcomingURL := make(chan upcoming, 1)
	fetching := false
	var nextRetry time.Time
	var duration float64
	loaded := false
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.done:
			return errors.New("mpv process or IPC exited unexpectedly")
		case result := <-upcomingURL:
			fetching = false
			wanted, _ := b.queue.after(id)
			if result.id != wanted || next != 0 {
				continue
			}
			if result.err != nil {
				nextRetry = time.Now().Add(5 * time.Second)
				continue // signing a successor must not stop the current track
			}
			if err := call("playlist-clear"); err != nil {
				return err
			}
			if err := call("loadfile", result.url, "append"); err != nil {
				return err
			}
			if err := register(result.id); err != nil {
				return err
			}
			next = result.id
		case m := <-p.events:
			switch m.Event {
			case "start-file":
				track, ok := entries[m.EntryID]
				if !ok {
					return errors.New("mpv started an unregistered queue entry")
				}
				if !b.queue.contains(track) {
					return errors.New("mpv upcoming entry was removed")
				}
				if track != id {
					startupAt = time.Now()
					awaitingAudio = true
					s.pos.Store(0)
					next = 0
					duration = 0
					output = format{}
				}
				id = track
				s.cur.Store(track)
				loaded = false
				b.decodedState(format{}, "buffering")
			case "file-loaded":
				loaded = true
				// Start is a per-load seek, not an offset for following tracks.
				if err := call("set_property", "start", "none"); err != nil {
					return err
				}
			case "property-change":
				switch m.Name {
				case "time-pos":
					var pos float64
					if string(m.Data) != "null" && json.Unmarshal(m.Data, &pos) == nil && pos >= 0 {
						s.pos.Store(math.Float64bits(pos))
					}
				case "duration":
					json.Unmarshal(m.Data, &duration)
				case "paused-for-cache":
					if json.Unmarshal(m.Data, &cachePaused) == nil && output.rate > 0 {
						if cachePaused {
							b.decodedState(output, "buffering")
						} else {
							b.decodedState(output, "running")
						}
					}
				}
			case "playback-restart":
				in, err := p.call(ctx, "get_property", "audio-params")
				if err != nil {
					continue
				} // teardown has no format
				out, err := p.call(ctx, "get_property", "audio-out-params")
				if err != nil {
					continue
				}
				source := mpvFormat(in)
				output = mpvFormat(out)
				if source.rate > 0 && output.rate > 0 {
					if err := validateMPVFormat(source, output); err != nil {
						return err
					}
					if cachePaused {
						b.decodedState(output, "buffering")
					} else {
						b.decodedState(output, "running")
					}
					if awaitingAudio {
						slog.Info("mpv audio ready", "entry", id, "startup_ms", time.Since(startupAt).Milliseconds())
						awaitingAudio = false
					}
				}
			case "end-file":
				track, ok := entries[m.EntryID]
				if !ok {
					continue
				}
				slog.Info("mpv track ended", "entry", track, "reason", m.Reason)
				if m.Reason != "eof" {
					return fmt.Errorf("mpv ended playback: %s", m.Reason)
				}
				// Mark the next current entry before the cloud acknowledges EOF.
				if next != 0 {
					s.cur.Store(next)
					s.pos.Store(0)
				}
				select {
				case b.events <- event{ended: true, entry: track, session: s}:
				case <-ctx.Done():
					return nil
				}
				delete(entries, m.EntryID)
				if next == 0 {
					return nil
				}
			}
		case <-tick.C:
			if !loaded {
				continue
			}
			wanted, _ := b.queue.after(id)
			if wanted == next {
				continue
			}
			if next != 0 {
				if err := call("playlist-clear"); err != nil {
					return err
				}
				for key, track := range entries {
					if track != id {
						delete(entries, key)
					}
				}
				next = 0
			}
			// Sign near the transition so a long current track cannot expire
			// its successor's URL. Keep at most one upcoming entry.
			if wanted == 0 || duration > 0 && duration-s.position() > 30 {
				continue
			}
			if fetching || time.Now().Before(nextRetry) {
				continue
			}
			fetching = true
			go func(id uint64) {
				u, err := b.original(ctx, id)
				upcomingURL <- upcoming{id: id, url: u, err: err}
			}(wanted)
		}
	}
}

func mpvFormat(data json.RawMessage) format {
	var p struct {
		Format   string `json:"format"`
		Rate     int    `json:"samplerate"`
		Channels int    `json:"channel-count"`
	}
	if json.Unmarshal(data, &p) != nil {
		return format{}
	}
	bits := 0
	switch p.Format {
	case "s16", "s16p":
		bits = 16
	case "s32", "s32p":
		bits = 32
	}
	return format{rate: p.Rate, channels: p.Channels, bits: bits}
}

func validateMPVFormat(source, output format) error {
	if source.rate != output.rate {
		return fmt.Errorf("mpv changed source sample rate: %d to %d Hz", source.rate, output.rate)
	}
	if source.channels != output.channels && !(source.channels == 1 && output.channels == 2) {
		return errors.New("mpv changed source channel layout")
	}
	if output.bits != 32 {
		return errors.New("mpv output is not S32 PCM")
	}
	return nil
}

func (b *mpvBackend) original(ctx context.Context, id uint64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return b.streamURL(ctx, id)
}
