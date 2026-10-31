package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/audiometer"
)

func TestSourcePinOrderAndPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sources.json")
	s := newSpeakerSources(file)
	s.Sources = append(s.Sources, &speakerSource{ID: "lan_1", Key: "remembered-peer", Name: "Laptop", IP: "192.0.2.12"})
	c := &controller{sources: s}
	command := func(body string, want int) {
		r := httptest.NewRequest("POST", "http://speaker.local/api/sources", strings.NewReader(body))
		r.Header.Set("Origin", "http://speaker.local")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slopify-Control", "1")
		w := httptest.NewRecorder()
		c.localSourceCommand(w, r)
		if w.Code != want {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	command(`{"revision":1,"mode":"priority","order":["lan_1","slopify"]}`, 204)
	command(`{"revision":1,"selected":"slopify"}`, 409)
	command(`{"revision":2,"selected":"lan_1"}`, 204)
	restored := newSpeakerSources(file)
	if restored.Mode != "off" || restored.Selected != "lan_1" || restored.Sources[0].ID != "lan_1" || restored.Sources[0].Connected {
		t.Fatalf("bad restored state: %+v", restored)
	}
	if restored.AutoMode != "priority" {
		t.Fatal("pin lost automatic policy")
	}
	command(`{"revision":3,"autoMode":"recent","minimumSoundMs":900}`, 204)
	if s.Mode != "off" || s.AutoMode != "recent" {
		t.Fatal("policy change released pin")
	}
	unpinAt := time.Now()
	command(`{"revision":4,"unpin":true}`, 204)
	if s.Sources[0].LastSound.Before(unpinAt) {
		t.Fatal("unlock did not promote current source recency")
	}
	if s.Mode != "recent" || s.MinimumSoundMS != 900 {
		t.Fatal("unlock did not resume chosen policy")
	}
	command(`{"revision":5,"minimumSoundMs":2001}`, 400)
	data, _ := json.Marshal(restored.snapshot())
	if strings.Contains(string(data), "remembered-peer") {
		t.Fatal("internal peer key leaked")
	}
	stat, err := os.Stat(file)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("private local settings permissions", err)
	}
}

func TestMeterGapDoesNotInventSoundRestart(t *testing.T) {
	now := time.Now()
	p := &speakerSource{}
	observeSourceSignal(p, audiometer.Window{At: now, Peak: .01}, now)
	started := p.LastSound
	observeSourceSignal(p, audiometer.Window{At: now, Peak: 0}, now.Add(3*time.Second))
	if !p.Sounding || p.LastSound != started {
		t.Fatal("missing telemetry was treated as silence")
	}
	observeSourceSignal(p, audiometer.Window{At: now.Add(4 * time.Second), Peak: .01}, now.Add(4*time.Second))
	if p.LastSound != started {
		t.Fatal("meter recovery invented a sound start")
	}
	observeSourceSignal(p, audiometer.Window{At: now.Add(5 * time.Second), Peak: 0}, now.Add(5*time.Second))
	if !p.Sounding {
		t.Fatal("silence hold was not respected")
	}
	observeSourceSignal(p, audiometer.Window{At: now.Add(7 * time.Second), Peak: 0}, now.Add(7*time.Second))
	if p.Sounding {
		t.Fatal("fresh silence did not end activity")
	}
}

func TestSourceHistoryIsBoundedAndSurvivesRefresh(t *testing.T) {
	now := time.Now()
	p := &speakerSource{ID: "slopify"}
	for i := 0; i < 200; i++ {
		rememberSourcePeak(p, audiometer.Window{Sequence: uint64(i + 1), At: now.Add(time.Duration(i-199) * 200 * time.Millisecond), Peak: .25}, now)
	}
	if len(p.history) != 150 {
		t.Fatalf("history length %d", len(p.history))
	}
	rememberSourcePeak(p, p.history[len(p.history)-1], now)
	if len(p.history) != 150 {
		t.Fatal("duplicate peak appended")
	}
	c := &controller{sources: &speakerSources{Sources: []*speakerSource{p}}}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		c.localHistory(w, httptest.NewRequest("GET", "/api/history", nil))
		var h map[string][]audiometer.Window
		if json.Unmarshal(w.Body.Bytes(), &h) != nil || len(h["slopify"]) != 150 {
			t.Fatal("refresh lost history")
		}
	}
	rememberSourcePeak(p, audiometer.Window{}, now.Add(31*time.Second))
	if len(p.history) != 0 {
		t.Fatal("expired history retained")
	}
}

func TestSoundGuardRequiresFreshSustainedSamples(t *testing.T) {
	p := &speakerSource{}
	start := time.Now()
	sample := func(ms int, peak float64) {
		at := start.Add(time.Duration(ms) * time.Millisecond)
		observeSourceSignal(p, audiometer.Window{At: at, Peak: peak}, at)
	}
	sample(0, .01)
	sample(200, .01)
	sample(400, .01)
	if p.SignalAt.Sub(p.AudibleSince) >= 500*time.Millisecond {
		t.Fatal("short burst qualified")
	}
	sample(600, .01)
	if p.SignalAt.Sub(p.AudibleSince) < 500*time.Millisecond {
		t.Fatal("sustained sound did not qualify")
	}
	sample(800, 0)
	if !p.AudibleSince.IsZero() {
		t.Fatal("silence did not reset guard")
	}
	sample(1000, .01)
	sample(1800, .01)
	if p.AudibleSince != p.SignalAt {
		t.Fatal("missing samples counted as sustained sound")
	}
}
func TestSoundGuardPersistsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.json")
	s := newSpeakerSources(path)
	if s.MinimumSoundMS != 500 {
		t.Fatal("wrong default")
	}
	s.MinimumSoundMS = 0
	s.save()
	if newSpeakerSources(path).MinimumSoundMS != 0 {
		t.Fatal("explicit immediate setting was lost")
	}
}

func TestPrioritySilenceAllowance(t *testing.T) {
	now := time.Now()
	p := &speakerSource{ID: "slopify", LastAudible: now}
	if !prioritySourceHeld(p, "slopify", now.Add(29*time.Second), 30000) {
		t.Fatal("did not retain selected source through gap")
	}
	if prioritySourceHeld(p, "slopify", now.Add(31*time.Second), 30000) {
		t.Fatal("retained source beyond allowance")
	}
	if prioritySourceHeld(p, "lan_1", now, 30000) {
		t.Fatal("held unselected source")
	}
	path := filepath.Join(t.TempDir(), "sources.json")
	s := newSpeakerSources(path)
	if s.PrioritySilenceMS != 30000 {
		t.Fatal("wrong silence default")
	}
	s.PrioritySilenceMS = 60000
	s.save()
	if newSpeakerSources(path).PrioritySilenceMS != 60000 {
		t.Fatal("silence allowance not persisted")
	}
}

func TestFallbackCountdownOnlyWithSoundingLowerPriority(t *testing.T) {
	now := time.Now()
	selected := &speakerSource{ID: "slopify", Connected: true, LastAudible: now.Add(-5 * time.Second)}
	lower := &speakerSource{ID: "lan_1", Connected: true, Sounding: true, SignalAt: now, AudibleSince: now.Add(-time.Second)}
	s := &speakerSources{Mode: "priority", Selected: "slopify", PrioritySilenceMS: 30000, Sources: []*speakerSource{selected, lower}}
	pending := s.pendingFallback(now)
	if pending == nil || pending.SourceID != "lan_1" || pending.At != now.Add(25*time.Second) {
		t.Fatalf("wrong countdown: %+v", pending)
	}
	selected.Starting = true
	if s.pendingFallback(now) != nil {
		t.Fatal("countdown during stream startup")
	}
	selected.Starting = false
	lower.AudibleSince = time.Time{}
	if s.pendingFallback(now) != nil {
		t.Fatal("quiet fallback displayed countdown")
	}
	lower.AudibleSince = now
	selected.AudibleSince = now
	selected.SignalAt = now
	if s.pendingFallback(now) != nil {
		t.Fatal("countdown while selected source is sounding")
	}
	selected.AudibleSince = time.Time{}
	s.Mode = "off"
	if s.pendingFallback(now) != nil {
		t.Fatal("countdown while pinned")
	}
	s.Mode = "priority"
	lower.Connected = false
	if s.pendingFallback(now) != nil {
		t.Fatal("disconnected fallback displayed countdown")
	}
	lower.Connected = true
	s.Sources = []*speakerSource{lower, selected}
	if s.pendingFallback(now) != nil {
		t.Fatal("countdown for immediate higher-priority takeover")
	}
}

func TestStartupReadinessBridgesFirstMeter(t *testing.T) {
	now := time.Now()
	p := &speakerSource{ID: "slopify", LastAudible: now.Add(-time.Minute), SignalAt: now.Add(-time.Minute)}
	updateSourceStartup(p, "buffering", now)
	if !p.Starting {
		t.Fatal("buffering did not protect startup")
	}
	updateSourceStartup(p, "running", now.Add(time.Second))
	if p.Starting || !sourceMeterStarting(p, now.Add(1450*time.Millisecond)) {
		t.Fatal("lost protection before first meter")
	}
	if !prioritySourceHeld(p, "slopify", now.Add(29*time.Second), 30000) {
		t.Fatal("old track's timestamp expired the new track hold")
	}
	if sourceMeterStarting(p, now.Add(5*time.Second)) {
		t.Fatal("unbounded meter startup grace")
	}
	if prioritySourceHeld(p, "slopify", now.Add(32*time.Second), 30000) {
		t.Fatal("unbounded silence hold")
	}
	p.SignalAt = now.Add(1500 * time.Millisecond)
	if sourceMeterStarting(p, now.Add(1600*time.Millisecond)) {
		t.Fatal("fresh meter did not end startup grace")
	}
	updateSourceStartup(p, "idle", now.Add(2*time.Second))
	if !p.ReadyAt.IsZero() {
		t.Fatal("paused playback retained startup hold")
	}
}

func TestSwitchNowKeepsAutomaticPolicy(t *testing.T) {
	now := time.Now()
	s := newSpeakerSources("")
	s.Mode = "priority"
	s.AutoMode = "priority"
	s.Selected = "slopify"
	current := s.Sources[0]
	current.Connected = true
	current.LastAudible = now.Add(-5 * time.Second)
	s.Sources = append(s.Sources, &speakerSource{ID: "lan_1", Connected: true, Sounding: true, SignalAt: now, AudibleSince: now.Add(-time.Second)})
	c := &controller{sources: s}
	send := func(body string) int {
		r := httptest.NewRequest("POST", "http://speaker.local/api/sources", strings.NewReader(body))
		r.Header.Set("Origin", "http://speaker.local")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slopify-Control", "1")
		w := httptest.NewRecorder()
		c.localSourceCommand(w, r)
		return w.Code
	}
	if got := send(`{"revision":1,"switchNow":true}`); got != 204 {
		t.Fatalf("switch: %d", got)
	}
	if s.Selected != "lan_1" || s.Mode != "priority" || !current.LastAudible.IsZero() {
		t.Fatal("switch pinned or retained previous hold")
	}
	if got := send(`{"revision":1,"switchNow":true}`); got != 409 {
		t.Fatal("accepted stale revision")
	}
	if got := send(`{"revision":2,"switchNow":true}`); got != 409 {
		t.Fatal("switched without pending fallback")
	}
}

func TestPausedSourceDoesNotHoldPriority(t *testing.T) {
	now := time.Now()
	p := &speakerSource{ID: "slopify", Paused: true, Starting: true, LastAudible: now, ReadyAt: now}
	if prioritySourceHeld(p, "slopify", now, 30000) || selectedSourceStarting([]*speakerSource{p}, "slopify", now) {
		t.Fatal("paused source retained selection hold")
	}
	p.Paused = false
	if !prioritySourceHeld(p, "slopify", now, 30000) {
		t.Fatal("playing source lost silence hold")
	}
}

func TestForgetSourceOnlyWhenDisconnected(t *testing.T) {
	s := newSpeakerSources("")
	s.Sources = append(s.Sources, &speakerSource{ID: "lan_1", Connected: true}, &speakerSource{ID: "lan_2"})
	s.Selected, s.Mode = "lan_2", "off"
	c := &controller{sources: s}
	command := func(body string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "http://speaker.local/api/sources", strings.NewReader(body))
		r.Header.Set("Origin", "http://speaker.local")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slopify-Control", "1")
		w := httptest.NewRecorder()
		c.localSourceCommand(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	command(`{"revision":1,"forget":"slopify"}`, 400)
	command(`{"revision":1,"forget":"lan_1"}`, 409)
	command(`{"revision":1,"forget":"lan_2"}`, 204)
	if s.source("lan_2") != nil || s.Selected != "slopify" || s.Mode != "recent" {
		t.Fatalf("selected %q mode %q", s.Selected, s.Mode)
	}
}
