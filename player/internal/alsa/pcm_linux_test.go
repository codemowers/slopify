//go:build linux

package alsa

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"unsafe"
)

func TestStructureSizes(t *testing.T) {
	want := map[uintptr][2]uintptr{4: {604, 104}, 8: {608, 136}}[unsafe.Sizeof(uintptr(0))]
	if got := unsafe.Sizeof(hwParams{}); got != want[0] {
		t.Fatalf("hw params %d, want %d", got, want[0])
	}
	if got := unsafe.Sizeof(swParams{}); got != want[1] {
		t.Fatalf("sw params %d, want %d", got, want[1])
	}
}

// Set SLOPIFY_TEST_ALSA_LOOPBACK=Loopback, with snd-aloop loaded and idle, to
// send full-scale counters through real ioctls and compare what arrives.
func TestLoopbackIsBitExact(t *testing.T) {
	card := os.Getenv("SLOPIFY_TEST_ALSA_LOOPBACK")
	if card == "" {
		t.Skip("SLOPIFY_TEST_ALSA_LOOPBACK unset")
	}
	for _, rate := range []int{44100, 48000, 96000} {
		out, err := Open("hw:"+card+",0", false, FormatS32LE, rate, 2)
		if err != nil {
			t.Fatal(err)
		}
		in, err := Open("hw:"+card+",1", true, FormatS32LE, rate, 2)
		if err != nil {
			t.Fatal(err)
		}
		cfg := out.Config()
		t.Logf("%d Hz: period %d, buffer %d", rate, cfg.PeriodFrames, cfg.BufferFrames)
		const frames = 1 << 16
		send := make([]byte, frames*8)
		for i := 0; i < frames*2; i++ {
			binary.LittleEndian.PutUint32(send[i*4:], uint32(i+1)*0x9e3779b1|1)
		}
		// Capture must be running before playback or the start of the signal is lost.
		got := make([]byte, 0, len(send)+cfg.BufferFrames*16)
		buf := make([]byte, cfg.PeriodFrames*8)
		if n, err := in.ReadFrames(buf); err != nil {
			t.Fatal(err)
		} else {
			got = append(got, buf[:n*8]...)
		}
		done := make(chan error, 1)
		go func() {
			// The loopback driver loses a partial period on drain, so the whole
			// signal must be followed by silence to pass through completely.
			_, err := out.WriteFrames(send)
			if err == nil {
				_, err = out.WriteFrames(make([]byte, cfg.BufferFrames*8))
			}
			if err == nil {
				err = out.Drain()
			}
			done <- err
		}()
		for len(got) < len(send)+cfg.BufferFrames*8 {
			n, err := in.ReadFrames(buf)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, buf[:n*8]...)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		in.Drop()
		in.Close()
		out.Close()
		first := 0
		for first+4 <= len(got) && binary.LittleEndian.Uint32(got[first:]) == 0 {
			first += 4
		}
		if first+len(send) > len(got) {
			t.Fatalf("captured %d bytes after %d of silence", len(got)-first, first)
		}
		for i := 0; i+4 <= len(send); i += 4 {
			if string(got[first+i:first+i+4]) != string(send[i:i+4]) {
				at := -1
				for j := 0; j+64 < len(send); j += 4 { // Where did this data really come from?
					if string(send[j:j+16]) == string(got[first+i:first+i+16]) {
						at = j
						break
					}
				}
				t.Fatalf("%d Hz: leading silence %d bytes; first difference at byte %d of %d; got %x want %x; got data matches sent offset %d",
					rate, first, i, len(send), got[first+i:first+i+8], send[i:i+8], at)
			}
		}
	}
}

func TestControlStructureSizes(t *testing.T) {
	if got := unsafe.Sizeof(elemInfo{}); got != 272 {
		t.Fatalf("elem info %d", got)
	}
	want := map[uintptr]uintptr{4: 712, 8: 1224}[unsafe.Sizeof(uintptr(0))]
	if got := unsafe.Sizeof(elemValue{}); got != want {
		t.Fatalf("elem value %d, want %d", got, want)
	}
}

// Set SLOPIFY_TEST_ALSA_CONTROL=card:Name to read a live volume and switch and
// write back the values already set, which leaves the device unchanged.
func TestLiveControlRoundTrip(t *testing.T) {
	spec := os.Getenv("SLOPIFY_TEST_ALSA_CONTROL")
	if spec == "" {
		t.Skip("SLOPIFY_TEST_ALSA_CONTROL unset")
	}
	card, name, _ := strings.Cut(spec, ":")
	m, err := OpenMixer(card)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Integer(name + " Playback Volume")
	if err != nil {
		t.Fatal(err)
	}
	values, err := m.Values(name + " Playback Volume")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("range %d..%d, channels %d, scale %+v, values %v", c.Min, c.Max, c.Channels, c.Scale, values)
	if err := m.SetValue(name+" Playback Volume", values[0]); err != nil {
		t.Fatal(err)
	}
	after, err := m.Values(name + " Playback Volume")
	if err != nil || after[0] != values[0] {
		t.Fatalf("after write: %v, %v", after, err)
	}
	switches, err := m.Switches(name + " Playback Switch")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("switch %v", switches)
	if err := m.SetSwitch(name+" Playback Switch", switches[0] != 0); err != nil {
		t.Fatal(err)
	}
}
