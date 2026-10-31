package audiodec

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestOpenStreamLPCMIsBigEndian(t *testing.T) {
	d, err := OpenStream(bytes.NewReader([]byte{0x12, 0x34, 0xff, 0xff, 0x7f}), "audio/L16;rate=44100;channels=2")
	if err != nil {
		t.Fatal(err)
	}
	if f := d.Format(); f.Rate != 44100 || f.Channels != 2 || f.Bits != 16 {
		t.Fatalf("%+v", f)
	}
	dst := make([]int32, 4)
	n, err := d.Read(dst)
	if err != nil || n != 2 || dst[0] != 0x1234<<16 || dst[1] != -1<<16 {
		t.Fatal(n, err, dst)
	}
	if _, err := d.Read(dst); err != io.EOF {
		t.Fatal(err)
	}
	d, err = OpenStream(bytes.NewReader([]byte{0x12, 0x34, 0x56}), "audio/L24;rate=96000;channels=1")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := d.Read(dst[:1]); err != nil || n != 1 || dst[0] != 0x12345600 {
		t.Fatal(n, err, dst[0])
	}
}

func TestOpenStreamRejectsBadLPCM(t *testing.T) {
	for _, ct := range []string{"audio/L16", "audio/L16;rate=1", "audio/L16;rate=44100;channels=99", "audio/L8;rate=44100"} {
		if _, err := OpenStream(bytes.NewReader(make([]byte, 16)), ct); err == nil {
			t.Fatal(ct)
		}
	}
	if _, err := OpenStream(bytes.NewReader(make([]byte, 16)), "audio/mpeg"); err == nil {
		t.Fatal("unknown content accepted")
	}
}

func TestOpenStreamWAVWithEndlessDataChunk(t *testing.T) {
	for _, magic := range []string{"RIFF", "RF64"} {
		var w bytes.Buffer
		w.WriteString(magic)
		binary.Write(&w, binary.LittleEndian, uint32(0xffffffff))
		w.WriteString("WAVE")
		w.WriteString("junk")
		binary.Write(&w, binary.LittleEndian, uint32(3))
		w.Write([]byte{1, 2, 3, 0}) // Padded to even.
		w.WriteString("fmt ")
		binary.Write(&w, binary.LittleEndian, uint32(16))
		for _, v := range []any{uint16(1), uint16(2), uint32(48000), uint32(192000), uint16(4), uint16(16)} {
			binary.Write(&w, binary.LittleEndian, v)
		}
		w.WriteString("data")
		binary.Write(&w, binary.LittleEndian, uint32(0xffffffff))
		w.Write([]byte{0x34, 0x12, 0xff, 0xff})
		d, err := OpenStream(io.MultiReader(&w), "audio/vnd.wave;codec=1")
		if err != nil {
			t.Fatal(magic, err)
		}
		if f := d.Format(); f.Rate != 48000 || f.Channels != 2 {
			t.Fatalf("%+v", f)
		}
		dst := make([]int32, 2)
		if n, err := d.Read(dst); err != nil || n != 2 || dst[0] != 0x1234<<16 || dst[1] != -1<<16 {
			t.Fatal(n, err, dst)
		}
	}
}
