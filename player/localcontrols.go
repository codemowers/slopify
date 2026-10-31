package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/codemowers/slopify/internal/volume"
)

type localVolume struct {
	Level         int           `json:"level"`
	Min           int           `json:"min"`
	Max           int           `json:"max"`
	Steps         []volume.Step `json:"steps"`
	DB            *float64      `json:"db"`
	Muted         bool          `json:"muted"`
	SwitchMuted   bool          `json:"switchMuted"`
	MuteAvailable bool          `json:"muteAvailable"`
}

func (m *mixer) localVolume() *localVolume {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := &localVolume{Level: m.control.Level, Min: m.control.Min, Max: m.control.Max, Steps: m.control.Steps, Muted: m.switchMuted, MuteAvailable: m.switchAvailable, SwitchMuted: m.switchMuted}
	for _, step := range m.control.Steps {
		if step.Level == v.Level {
			v.DB = step.DB
			v.Muted = v.Muted || step.Muted
			break
		}
	}
	return v
}

// LAN controls are deliberately same-origin JSON only: no cross-origin form
// submissions, CORS permissions, credentials in URLs or arbitrary mixer names.
func localMutation(w http.ResponseWriter, r *http.Request, command any) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if err != nil || origin.Scheme != scheme || origin.Host != r.Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || r.Header.Get("X-Slopify-Control") != "1" || strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		http.Error(w, "Same-origin JSON control required", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(command) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid command", http.StatusBadRequest)
		return false
	}
	return true
}

func (c *controller) localAmpCommand(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Delta int `json:"delta"`
	}
	if !localMutation(w, r, &command) {
		return
	}
	if c.amp == nil {
		http.Error(w, "Amplifier control disabled", http.StatusConflict)
		return
	}
	if command.Delta != 1 && command.Delta != -1 {
		http.Error(w, "Delta must be 1 or -1", http.StatusBadRequest)
		return
	}
	c.amp.adjust(command.Delta)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c.amp.status())
}

func (c *controller) localVolumeCommand(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Level *int  `json:"level"`
		Muted *bool `json:"muted"`
	}
	if !localMutation(w, r, &command) {
		return
	}
	if c.mixer == nil {
		http.Error(w, "Hardware volume disabled", http.StatusConflict)
		return
	}
	if (command.Level == nil) == (command.Muted == nil) {
		http.Error(w, "Specify level or muted", http.StatusBadRequest)
		return
	}
	var err error
	if command.Level != nil {
		v := c.mixer.snapshot()
		if *command.Level < v.Min || *command.Level > v.Max {
			http.Error(w, "Outside hardware volume limits", http.StatusBadRequest)
			return
		}
		err = c.mixer.setLevel(r.Context(), *command.Level)
	} else {
		err = c.mixer.setMute(r.Context(), *command.Muted)
	}
	if err != nil {
		http.Error(w, "Hardware volume command failed", http.StatusServiceUnavailable)
		return
	}
	c.reportVolume(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c.mixer.localVolume())
}
