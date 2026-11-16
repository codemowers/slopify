package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codemowers/slopify/player/internal/audiometer"
)

func TestTransitionSoundBoundedSmoothAndLocal(t *testing.T) {
	for _, rate := range []int{44100, 48000, 96000} {
		pcm := transitionSound(rate, 2)
		if len(pcm) != rate*220/1000*2 {
			t.Fatal("duration")
		}
		peak := int32(0)
		for i, v := range pcm {
			if v > peak {
				peak = v
			}
			if int64(v) < -cueAmplitude(cueLevelDB) || int64(v) > cueAmplitude(cueLevelDB) {
				t.Fatal("cue too loud")
			}
			if i%2 == 1 && v != pcm[i-1] {
				t.Fatal("stereo mismatch")
			}
		}
		if peak == 0 || pcm[0] != 0 || pcm[len(pcm)-1] != 0 {
			t.Fatal("cue envelope")
		}
	}
	r, devices, _ := testRouter()
	r.selectSource("slopify")
	r.write("slopify", format{rate: 44100, channels: 2, bits: 16}, []int32{1, 2})
	r.selectSourceWithCue("lan_1", true)
	if devices.writes["transition"] == 0 || r.owns("lan_1") || r.transition != nil {
		t.Fatal("cue must drain and release before source opens")
	}
	before := devices.writes["transition"]
	r.selectSourceWithCue("lan_1", true)
	r.selectSourceWithCue("slopify", false)
	if devices.writes["transition"] != before {
		t.Fatal("same-source/disabled cue repeated")
	}
}
func TestPriorityTakeoverRequiresContinuousSound(t *testing.T) {
	now := time.Now()
	hi := &speakerSource{ID: "lan_1", Connected: true, Sounding: true, AudibleSince: now, SignalAt: now, meter: audiometer.New()}
	hi.meter.Observe((1<<31)/10, now)
	low := &speakerSource{ID: "slopify", Connected: true, LastAudible: now, meter: audiometer.New()}
	s := &speakerSources{Mode: "priority", Selected: "slopify", PrioritySilenceMS: 30000, PriorityTakeoverMS: 2000, Sources: []*speakerSource{hi, low}}
	hi.SignalAt = now.Add(1900 * time.Millisecond)
	hi.meter.Observe((1<<31)/10, hi.SignalAt)
	s.chooseSource(hi.SignalAt)
	if s.Selected != "slopify" {
		t.Fatal("early takeover")
	}
	hi.AudibleSince = time.Time{}
	hi.SignalAt = now.Add(2 * time.Second)
	s.chooseSource(hi.SignalAt)
	if s.Selected != "slopify" {
		t.Fatal("terminal bell silence took over")
	}
	hi.AudibleSince = now.Add(3 * time.Second)
	hi.SignalAt = now.Add(5 * time.Second)
	hi.meter.Observe((1<<31)/10, hi.SignalAt)
	s.chooseSource(hi.SignalAt)
	if s.Selected != "lan_1" {
		t.Fatal("sustained sound did not take over")
	}
	s.Selected = "slopify"
	s.PriorityTakeoverMS = 0
	hi.AudibleSince = hi.SignalAt
	s.chooseSource(hi.SignalAt)
	if s.Selected != "lan_1" {
		t.Fatal("zero delay not immediate")
	}
}
func TestTransitionSettingsPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	s := newSpeakerSources(file)
	if s.PriorityTakeoverMS != 0 || !s.TransitionSounds {
		t.Fatal("defaults")
	}
	s.PriorityTakeoverMS = 2000
	s.TransitionSounds = false
	s.save()
	restored := newSpeakerSources(file)
	if restored.PriorityTakeoverMS != 2000 || restored.TransitionSounds {
		t.Fatal("settings lost")
	}
}

func TestSeekAndStallSoundsBoundedAndSeamless(t *testing.T) {
	for _, rate := range []int{44100, 48000, 96000} {
		for name, pcm := range map[string][]int32{"seek": seekSound(rate, 2), "stall": stallSound(rate, 2), "pause": pauseSound(rate, 2)} {
			limit := int32(cueAmplitude(cueLevelDB))
			if name == "stall" {
				limit = int32(cueAmplitude(cueLevelDB - 6))
			}
			var peak int32
			for i, v := range pcm {
				peak = max(peak, v)
				if v < -limit || v > limit {
					t.Fatalf("%s too loud", name)
				}
				if i%2 == 1 && v != pcm[i-1] {
					t.Fatalf("%s stereo mismatch", name)
				}
			}
			if peak == 0 || pcm[0] != 0 || pcm[len(pcm)-1] != 0 {
				t.Fatalf("%s envelope", name)
			}
		}
		if len(seekSound(rate, 2)) == len(transitionSound(rate, 2)) {
			t.Fatal("seek sound should be distinct from the transition sound")
		}
	}
}

func TestPauseSoundRespectsRoutingAndSetting(t *testing.T) {
	r, devices, _ := testRouter()
	f := format{44100, 2, 24}
	b := &nativeBackend{last: f, pauseCue: r.playPauseSound, cues: func() bool { return true }}
	r.selectSource("lan_1")
	b.PauseSound()
	if devices.writes["transition"] != 0 {
		t.Fatal("pause interrupted LAN source")
	}
	r.selectSource("slopify")
	b.PauseSound()
	before := devices.writes["transition"]
	if before == 0 || r.transition != nil {
		t.Fatal("pause cue not played and released")
	}
	b.cues = func() bool { return false }
	b.PauseSound()
	if devices.writes["transition"] != before {
		t.Fatal("disabled pause cue played")
	}
}

type pauseTestBackend struct {
	queueTestBackend
	session *queueTestSession
	calls   int
	t       *testing.T
}

func (b *pauseTestBackend) PauseSound() {
	if !b.session.closed {
		b.t.Fatal("cue before session closed")
	}
	b.calls++
}
func TestPauseCueOnlyOnPlayingTransition(t *testing.T) {
	for _, prior := range []string{"playing", "paused", ""} {
		s := &queueTestSession{}
		b := &pauseTestBackend{session: s, t: t}
		c := &controller{backend: b, sess: s, queue: queueView{}, applied: 1}
		if prior != "" {
			c.desired = &state{State: prior, Current: 7}
		}
		paused := &state{State: "paused", Current: 7, Adopted: true, Version: 1, Queue: []entry{{ID: 7}}}
		c.reconcile(paused)
		c.reconcile(paused)
		want := 0
		if prior == "playing" {
			want = 1
		}
		if b.calls != want {
			t.Fatalf("prior %q: got %d cues, want %d", prior, b.calls, want)
		}
	}
}

func TestStartupCueRepeatsUntilAudioAndCancels(t *testing.T) {
	chunks := make(chan nativeChunk, 2)
	f := format{44100, 2, 24}
	chunks <- nativeChunk{begin: &f}
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pending := waitStartupAudio(ctx, chunks, func() {
		calls++
		if calls == 3 {
			chunks <- nativeChunk{samples: []int32{1, 2}}
		}
	}, time.Millisecond)
	if calls != 3 || len(pending) != 2 || len(pending[1].samples) != 2 {
		t.Fatalf("calls=%d pending=%v", calls, pending)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	waitStartupAudio(canceled, chunks, func() { t.Fatal("cue after cancellation") }, time.Millisecond)
	chunks <- nativeChunk{failure: context.Canceled}
	pending = waitStartupAudio(ctx, chunks, func() {}, time.Millisecond)
	if len(pending) != 1 || pending[0].failure == nil {
		t.Fatal("startup failure swallowed")
	}
}

func TestCueLevelAmplitude(t *testing.T) {
	previous := cueLevelDB
	defer func() { cueLevelDB = previous }()
	last := int64(0)
	for _, db := range []int{-60, -30, -12, -6, 0} {
		cueLevelDB = db
		peak := int64(0)
		for _, v := range transitionSound(48000, 2) {
			x := int64(v)
			if x < 0 {
				x = -x
			}
			peak = max(peak, x)
		}
		ceiling := cueAmplitude(db)
		if peak > ceiling || peak < ceiling*9/10 || peak <= last {
			t.Fatalf("db=%d peak=%d ceiling=%d", db, peak, ceiling)
		}
		last = peak
	}
}

type cueTrackSession struct {
	queueTestSession
	id uint64
}

func (s *cueTrackSession) current() uint64 { return s.id }

type cueTrackBackend struct {
	queueTestBackend
	announced int
}

func (b *cueTrackBackend) StartAnnounced(id uint64, at float64) playbackSession {
	b.announced++
	return &cueTrackSession{id: id}
}
func TestSelectedTrackCuePreservesGaplessAdvance(t *testing.T) {
	for _, current := range []uint64{7, 8} {
		b := &cueTrackBackend{}
		c := &controller{backend: b, sess: &cueTrackSession{id: current}, applied: 1,
			desired: &state{State: "playing", Current: 7, Version: 1}}
		next := &state{State: "playing", Current: 8, Version: 2, Adopted: true, Queue: []entry{{ID: 8}}}
		c.reconcile(next)
		c.reconcile(next)
		want := 0
		if current == 7 {
			want = 1
		}
		if b.announced != want {
			t.Fatalf("session track=%d got %d cues want %d", current, b.announced, want)
		}
	}
}

type cueBufferedOutput struct{ fakeOutput }

func (o *cueBufferedOutput) BufferFrames() int { return 4800 }

func TestNoWindDownDuringStartupOrCancellation(t *testing.T) {
	for _, cancelOnOpen := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		samples := []int32{}
		formats := []format{}
		f := format{48000, 2, 16}
		b := &nativeBackend{playbackServices: playbackServices{decodedState: func(string) {}}}
		b.open = func(format) (nativeOutput, error) {
			if cancelOnOpen {
				cancel()
			}
			return &cueBufferedOutput{fakeOutput{f: f, samples: &samples, formats: &formats}}, nil
		}
		chunks := make(chan nativeChunk, 2)
		chunks <- nativeChunk{entry: 1, begin: &f}
		chunks <- nativeChunk{entry: 1, samples: make([]int32, 960)}
		done := make(chan struct{})
		go func() { b.output(ctx, &session{first: 1}, chunks, false, false); close(done) }()
		timer := time.AfterFunc(50*time.Millisecond, cancel)
		<-done
		timer.Stop()
		cancel()
		if len(samples) > 960 {
			t.Fatalf("cancelOnOpen=%v: startup wrote %d extra cue samples", cancelOnOpen, len(samples)-960)
		}
	}
}
