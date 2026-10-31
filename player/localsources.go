package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codemowers/slopify/player/internal/audiometer"
)

type speakerSource struct {
	Paused       bool              `json:"paused,omitempty"`
	Playing      bool              `json:"playing,omitempty"`
	Buffer       *sourceBuffer     `json:"buffer,omitempty"`
	ReadyAt      time.Time         `json:"-"`
	Starting     bool              `json:"starting,omitempty"`
	AudibleSince time.Time         `json:"-"`
	SignalAt     time.Time         `json:"-"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	LastSeen     time.Time         `json:"-"`
	ID           string            `json:"id"`
	Key          string            `json:"key,omitempty"`
	Name         string            `json:"name"`
	Protocol     string            `json:"protocol,omitempty"`
	IP           string            `json:"ip,omitempty"`
	Host         string            `json:"host,omitempty"`
	Title        string            `json:"title,omitempty"`
	Format       *localPCM         `json:"format,omitempty"`
	State        string            `json:"state,omitempty"`
	Connected    bool              `json:"connected"`
	Active       bool              `json:"active"`
	Level        audiometer.Window `json:"level"`
	LastSound    time.Time         `json:"-"`
	Sounding     bool              `json:"-"`
	LastAudible  time.Time         `json:"-"`
	meter        *audiometer.Meter
	history      []audiometer.Window
}

// sourceBuffer shows how full a source's queues are, for the speaker page.
type sourceBuffer struct {
	StreamMS    int    `json:"streamMs"`
	StreamMaxMS int    `json:"streamMaxMs"`
	DeviceMS    int    `json:"deviceMs"`
	DeviceMaxMS int    `json:"deviceMaxMs"`
	Underflows  uint64 `json:"underflows"`
}

type sourceOutputWindow struct {
	SourceID string `json:"sourceId"`
	audiometer.Window
}
type speakerSources struct {
	resumeRevision    uint64
	PrioritySilenceMS int                  `json:"prioritySilenceMs"`
	AutoMode          string               `json:"autoMode"`
	MinimumSoundMS    int                  `json:"minimumSoundMs"`
	Output            []sourceOutputWindow `json:"output"`
	mu                sync.Mutex
	Mode              string           `json:"mode"`
	Selected          string           `json:"selected"`
	Sources           []*speakerSource `json:"sources"`
	Revision          uint64           `json:"revision"`
	Error             string           `json:"error,omitempty"`
	Fixed             bool             `json:"fixed,omitempty"`
	file              string
}

func readSourceSettings(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 16385))
}

func newSpeakerSources(file string) *speakerSources {
	s := &speakerSources{PrioritySilenceMS: 30000, AutoMode: "recent", MinimumSoundMS: 500, Mode: "off", Selected: "slopify", Revision: 1, file: file}
	if file != "" {
		if data, err := readSourceSettings(file); err == nil && len(data) <= 16384 {
			saved := speakerSources{MinimumSoundMS: 500, PrioritySilenceMS: 30000}
			if json.Unmarshal(data, &saved) == nil && validSourceMode(saved.Mode) && saved.MinimumSoundMS >= 0 && saved.MinimumSoundMS <= 2000 && saved.PrioritySilenceMS >= 0 && saved.PrioritySilenceMS <= 120000 && len(saved.Sources) <= 8 {
				seen := map[string]bool{}
				valid := true
				for _, source := range saved.Sources {
					if source == nil || !validSourceID(source.ID) || seen[source.ID] || len(source.Key) > 512 {
						valid = false
						break
					}
					seen[source.ID] = true
				}
				if valid {
					if saved.AutoMode == "recent" || saved.AutoMode == "priority" {
						s.AutoMode = saved.AutoMode
					} else if saved.Mode == "priority" {
						s.AutoMode = "priority"
					}
					s.PrioritySilenceMS = saved.PrioritySilenceMS
					s.MinimumSoundMS = saved.MinimumSoundMS
					s.Mode = saved.Mode
					s.Selected = saved.Selected
					s.Sources = saved.Sources
				}
			}
		}
	}
	foundSlopify := false
	for _, source := range s.Sources {
		foundSlopify = foundSlopify || source.ID == "slopify"
	}
	if !foundSlopify {
		s.Sources = append([]*speakerSource{{ID: "slopify", Key: "slopify", Name: "Slopify"}}, s.Sources...)
	}
	for _, source := range s.Sources {
		source.Connected = false
		source.Active = false
		source.meter = audiometer.New()
	}
	return s
}
func validSourceID(id string) bool {
	return id == "slopify" || len(id) == 5 && strings.HasPrefix(id, "lan_") && id[4] >= '1' && id[4] <= '7'
}

// withoutServer leaves no way to play Slopify, so a pin on it must not keep LAN
// sources silent. The setting is only saved if the user changes something.
func (s *speakerSources) withoutServer() {
	if s.Mode == "off" && s.Selected == "slopify" {
		s.Mode = s.AutoMode
	}
}

func validSourceMode(mode string) bool {
	return mode == "off" || mode == "recent" || mode == "priority"
}
func (s *speakerSources) save() {
	if s.file == "" {
		return
	}
	// Serialize only the remembered peer mapping and user policy, never current
	// node IDs or credentials. Atomic rename preserves the previous good state.
	type peer struct{ ID, Key, Name, IP, Host, Protocol string }
	saved := struct {
		PrioritySilenceMS int    `json:"prioritySilenceMs"`
		AutoMode          string `json:"autoMode"`
		MinimumSoundMS    int    `json:"minimumSoundMs"`
		Mode              string `json:"mode"`
		Selected          string `json:"selected"`
		Sources           []peer `json:"sources"`
	}{PrioritySilenceMS: s.PrioritySilenceMS, AutoMode: s.AutoMode, MinimumSoundMS: s.MinimumSoundMS, Mode: s.Mode, Selected: s.Selected}
	for _, p := range s.Sources {
		saved.Sources = append(saved.Sources, peer{p.ID, p.Key, p.Name, p.IP, p.Host, p.Protocol})
	}
	data, err := json.Marshal(saved)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.file), 0700)
	}
	if err == nil {
		err = os.WriteFile(s.file+".tmp", data, 0600)
	}
	if err == nil {
		err = os.Rename(s.file+".tmp", s.file)
	}
	if err != nil {
		s.Error = "Cannot persist local source settings"
	}
}

type sourceFallback struct {
	SourceID string    `json:"sourceId"`
	At       time.Time `json:"at"`
}

// Called with s.mu held. Report only an actionable lower-priority fallback;
// never infer silence solely from missing samples or invent a target.
func (s *speakerSources) pendingFallback(now time.Time) *sourceFallback {
	if s.Mode != "priority" {
		return nil
	}
	currentIndex := -1
	for i, p := range s.Sources {
		if p.ID == s.Selected {
			currentIndex = i
			break
		}
	}
	if currentIndex < 0 {
		return nil
	}
	current := s.Sources[currentIndex]
	if current.Starting || sourceMeterStarting(current, now) || !prioritySourceHeld(current, s.Selected, now, s.PrioritySilenceMS) {
		return nil
	}
	// A fresh above-threshold window means the current source is still sounding.
	if current.Connected && !current.AudibleSince.IsZero() && now.Sub(current.SignalAt) <= time.Second {
		return nil
	}
	for i, p := range s.Sources {
		sounding := !p.Paused && p.Connected && p.Sounding && !p.AudibleSince.IsZero() && now.Sub(p.SignalAt) <= time.Second
		if !sounding {
			continue
		}
		if i < currentIndex {
			return nil
		} // Higher priority takes over without this hold.
		if i > currentIndex {
			return &sourceFallback{SourceID: p.ID, At: priorityHoldStart(current).Add(time.Duration(s.PrioritySilenceMS) * time.Millisecond)}
		}
	}
	return nil
}

func (s *speakerSources) snapshot() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := struct {
		Fallback          *sourceFallback      `json:"fallback,omitempty"`
		PrioritySilenceMS int                  `json:"prioritySilenceMs"`
		AutoMode          string               `json:"autoMode"`
		MinimumSoundMS    int                  `json:"minimumSoundMs"`
		Mode              string               `json:"mode"`
		Selected          string               `json:"selected"`
		Sources           []speakerSource      `json:"sources"`
		Revision          uint64               `json:"revision"`
		Error             string               `json:"error,omitempty"`
		Output            []sourceOutputWindow `json:"output"`
		Fixed             bool                 `json:"fixed,omitempty"`
	}{Fixed: s.Fixed, Fallback: s.pendingFallback(time.Now()), PrioritySilenceMS: s.PrioritySilenceMS, AutoMode: s.AutoMode, MinimumSoundMS: s.MinimumSoundMS, Mode: s.Mode, Selected: s.Selected, Revision: s.Revision, Error: s.Error, Output: append([]sourceOutputWindow(nil), s.Output...)}
	for _, p := range s.Sources {
		copy := *p
		copy.Key = ""
		if p.meter != nil {
			copy.Level = p.meter.Snapshot()
		}
		result.Sources = append(result.Sources, copy)
	}
	return result
}
func (c *controller) localSourceCommand(w http.ResponseWriter, r *http.Request) {
	var cmd struct {
		PrioritySilenceMS *int     `json:"prioritySilenceMs"`
		SwitchNow         bool     `json:"switchNow"`
		Unpin             bool     `json:"unpin"`
		AutoMode          string   `json:"autoMode"`
		MinimumSoundMS    *int     `json:"minimumSoundMs"`
		Mode              string   `json:"mode"`
		Selected          string   `json:"selected"`
		Forget            string   `json:"forget"`
		Order             []string `json:"order"`
		Revision          uint64   `json:"revision"`
	}
	if !localMutation(w, r, &cmd) {
		return
	}
	s := c.sources
	if s == nil {
		http.Error(w, "Source control unavailable", 409)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cmd.Revision != s.Revision {
		http.Error(w, "Source settings changed; refresh and retry", 409)
		return
	}
	if cmd.AutoMode != "" && cmd.AutoMode != "recent" && cmd.AutoMode != "priority" {
		http.Error(w, "Invalid automatic mode", 400)
		return
	}
	if cmd.SwitchNow {
		if cmd.Unpin || cmd.Selected != "" || cmd.Mode != "" || cmd.AutoMode != "" || cmd.Order != nil || cmd.MinimumSoundMS != nil || cmd.PrioritySilenceMS != nil {
			http.Error(w, "Switch now cannot be combined with policy changes", 400)
			return
		}
		fallback := s.pendingFallback(time.Now())
		if fallback == nil {
			http.Error(w, "No pending source switch", 409)
			return
		}
		for _, source := range s.Sources {
			if source.ID == s.Selected {
				source.LastAudible = time.Time{}
				source.ReadyAt = time.Time{}
			}
		}
		s.Selected = fallback.SourceID
		s.Revision++
		s.save()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cmd.Unpin && cmd.Selected != "" {
		http.Error(w, "Cannot unpin with this command", 400)
		return
	}
	if cmd.PrioritySilenceMS != nil && (*cmd.PrioritySilenceMS < 0 || *cmd.PrioritySilenceMS > 120000) {
		http.Error(w, "Silence allowance must be 0–120000 ms", 400)
		return
	}
	if cmd.MinimumSoundMS != nil && (*cmd.MinimumSoundMS < 0 || *cmd.MinimumSoundMS > 2000) {
		http.Error(w, "Minimum sound duration must be 0–2000 ms", 400)
		return
	}
	if cmd.Mode != "" && !validSourceMode(cmd.Mode) {
		http.Error(w, "Invalid source mode", 400)
		return
	}
	if cmd.Forget != "" {
		forgotten := s.source(cmd.Forget)
		if forgotten == nil || forgotten.ID == "slopify" {
			http.Error(w, "Unknown source", 400)
			return
		}
		if forgotten.Connected {
			http.Error(w, "Source is connected", http.StatusConflict)
			return
		}
		s.Sources = slices.DeleteFunc(s.Sources, func(p *speakerSource) bool { return p == forgotten })
		if s.Selected == cmd.Forget { // A pin on it must not leave the speaker silent.
			s.Selected, s.Mode = "slopify", s.AutoMode
		}
	}
	if cmd.Selected != "" {
		found := false
		for _, p := range s.Sources {
			found = found || p.ID == cmd.Selected
		}
		if !found {
			http.Error(w, "Unknown source", 400)
			return
		}
	}
	if cmd.Order != nil {
		if len(cmd.Order) != len(s.Sources) {
			http.Error(w, "Invalid source order", 400)
			return
		}
		positions := map[string]int{}
		for i, id := range cmd.Order {
			positions[id] = i
		}
		for _, p := range s.Sources {
			if _, ok := positions[p.ID]; !ok {
				http.Error(w, "Invalid source order", 400)
				return
			}
		}
		sort.SliceStable(s.Sources, func(i, j int) bool { return positions[s.Sources[i].ID] < positions[s.Sources[j].ID] })
	}
	if cmd.PrioritySilenceMS != nil {
		s.PrioritySilenceMS = *cmd.PrioritySilenceMS
	}
	if cmd.MinimumSoundMS != nil {
		s.MinimumSoundMS = *cmd.MinimumSoundMS
	}
	if cmd.Mode != "" {
		s.Mode = cmd.Mode
		if cmd.Mode != "off" {
			s.AutoMode = cmd.Mode
		}
	}
	if cmd.AutoMode != "" {
		s.AutoMode = cmd.AutoMode
		if s.Mode != "off" {
			s.Mode = cmd.AutoMode
		}
	}
	if cmd.Unpin {
		s.Mode = s.AutoMode
		// Releasing a pin is an explicit recency choice. Already-sounding
		// peers must not immediately reclaim the output using older starts.
		if s.Mode == "recent" {
			for _, source := range s.Sources {
				if source.ID == s.Selected {
					source.LastSound = time.Now()
					break
				}
			}
		}
	}
	if cmd.Selected != "" {
		s.Selected = cmd.Selected
		s.Mode = "off"
	}

	s.Revision++
	if cmd.Selected == "slopify" {
		s.resumeRevision = s.Revision
	}
	s.save()
	w.WriteHeader(http.StatusNoContent)
}
func rememberSourcePeak(p *speakerSource, level audiometer.Window, now time.Time) {
	if !level.At.IsZero() && now.Sub(level.At) <= 30*time.Second && (len(p.history) == 0 || p.history[len(p.history)-1].Sequence != level.Sequence) {
		p.history = append(p.history, level)
	}
	for len(p.history) > 150 || len(p.history) > 0 && now.Sub(p.history[0].At) > 30*time.Second {
		p.history = p.history[1:]
	}
}
func (c *controller) localHistory(w http.ResponseWriter, r *http.Request) {
	history := map[string][]audiometer.Window{}
	sources := c.sourceList()
	sources.mu.Lock()
	for _, source := range sources.Sources {
		history[source.ID] = append([]audiometer.Window(nil), source.history...)
	}
	sources.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

// Missing telemetry is not silence. In particular, reconfiguring a raw port
// can briefly recreate its meter; that must not invent a sound-stop/start edge.
func observeSourceSignal(p *speakerSource, level audiometer.Window, now time.Time) {
	if level.At.IsZero() || now.Sub(level.At) > time.Second {
		p.AudibleSince = time.Time{}
		return
	}
	if !level.At.After(p.SignalAt) {
		return
	}
	if !p.SignalAt.IsZero() && level.At.Sub(p.SignalAt) > 500*time.Millisecond {
		p.AudibleSince = time.Time{}
	}
	p.SignalAt = level.At
	if level.Peak >= .001 {
		if p.AudibleSince.IsZero() {
			p.AudibleSince = level.At
		}
		if !p.Sounding {
			p.LastSound = now
		}
		p.Sounding = true
		p.LastAudible = now
	} else {
		p.AudibleSince = time.Time{}
		if now.Sub(p.LastAudible) > 2*time.Second {
			p.Sounding = false
		}
	}
}

// A selected priority source may retain its place through silence, meter gaps
// and brief node recreation. Higher-priority sounding sources still win first.
func prioritySourceHeld(p *speakerSource, selected string, now time.Time, allowanceMS int) bool {
	at := priorityHoldStart(p)
	return !p.Paused && p.ID == selected && !at.IsZero() && now.Sub(at) <= time.Duration(allowanceMS)*time.Millisecond
}

func priorityHoldStart(p *speakerSource) time.Time {
	if p.ReadyAt.After(p.LastAudible) {
		return p.ReadyAt
	}
	return p.LastAudible
}
func sourceMeterStarting(p *speakerSource, now time.Time) bool {
	return !p.ReadyAt.IsZero() && p.SignalAt.Before(p.ReadyAt) && now.Sub(p.ReadyAt) < 3*time.Second
}
func selectedSourceStarting(sources []*speakerSource, selected string, now time.Time) bool {
	for _, p := range sources {
		if p.ID == selected {
			return !p.Paused && (p.Starting || sourceMeterStarting(p, now))
		}
	}
	return false
}
func updateSourceStartup(p *speakerSource, playbackState string, now time.Time) {
	if p.ID != "slopify" {
		return
	}
	starting := playbackStarting(playbackState)
	if p.Starting && playbackState == "running" {
		p.ReadyAt = now
	}
	if playbackState == "idle" || playbackState == "error" {
		p.ReadyAt = time.Time{}
	}
	p.Starting = starting
}

// chooseSource applies automatic selection, with the lock held.
func (s *speakerSources) chooseSource(now time.Time) {
	if s.Mode == "off" || selectedSourceStarting(s.Sources, s.Selected, now) {
		return
	}
	chosen := ""
	var last time.Time
	for _, p := range s.Sources {
		if s.Mode == "priority" && prioritySourceHeld(p, s.Selected, now, s.PrioritySilenceMS) {
			chosen = p.ID
			break
		}
		if p.Paused || !p.Connected || !p.Sounding || ((p.ID != s.Selected || s.Mode == "priority") && now.Sub(p.meter.Snapshot().At) > time.Second) {
			continue
		}
		if s.Mode == "recent" && p.ID != s.Selected && s.MinimumSoundMS > 0 && (p.AudibleSince.IsZero() || p.SignalAt.Sub(p.AudibleSince) < time.Duration(s.MinimumSoundMS)*time.Millisecond) {
			continue
		}
		if s.Mode == "priority" {
			chosen = p.ID
			break
		}
		if chosen == "" || p.LastSound.After(last) {
			chosen = p.ID
			last = p.LastSound
		}
	}
	if chosen == "" && s.Mode == "priority" {
		for _, p := range s.Sources {
			if p.ID == "slopify" && p.Connected {
				chosen = p.ID
				break
			}
		}
	}
	if chosen != "" && chosen != s.Selected {
		s.Selected = chosen
		s.Revision++
	}
}

func (c *controller) sourceSnapshot() any {
	if s := c.sourceList(); s != nil {
		return s.snapshot()
	}
	return nil
}

// sourceList is the arbitrated sources, or the native engine's single fixed one.
func (c *controller) sourceList() *speakerSources {
	if c.sources != nil {
		return c.sources
	}
	if c.local != nil {
		return c.local.nativeSources
	}
	return nil
}

// source finds a source by ID, with the lock held.
func (s *speakerSources) source(id string) *speakerSource {
	for _, p := range s.Sources {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func newFixedSources() *speakerSources {
	return &speakerSources{Fixed: true, Mode: "off", AutoMode: "recent", Selected: "slopify", Revision: 1,
		Sources: []*speakerSource{{ID: "slopify", Name: "Slopify", Connected: true, meter: audiometer.New()}}}
}
