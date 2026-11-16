package main

import (
	"testing"
	"time"
)

func disconnectedDevice(t *testing.T) (*store, *Player, *Player) {
	t.Helper()
	s := queueStore()
	defer addBrowser(t, s, "alice", "browser")()
	if err := s.userCommand("alice", command{Op: "append", Tracks: []Entry{{Path: "song.flac", Duration: 180}}, Play: true}); err != nil {
		t.Fatal(err)
	}
	detach, err := s.attach("device", func() *Player { return &Player{Kind: Device, Name: "Speaker", Adopted: true, Owner: queueID("alice")} }, func(*Player) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.userCommand("alice", command{Op: "outputs", Outputs: []string{"device"}}); err != nil {
		t.Fatal(err)
	}
	p := s.players["device"]
	detach()
	t.Cleanup(func() {
		if p.disconnectTimer != nil {
			p.disconnectTimer.Stop()
		}
	})
	return s, p, s.players[queueID("alice")]
}
func TestDeviceDisconnectGraceAndReconnect(t *testing.T) {
	s, p, q := disconnectedDevice(t)
	if p.Connected || !p.reachable() || p.State != Playing || q.State != Playing || q.OutputID != p.ID || !q.Connected {
		t.Fatal("disconnect interrupted queue")
	}
	if left := time.Until(p.lingerUntil); left < 29*time.Second || left > 30*time.Second {
		t.Fatalf("grace %v", left)
	}
	generation := p.connectionGeneration
	_, err := s.attach(p.ID, func() *Player { t.Fatal("lost device"); return nil }, func(*Player) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.finishDeviceDisconnect(p, generation)
	if p.State != Playing || q.State != Playing || !p.Connected || p.disconnectTimer != nil {
		t.Fatal("old timeout paused reconnected output")
	}
}
func TestDeviceDisconnectExpiry(t *testing.T) {
	s, p, q := disconnectedDevice(t)
	p.disconnectTimer.Stop()
	s.finishDeviceDisconnect(p, p.connectionGeneration)
	if p.reachable() || p.State != Paused || q.State != Paused || q.Pause == nil || q.Pause.Source != "Output disconnected" {
		t.Fatal("expired device did not pause queue")
	}
}
func TestDeviceExplicitSourcePauseBypassesGrace(t *testing.T) {
	s, p, q := disconnectedDevice(t)
	if err := s.deviceControl(p.ID, deviceControl{Op: "pause-if-sole-output", Version: p.Version, Entry: p.Current}); err != nil {
		t.Fatal(err)
	}
	if q.State != Paused || q.Pause == nil || q.Pause.Source != "Source selection away from Slopify" {
		t.Fatal("explicit pause waited for timeout")
	}
	reason := q.Pause
	p.disconnectTimer.Stop()
	s.finishDeviceDisconnect(p, p.connectionGeneration)
	if q.Pause != reason {
		t.Fatal("timeout replaced explicit pause reason")
	}
}
