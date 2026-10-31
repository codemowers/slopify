package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/codemowers/slopify/internal/volume"
	"math"
	"testing"
)

func level(n int) *int { return &n }

func TestVolumeOwnershipBoundsAndRevisions(t *testing.T) {
	s := queueStore()
	defer addBrowser(t, s, "alice", "web-alice")()
	p := &Player{ID: "amp", Kind: Device, Owner: queueID("alice"), Connected: true, Adopted: true, Volume: &volume.Control{Name: "Digital", Min: 0, Max: 40, Level: 20, Target: 20, Revision: 1}, Version: 8, Current: 1, State: Playing}
	s.players[p.ID] = p
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{p.ID}}); err != nil {
		t.Fatal(err)
	}
	version, current, state := p.Version, p.Current, p.State
	for _, user := range []string{"bob", "alice"} {
		v := 30
		err := s.webCommand(user, p.ID, command{Op: "volume", Level: &v})
		if user == "bob" && !errors.Is(err, errForbidden) {
			t.Fatal("foreign volume allowed", err)
		}
		if user == "alice" && err != nil {
			t.Fatal(err)
		}
	}
	if p.Version != version || p.Current != current || p.State != state || p.Volume.Level != 20 || p.Volume.Target != 30 || p.Volume.Revision != 2 {
		t.Fatalf("volume changed playback or skipped hardware confirmation: %+v", p)
	}
	for _, v := range []*int{nil, level(-1), level(41)} {
		if err := s.webCommand("alice", p.ID, command{Op: "volume", Level: v}); !errors.Is(err, errBadRequest) {
			t.Fatal("invalid level accepted", err)
		}
	}
	if err := s.volumeReport(p.ID, volume.Report{Revision: 1, Level: 10}); err != nil {
		t.Fatal(err)
	}
	if p.Volume.Target != 30 {
		t.Fatal("stale volume report overwrote command")
	}
	if err := s.volumeReport(p.ID, volume.Report{Revision: 2, Level: 30}); err != nil {
		t.Fatal(err)
	}
	if err := s.volumeReport(p.ID, volume.Report{Revision: 2, Level: 25}); err != nil {
		t.Fatal(err)
	}
	if p.Volume.Level != 25 || p.Volume.Target != 25 || p.Version != version {
		t.Fatal("local mixer change not reflected independently")
	}
	var snapshot Player
	if err := json.Unmarshal(s.webMarshal(p), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Volume == nil || snapshot.Volume.Level != 25 {
		t.Fatal("volume missing from public SSE snapshot")
	}
}

func TestVolumeReportCarriesHardwareGain(t *testing.T) {
	db := -42.0
	s := queueStore()
	s.players["d"] = &Player{ID: "d", Kind: Device, Volume: &volume.Control{Min: 0, Max: 40, Level: 20, Target: 20, Revision: 1}}
	if err := s.volumeReport("d", volume.Report{Revision: 1, Level: 20, DB: &db, Muted: true}); err != nil {
		t.Fatal(err)
	}
	v := s.players["d"].Volume
	if v.DB == nil || *v.DB != -42 || !v.Muted || v.Level != 20 {
		t.Fatalf("%+v", v)
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), -1000, 1000} {
		if err := s.volumeReport("d", volume.Report{Revision: 1, Level: 20, DB: &bad}); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestDeviceButtonControlsOnlySelectedQueueAndIgnoresStalePresses(t *testing.T) {
	s := queueStore()
	defer addBrowser(t, s, "alice", "web-alice")()
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "song.flac", Duration: 120}}, Play: true}); err != nil {
		t.Fatal(err)
	}
	p := &Player{ID: "amp", Kind: Device, Owner: queueID("alice"), Connected: true, Adopted: true}
	s.players[p.ID] = p
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{p.ID}}); err != nil {
		t.Fatal(err)
	}
	press := deviceControl{Op: "toggle", Version: p.Version, Entry: p.Current}
	if err := s.deviceControl(p.ID, press); err != nil {
		t.Fatal(err)
	}
	q := s.players[queueID("alice")]
	if q.State != Paused || p.State != Paused {
		t.Fatal("button did not pause shared queue")
	}
	if err := s.deviceControl(p.ID, press); err != nil {
		t.Fatal(err)
	}
	if q.State != Paused {
		t.Fatal("stale repeated press resumed playback")
	}
	if err := s.deviceControl(p.ID, deviceControl{Op: "clear", Version: p.Version, Entry: p.Current}); !errors.Is(err, errBadRequest) {
		t.Fatal("device accepted arbitrary command")
	}
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.deviceControl(p.ID, deviceControl{Op: "toggle", Version: p.Version, Entry: p.Current}); !errors.Is(err, errBadRequest) {
		t.Fatal("detached device controlled account queue")
	}
}

func TestLocalSourcePauseOnlyAffectsSoleSelectedOutput(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint("shared=", shared), func(t *testing.T) {
			s := queueStore()
			defer addBrowser(t, s, "alice", "web-alice")()
			if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "song.flac", Duration: 120}}, Play: true}); err != nil {
				t.Fatal(err)
			}
			p := &Player{ID: "amp", Kind: Device, Owner: queueID("alice"), Connected: true, Adopted: true}
			s.players[p.ID] = p
			outputs := []string{p.ID}
			if shared {
				outputs = append(outputs, "web-alice")
			}
			if err := s.userCommand("alice", command{Op: "outputs", Outputs: outputs}); err != nil {
				t.Fatal(err)
			}
			q := s.players[queueID("alice")]
			stale := deviceControl{Op: "pause-if-sole-output", Version: p.Version - 1, Entry: p.Current}
			if err := s.deviceControl(p.ID, stale); err != nil {
				t.Fatal(err)
			}
			if q.State != Playing {
				t.Fatal("stale request paused queue")
			}
			request := deviceControl{Op: "pause-if-sole-output", Version: p.Version, Entry: p.Current}
			version := q.Version
			if err := s.deviceControl(p.ID, request); err != nil {
				t.Fatal(err)
			}
			if shared {
				if q.State != Playing || p.State != Playing || q.Version != version {
					t.Fatal("local source switch changed shared playback")
				}
			} else if q.State != Paused || p.State != Paused {
				t.Fatal("sole-output queue did not pause")
			}
			if err := s.deviceControl(p.ID, request); err != nil {
				t.Fatal(err)
			}
			if !shared && q.State != Paused {
				t.Fatal("repeated request resumed queue")
			}
		})
	}
}

func TestPinResumeOnlyAffectsSoleSelectedOutput(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			s := queueStore()
			defer addBrowser(t, s, "alice", "web-alice")()
			if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "song.flac", Duration: 120}}, Play: true}); err != nil {
				t.Fatal(err)
			}
			p := &Player{ID: "amp", Kind: Device, Owner: queueID("alice"), Connected: true, Adopted: true}
			s.players[p.ID] = p
			outputs := []string{p.ID}
			if shared {
				outputs = append(outputs, "web-alice")
			}
			if err := s.userCommand("alice", command{Op: "outputs", Outputs: outputs}); err != nil {
				t.Fatal(err)
			}
			if err := s.userCommand("alice", command{Op: "pause"}); err != nil {
				t.Fatal(err)
			}
			q := s.players[queueID("alice")]
			version := q.Version
			if err := s.deviceControl(p.ID, deviceControl{Op: "play-if-sole-output", Version: p.Version - 1, Entry: p.Current}); err != nil {
				t.Fatal(err)
			}
			if q.State != Paused {
				t.Fatal("stale pin resumed queue")
			}
			request := deviceControl{Op: "play-if-sole-output", Version: p.Version, Entry: p.Current}
			if err := s.deviceControl(p.ID, request); err != nil {
				t.Fatal(err)
			}
			if shared {
				if q.State != Paused || q.Version != version {
					t.Fatal("pin resumed shared queue")
				}
			} else if q.State != Playing {
				t.Fatal("sole-output pin did not resume")
			}
		})
	}
}

func TestDeviceButtonSkipsTracks(t *testing.T) {
	s := queueStore()
	defer addBrowser(t, s, "alice", "web-alice")()
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "a.flac", Duration: 120}, {Path: "b.flac", Duration: 120}}, Play: true}); err != nil {
		t.Fatal(err)
	}
	p := &Player{ID: "amp", Kind: Device, Owner: queueID("alice"), Connected: true, Adopted: true}
	s.players[p.ID] = p
	if err := s.userCommand("alice", command{Op: "outputs", Outputs: []string{p.ID}}); err != nil {
		t.Fatal(err)
	}
	first := p.Current
	if err := s.deviceControl(p.ID, deviceControl{Op: "next", Version: p.Version, Entry: p.Current}); err != nil {
		t.Fatal(err)
	}
	if p.Current == first {
		t.Fatal("next did not change track")
	}
	if err := s.deviceControl(p.ID, deviceControl{Op: "prev", Version: p.Version, Entry: p.Current}); err != nil {
		t.Fatal(err)
	}
	if p.Current != first {
		t.Fatal("prev did not return to the first track")
	}
}
