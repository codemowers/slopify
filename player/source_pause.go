package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

type sourcePauseIntent struct {
	Version   uint64 `json:"version"`
	Entry     uint64 `json:"entry"`
	Op        string `json:"op"`
	selection uint64
}

// Called only by the controller owner. Do not share its desired state with the
// routing or HTTP goroutines. Coalesce intents; routing never waits for cloud.
func (c *controller) requestSourcePause() {
	if c.sources == nil || c.desired == nil || !c.desired.Adopted {
		return
	}
	c.sources.mu.Lock()
	selected := c.sources.Selected
	other := selected != "" && selected != "slopify"
	revision := c.sources.Revision
	resume := c.sources.resumeRevision == revision && revision != 0 && c.sources.Selected == "slopify" && c.sources.Mode == "off"
	c.sources.resumeRevision = 0
	c.sources.mu.Unlock()
	// Pause only at the transition away from Slopify. A later explicit Play
	// is a new user decision, even while another input remains pinned. Restored
	// startup selection is not a new switch either.
	leftSlopify := c.sourcePauseSelected == "slopify" && other
	c.sourcePauseSelected = selected
	op := ""
	if leftSlopify && c.desired.State == "playing" {
		op = "pause-if-sole-output"
	}
	if resume && c.desired.State == "paused" && c.desired.Current != 0 {
		op = "play-if-sole-output"
	}
	if op == "" {
		return
	}
	intent := sourcePauseIntent{Version: c.desired.Version, Entry: c.desired.Current, Op: op, selection: revision}
	select {
	case <-c.sourcePause:
	default:
	}
	select {
	case c.sourcePause <- intent:
	default:
	}
}

func (c *controller) runSourcePause(ctx context.Context) {
	var completed sourcePauseIntent
	var logAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case intent := <-c.sourcePause:
			if intent == completed {
				continue
			}
			c.sources.mu.Lock()
			valid := c.sources.Revision == intent.selection && ((intent.Op == "pause-if-sole-output" && c.sources.Selected != "slopify") || (intent.Op == "play-if-sole-output" && c.sources.Selected == "slopify" && c.sources.Mode == "off"))
			c.sources.mu.Unlock()
			if !valid {
				continue
			}
			reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			resp, err := c.request(reqCtx, http.MethodPost, "/device/v1/control", intent)
			status := 0
			if resp != nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil && status == http.StatusNoContent {
				completed = intent
				slog.Info("source playback control", "op", intent.Op, "entry", intent.Entry)
				continue
			}
			// A failed mutation is not replayed against a newer playback decision.
			if status >= 400 && status < 500 {
				completed = intent
			}
			if time.Now().After(logAt) {
				slog.Warn("conditional source control failed", "status", status)
				logAt = time.Now().Add(30 * time.Second)
			}
		}
	}
}
