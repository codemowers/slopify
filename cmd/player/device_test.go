package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// As aplay prints it for a USB DAC that takes only 32-bit stereo.
const usbDAC = `HW Params of device "hw:CARD=PMA60,DEV=0":
--------------------
ACCESS:  MMAP_INTERLEAVED RW_INTERLEAVED
FORMAT:  S32_LE SPECIAL
SUBFORMAT:  STD
SAMPLE_BITS: 32
FRAME_BITS: 64
CHANNELS: 2
RATE: [44100 384000]
PERIOD_TIME: (83 2972155)
--------------------
`

func TestParseHWParams(t *testing.T) {
	c, err := parseHWParams(usbDAC)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.formats, ",") != "S32_LE,SPECIAL" || c.minCh != 2 || c.maxCh != 2 || c.minRate != 44100 || c.maxRate != 384000 {
		t.Errorf("parsed %+v", c)
	}
	if lo, hi := parseRange("(44099 44101]"); lo != 44100 || hi != 44101 {
		t.Errorf("parseRange = %d %d", lo, hi)
	}
}

func TestFit(t *testing.T) {
	c, _ := parseHWParams(usbDAC)
	cv, err := c.fit(format{rate: 44100, channels: 1, bits: 24})
	if err != nil || cv.alsa != "S32_LE" || cv.channels != 2 {
		t.Errorf("fit = %+v, %v", cv, err)
	}
	if _, err := c.fit(format{rate: 22050, channels: 2, bits: 16}); err == nil {
		t.Error("22.05 kHz fits a device starting at 44.1 kHz")
	}
	i2s := caps{formats: []string{"S16_LE", "S24_LE", "S32_LE"}, minCh: 2, maxCh: 2, minRate: 44100, maxRate: 192000}
	if cv, _ := i2s.fit(format{rate: 96000, channels: 2, bits: 16}); !cv.identity() {
		t.Errorf("16-bit stereo on a device taking S16_LE is converted: %+v", cv)
	}
}

func TestConversion(t *testing.T) {
	c, _ := parseHWParams(usbDAC)
	// Mono 16-bit: -2 and 0x1234 become stereo 32-bit words with the
	// samples in the top 16 bits.
	cv, _ := c.fit(format{rate: 44100, channels: 1, bits: 16})
	in := []byte{0xfe, 0xff, 0x34, 0x12}
	want := make([]byte, 16)
	for i, v := range []int32{-2 << 16, -2 << 16, 0x1234 << 16, 0x1234 << 16} {
		binary.LittleEndian.PutUint32(want[i*4:], uint32(v))
	}
	if got := cv.apply(in); !bytes.Equal(got, want) {
		t.Errorf("16-bit mono → % x, want % x", got, want)
	}
	// Stereo 24-bit in S24_LE words gains eight zero low bits.
	cv, _ = c.fit(format{rate: 48000, channels: 2, bits: 24})
	in = make([]byte, 8)
	neg := int32(-0x123456)
	binary.LittleEndian.PutUint32(in, uint32(neg))
	binary.LittleEndian.PutUint32(in[4:], 0x7fffff)
	got := cv.apply(in)
	if v := int32(binary.LittleEndian.Uint32(got)); v != -0x123456<<8 {
		t.Errorf("left = %x", v)
	}
	if v := int32(binary.LittleEndian.Uint32(got[4:])); v != 0x7fffff<<8 {
		t.Errorf("right = %x", v)
	}
}
