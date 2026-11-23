package main

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/rtp"
)

func multicastTestSender() *rtp.Sender {
	return rtp.NewSender(rtp.SenderConfig{Group: netip.MustParseAddrPort("239.192.1.2:5004")})
}

func TestNativeMulticastDelayPreservesTracksAndPCM(t *testing.T) {
	files := map[uint64][]byte{1: wavFile(1000, 200, -100), 2: wavFile(1000, 200, 1<<24)}
	b, formats, samples, events := nativeHarness(t, files, []entry{{ID: 1}, {ID: 2}}, 0)
	b.sender = multicastTestSender()
	b.multicastLocalDelay = 60 * time.Millisecond
	started := time.Now()
	s := b.Start(1, 0)
	defer s.close()
	for _, id := range []uint64{1, 2} {
		select {
		case e := <-events:
			if e.entry != id || !e.ended {
				t.Fatalf("unexpected event %+v", e)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("delayed playback did not finish")
		}
	}
	s.close()
	if elapsed := time.Since(started); elapsed < b.multicastLocalDelay+180*time.Millisecond {
		t.Fatalf("local timeline completed too early: %v", elapsed)
	}
	if len(*formats) != 1 || len(*samples) != 400 {
		t.Fatalf("formats %v, samples %d", *formats, len(*samples))
	}
	for i := 0; i < 200; i++ {
		if (*samples)[i] != int32(i)-100 || (*samples)[200+i] != 1<<24+int32(i) {
			t.Fatalf("PCM changed at %d", i)
		}
	}
	if s.failure() {
		t.Fatal("delayed playback failed")
	}
}

func TestMulticastFeedBypassesDelayWhenGatedAndReanchorsOnResume(t *testing.T) {
	b := &nativeBackend{sender: multicastTestSender(), multicastLocalDelay: 200 * time.Millisecond, playbackServices: playbackServices{queue: &queueView{}}}
	b.sender.SetEnabled(false)
	input := make(chan nativeChunk, 4)
	f := format{rate: 1000, channels: 2, bits: 32}
	input <- nativeChunk{begin: &f}
	input <- nativeChunk{samples: make([]int32, nativeChunkSamples)[:20]}
	out, stop := b.multicastFeed(context.Background(), input, nil)
	defer stop()
	<-out // track boundary
	c := <-out
	if !c.localAt.IsZero() || c.feedAck == nil {
		t.Fatal("disabled sender delayed or read ahead of local playback")
	}
	b.sender.SetEnabled(true)
	close(c.feedAck)
	resumed := time.Now()
	input <- nativeChunk{samples: make([]int32, nativeChunkSamples)[:20]}
	c = <-out
	if !c.localCopy || c.localAt.Before(resumed.Add(b.multicastLocalDelay)) || c.localAt.After(time.Now().Add(b.multicastLocalDelay)) {
		t.Fatalf("resume did not anchor a new local deadline: %v", c.localAt)
	}
	// An interruption longer than the old deadline starts a fresh timeline.
	time.Sleep(130 * time.Millisecond)
	resumed = time.Now()
	input <- nativeChunk{samples: make([]int32, nativeChunkSamples)[:20]}
	c = <-out
	if c.localAt.Before(resumed.Add(b.multicastLocalDelay)) {
		t.Fatal("interrupted input retained an expired timeline")
	}
}

func TestNativeMulticastDelayCancellation(t *testing.T) {
	b, _, _, _ := nativeHarness(t, map[uint64][]byte{1: wavFile(1000, 200, 0)}, []entry{{ID: 1}}, 0)
	b.sender = multicastTestSender()
	b.multicastLocalDelay = 2 * time.Second
	opened := make(chan struct{})
	open := b.open
	b.open = func(f format) (nativeOutput, error) { close(opened); return open(f) }
	s := b.Start(1, 0)
	defer s.close()
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("device was not opened")
	}
	start := time.Now()
	s.close()
	if time.Since(start) > 300*time.Millisecond {
		t.Fatal("stop waited for multicast delay")
	}
}
