//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMPVRefusesLossyOutput(t *testing.T) {
	for _, output := range []format{{rate: 48000, channels: 2, bits: 32}, {rate: 44100, channels: 2, bits: 0}, {rate: 44100, channels: 1, bits: 32}} {
		if validateMPVFormat(format{rate: 44100, channels: 2, bits: 32}, output) == nil {
			t.Fatalf("accepted %+v", output)
		}
	}
	if validateMPVFormat(format{rate: 44100, channels: 1, bits: 16}, format{rate: 44100, channels: 2, bits: 32}) != nil {
		t.Fatal("rejected lossless mono duplication")
	}
}

// A real subprocess integration test, always using null output. Never probes
// or opens the developer's sound hardware.
func TestMPVPersistentPlaybackAndSeek(t *testing.T) {
	executable, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("mpv not installed")
	}
	file := mpvTestWAV(t, 44100)
	second := mpvTestWAV(t, 48000)
	q := &queueView{}
	q.set([]entry{{ID: 1, Bits: 32}, {ID: 2, Bits: 32}})
	events := make(chan event, 16)
	backend, err := newMPVBackend(playbackServices{queue: q, events: events, streamURL: func(_ context.Context, id uint64) (string, error) {
		if id == 2 {
			return second, nil
		}
		return file, nil
	}, decodedState: (*localStatus)(nil).decodedState}, executable, "null")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	b := backend.(*mpvBackend)
	s := b.Start(1, 0)
	defer s.close()
	wait := func(s playbackSession) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !s.finished() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !s.finished() || s.failure() {
			t.Fatalf("session did not complete normally: finished=%v failed=%v", s.finished(), s.failure())
		}
	}
	wait(s)
	for _, id := range []uint64{1, 2} {
		select {
		case e := <-events:
			if e.entry != id || !e.ended {
				t.Fatalf("unexpected completion %+v", e)
			}
		default:
			t.Fatalf("missing completion for %d", id)
		}
	}
	pid := b.client.cmd.Process.Pid
	q.set([]entry{{ID: 1, Bits: 32}})
	s2 := b.Start(1, 0.5)
	defer s2.close()
	if s2.position() != 0.5 {
		t.Fatal("seek position not initialized")
	}
	wait(s2)
	if b.client.cmd.Process.Pid != pid {
		t.Fatal("mpv was restarted between sessions")
	}
	// Pause stops the active load but retains the process. Immediate resume
	// must not mistake the previous load's stop event for a new failure.
	s3 := b.Start(1, 0)
	time.Sleep(100 * time.Millisecond)
	s3.close()
	if s3.failure() {
		t.Fatal("intentional stop marked failed")
	}
	s4 := b.Start(1, 0.8)
	defer s4.close()
	wait(s4)
	if b.client.cmd.Process.Pid != pid {
		t.Fatal("pause restarted mpv")
	}
	// Process death is recoverable and must not advance the queue as EOF.
	for len(events) > 0 {
		<-events
	}
	s5 := b.Start(1, 0)
	time.Sleep(100 * time.Millisecond)
	b.client.cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for !s5.finished() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s5.finished() || !s5.failure() {
		t.Fatal("process exit not marked failed")
	}
	if len(events) != 0 {
		t.Fatal("process death advanced queue")
	}
	s6 := b.Start(1, 0.8)
	defer s6.close()
	wait(s6)
	if b.client.cmd.Process.Pid == pid {
		t.Fatal("dead mpv process reused")
	}
}

func mpvTestWAV(t *testing.T, rate int) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "fixture.wav")
	samples := rate
	data := make([]byte, 44+samples*8)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 2)
	binary.LittleEndian.PutUint32(data[24:], uint32(samples))
	binary.LittleEndian.PutUint32(data[28:], uint32(samples*8))
	binary.LittleEndian.PutUint16(data[32:], 8)
	binary.LittleEndian.PutUint16(data[34:], 32)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(samples*8))
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestMPVQueueEditWhileSigningSuccessor(t *testing.T) {
	executable, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("mpv not installed")
	}
	file := mpvTestWAV(t, 44100)
	q := &queueView{}
	q.set([]entry{{ID: 1}, {ID: 2}})
	signing, release := make(chan struct{}), make(chan struct{})
	events := make(chan event, 8)
	b, err := newMPVBackend(playbackServices{queue: q, events: events, decodedState: (*localStatus)(nil).decodedState,
		streamURL: func(ctx context.Context, id uint64) (string, error) {
			if id == 2 {
				close(signing)
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return file, nil
		}}, executable, "null")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s := b.Start(1, 0)
	defer s.close()
	select {
	case <-signing:
	case <-time.After(3 * time.Second):
		t.Fatal("successor was not requested")
	}
	q.set([]entry{{ID: 1}, {ID: 3}})
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !s.finished() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.finished() || s.failure() {
		t.Fatal("edited queue failed")
	}
	for _, id := range []uint64{1, 3} {
		select {
		case e := <-events:
			if e.entry != id {
				t.Fatalf("played removed entry: %+v", e)
			}
		default:
			t.Fatalf("missing completion %d", id)
		}
	}
}
