package pcmring

import (
	"bytes"
	"testing"
	"time"
)

func TestRingKeepsOrderAcrossItsEnd(t *testing.T) {
	r := New(16, 4, 4, nil)
	buf := make([]byte, 12)
	for i := byte(0); i < 5; i++ {
		in := bytes.Repeat([]byte{i, i + 1, i + 2, i + 3}, 3)
		if !r.Write(in) {
			t.Fatal("write refused")
		}
		if n := r.Read(buf); n != 12 || !bytes.Equal(buf, in) {
			t.Fatal(n, buf)
		}
	}
}

func TestRingBuffersUntilThresholdAndAfterUnderflow(t *testing.T) {
	r := New(16, 8, 4, nil)
	buf := make([]byte, 16)
	r.Write(make([]byte, 4))
	if r.Read(buf) != 0 {
		t.Fatal("played before the threshold")
	}
	r.Write(make([]byte, 4))
	if r.Read(buf) != 8 {
		t.Fatal("did not start")
	}
	if _, underflows := r.Stats(); underflows != 1 {
		t.Fatal(underflows)
	}
	r.Write(make([]byte, 4))
	if r.Read(buf) != 0 {
		t.Fatal("did not rebuffer")
	}
}

func TestRingDrainsWholeFramesAfterFinish(t *testing.T) {
	closed := 0
	r := New(16, 12, 4, func() { closed++ })
	r.Write(make([]byte, 6)) // A frame and a fragment.
	r.Finish()
	if n := r.Read(make([]byte, 16)); n != 4 {
		t.Fatal(n)
	}
	select {
	case <-r.Done():
	case <-time.After(time.Second):
		t.Fatal("did not close")
	}
	r.Close()
	if closed != 1 {
		t.Fatal(closed)
	}
}

func TestCloseReleasesBlockedWriter(t *testing.T) {
	r := New(4, 4, 4, nil)
	result := make(chan bool)
	go func() { result <- r.Write(make([]byte, 64)) }()
	time.Sleep(20 * time.Millisecond)
	r.Close()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("write reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("writer stayed blocked")
	}
}
