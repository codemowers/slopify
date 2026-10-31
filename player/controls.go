package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/codemowers/slopify/internal/volume"
)

// runKnob owns local input actuation independently of playback and HTTP.
// No network request is made here, even while the server is unreachable.
func (c *controller) runKnob(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-c.knob:
			if e.delta != 0 && c.amp != nil {
				c.amp.adjust(e.delta)
			}
			if e.delta != 0 && c.mixer != nil {
				if err := c.mixer.adjust(ctx, e.delta); err != nil {
					slog.Warn("adjusting knob volume", "err", err)
				} else {
					c.reportVolume(ctx)
				}
			}
			if e.action == actionMute && c.mixer != nil {
				if err := c.mixer.toggleMute(ctx); err != nil {
					slog.Warn("toggling mute", "err", err)
				} else {
					c.reportVolume(ctx)
				}
			}
			if op := e.action.buttonOp(); op != "" {
				// Do not let a stalled playback command stop volume actuation.
				select {
				case c.buttons <- op:
				default:
				}
			}
		}
	}
}

// A one-slot notification coalesces intermediate values. The reporter always
// takes a fresh snapshot; callers never wait for the report or its HTTP request.
func (c *controller) reportVolume(context.Context) {
	if c.mixer == nil {
		return
	}
	select {
	case c.volumeWake <- struct{}{}:
	default:
	}
}

func (c *controller) volumeReports(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.volumeWake:
		}
		// Limit telemetry to at most ten requests per second while retaining
		// every local hardware step. A slow request naturally coalesces more.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// Consume a redundant wake before taking the latest snapshot.
		select {
		case <-c.volumeWake:
		default:
		}
		if err := c.sendVolume(ctx); err != nil && ctx.Err() == nil {
			// Retry the latest state, never replay a queued historic level.
			timer = time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			c.reportVolume(ctx)
		}
	}
}

func (c *controller) sendVolume(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v := c.mixer.snapshot()
	resp, err := c.request(ctx, http.MethodPost, "/device/v1/volume", volume.Report{Revision: v.Revision, Level: v.Level, Error: v.Error, DB: v.DB, Muted: v.Muted})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("volume report: HTTP %d", resp.StatusCode)
	}
	return nil
}
