//go:build !no_alac

package audiodec

import (
	"encoding/binary"
	"math/rand"
	"os/exec"
	"strconv"
	"testing"
)

// cafPackets splits a CAF file into its ALAC magic cookie and packets.
func cafPackets(t *testing.T, data []byte) (cookie []byte, packets [][]byte) {
	t.Helper()
	var audio, table []byte
	for pos := 8; pos+12 <= len(data); {
		kind := string(data[pos : pos+4])
		size := int(binary.BigEndian.Uint64(data[pos+4:]))
		body := data[pos+12:]
		if size < 0 || size > len(body) {
			size = len(body)
		}
		switch kind {
		case "kuki":
			cookie = body[:size]
		case "data":
			audio = body[4:size] // After the edit count.
		case "pakt":
			table = body[24:size]
		}
		pos += 12 + size
	}
	for len(table) > 0 {
		var size int
		for {
			b := table[0]
			table = table[1:]
			size = size<<7 | int(b&0x7f)
			if b&0x80 == 0 {
				break
			}
		}
		packets = append(packets, audio[:size])
		audio = audio[size:]
	}
	return cookie, packets
}

func TestALACMatchesFFmpeg(t *testing.T) {
	for _, tc := range []struct {
		name, sampleFormat string
		channels           int
	}{
		{"16 bit stereo", "s16p", 2},
		{"16 bit mono", "s16p", 1},
		{"24 bit stereo", "s32p", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixture(t, "a.caf", 44100, "-ac", strconv.Itoa(tc.channels), "-c:a", "alac", "-sample_fmt", tc.sampleFormat)
			want := reference(t, data, "a.caf")
			cookie, packets := cafPackets(t, data)
			config, err := ParseALACConfig(cookie)
			if err != nil {
				t.Fatal(err)
			}
			decoder, err := NewALAC(config)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]int32, int(config.FrameLength)*config.Channels)
			var got []int32
			for i, packet := range packets {
				n, err := decoder.Decode(packet, buf)
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
				got = append(got, buf[:n*config.Channels]...)
			}
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("sample %d: %d, want %d", i, got[i], want[i])
				}
			}
		})
	}
}

// TestALACDecodesSilenceRuns exercises the zero-run path, which noise never reaches.
func TestALACDecodesSilenceRuns(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	data := fixture(t, "a.caf", 44100, "-af", "volume=enable='between(t,1,2)':volume=0", "-c:a", "alac", "-sample_fmt", "s16p")
	want := reference(t, data, "a.caf")
	cookie, packets := cafPackets(t, data)
	config, err := ParseALACConfig(cookie)
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := NewALAC(config)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int32, int(config.FrameLength)*2)
	at := 0
	for i, packet := range packets {
		n, err := decoder.Decode(packet, buf)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		for j, v := range buf[:n*2] {
			if v != want[at+j] {
				t.Fatalf("packet %d sample %d: %d, want %d", i, j, v, want[at+j])
			}
		}
		at += n * 2
	}
}

// uncompressedALAC builds the frame that PipeWire and PulseAudio RAOP senders emit.
func uncompressedALAC(samples []int16) []byte {
	var bit int
	out := make([]byte, 16+len(samples)*2)
	put := func(v uint32, n int) {
		for i := n - 1; i >= 0; i-- {
			if v>>i&1 == 1 {
				out[bit>>3] |= 0x80 >> (bit & 7)
			}
			bit++
		}
	}
	put(1, 3)
	put(0, 16)
	put(1, 1)
	put(0, 2)
	put(1, 1)
	put(uint32(len(samples)/2), 32)
	for _, s := range samples {
		put(uint32(uint16(s)), 16)
	}
	put(7, 3)
	return out[:bit/8+1]
}

func TestALACUncompressedFrames(t *testing.T) {
	decoder, err := NewALAC(ALACConfig{FrameLength: 352, BitDepth: 16, Channels: 2, HistoryMult: 40, InitialHistory: 10, Limit: 14, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]int32, 704)
	n, err := decoder.Decode(uncompressedALAC([]int16{1, -2, 32767, -32768}), dst)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	for i, want := range []int32{1 << 16, -2 << 16, 32767 << 16, -32768 << 16} {
		if dst[i] != want {
			t.Fatalf("sample %d: %d, want %d", i, dst[i], want)
		}
	}
	// PipeWire sends 7 + 4n bytes: the end tag that would need one more byte is cut off.
	samples := make([]int16, 704)
	samples[703] = -1
	full := uncompressedALAC(samples)
	if n, err := decoder.Decode(full[:7+4*352], dst); err != nil || n != 352 || dst[703] != -1<<16 {
		t.Fatalf("frame without end tag: %d %v", n, err)
	}
	if _, err := decoder.Decode(full[:7+4*351], dst); err == nil {
		t.Fatal("accepted a frame missing samples")
	}
}

// Packets come straight off the network, so damage must never panic or overrun.
func TestALACRejectsDamagedPackets(t *testing.T) {
	data := fixture(t, "a.caf", 44100, "-c:a", "alac", "-sample_fmt", "s16p")
	cookie, packets := cafPackets(t, data)
	config, err := ParseALACConfig(cookie)
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := NewALAC(config)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]int32, int(config.FrameLength)*2)
	random := rand.New(rand.NewSource(1))
	for range 3000 {
		packet := append([]byte(nil), packets[random.Intn(len(packets))]...)
		for range 1 + random.Intn(8) {
			packet[random.Intn(len(packet))] ^= byte(1 << random.Intn(8))
		}
		decoder.Decode(packet[:random.Intn(len(packet)+1)], buf)
	}
	if _, err := decoder.Decode(nil, buf); err == nil {
		t.Fatal("accepted an empty packet")
	}
	if _, err := decoder.Decode(packets[0], buf[:2]); err == nil {
		t.Fatal("accepted a small buffer")
	}
}

func TestALACConfigValidation(t *testing.T) {
	for _, config := range []ALACConfig{
		{FrameLength: 352, BitDepth: 8, Channels: 2},
		{FrameLength: 352, BitDepth: 16, Channels: 6},
		{FrameLength: 0, BitDepth: 16, Channels: 2},
		{FrameLength: alacMaxFrame + 1, BitDepth: 16, Channels: 2},
	} {
		if _, err := NewALAC(config); err == nil {
			t.Fatalf("accepted %+v", config)
		}
	}
	if _, err := ParseALACConfig(make([]byte, 10)); err == nil {
		t.Fatal("accepted a short config")
	}
}
