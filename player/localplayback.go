package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type localPlaybackCommand struct {
	Source  string `json:"source,omitempty"`
	Op      string `json:"op"`
	Version uint64 `json:"version"`
	Entry   uint64 `json:"entry"`
}
type localTransport struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Version   uint64 `json:"version"`
	Entry     uint64 `json:"entry"`
	Source    string `json:"source,omitempty"` // The protocol of a sender controlled instead, such as "airplay".
	Title     string `json:"title,omitempty"`
	Artist    string `json:"artist,omitempty"`
	Album     string `json:"album,omitempty"`
}
type localPlaybackRequest struct {
	ctx     context.Context
	command *localPlaybackCommand
	reply   chan localPlaybackResult
}
type localPlaybackResult struct {
	status    int
	transport localTransport
}

// Only run reads desired. HTTP hands it a bounded, cancellable request.
func (c *controller) handleLocalPlayback(req localPlaybackRequest) {
	result := localPlaybackResult{status: http.StatusOK}
	defer func() { req.reply <- result }()
	if req.ctx.Err() != nil {
		result.status = http.StatusGatewayTimeout
		return
	}
	s := c.desired
	if s != nil {
		result.transport = localTransport{Available: s.Adopted && s.Current != 0, State: s.State, Version: s.Version, Entry: s.Current}
		if e := s.entry(s.Current); e != nil {
			result.transport.Title = e.Title
			result.transport.Artist = e.Artist
			result.transport.Album = e.Album
		}
	}
	if req.command == nil {
		return
	}
	cmd := *req.command
	cmd.Source = "local-controls"
	if !result.transport.Available || cmd.Version != s.Version || cmd.Entry != s.Current {
		result.status = http.StatusConflict
		return
	}
	// The cloud also checks version and entry under the queue lock.
	switch cmd.Op {
	case "play":
		if s.State == "playing" {
			result.status = http.StatusNoContent
			return
		}
		cmd.Op = "toggle"
	case "pause", "stop":
		if s.State != "playing" {
			result.status = http.StatusNoContent
			return
		}
		cmd.Op = "toggle"
	}
	ctx, cancel := context.WithTimeout(req.ctx, 5*time.Second)
	defer cancel()
	resp, err := c.request(ctx, http.MethodPost, "/device/v1/control", cmd)
	if err != nil {
		result.status = http.StatusBadGateway
		return
	}
	resp.Body.Close()
	result.status = resp.StatusCode
	if result.status != http.StatusNoContent {
		result.status = http.StatusConflict
	}
}

func (c *controller) localPlaybackControl(w http.ResponseWriter, r *http.Request) {
	var cmd *localPlaybackCommand
	if r.Method == http.MethodPost {
		cmd = &localPlaybackCommand{}
		if !localMutation(w, r, cmd) {
			return
		}
		switch cmd.Op {
		case "toggle", "play", "pause", "stop", "next", "prev":
		default:
			http.Error(w, "Invalid playback operation", 400)
			return
		}
	}
	if stream, ok := c.selectedRemote(); ok {
		c.remotePlayback(w, r, stream, cmd)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req := localPlaybackRequest{ctx: ctx, command: cmd, reply: make(chan localPlaybackResult, 1)}
	select {
	case c.localPlaybackRequests <- req:
	case <-ctx.Done():
		http.Error(w, "Player busy", 504)
		return
	}
	select {
	case result := <-req.reply:
		if result.status == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result.transport)
		} else if result.status == http.StatusNoContent {
			w.WriteHeader(result.status)
		} else {
			http.Error(w, "Playback command unavailable, stale or failed; refresh before retrying", result.status)
		}
	case <-ctx.Done():
		http.Error(w, "Playback command timed out; outcome unknown", 504)
	}
}
