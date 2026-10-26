package main

import (
	"encoding/binary"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// caps is what an ALSA device accepts, as aplay reports it.
type caps struct {
	formats          []string
	minCh, maxCh     int
	minRate, maxRate int
}

// probeAplay reads the device's hardware parameters. aplay prints them
// before applying any settings, so the throwaway playback of /dev/null
// that follows does not matter.
func probeAplay(device string) (caps, error) {
	out, _ := exec.Command("aplay", "-D", device, "--dump-hw-params",
		"-t", "raw", "-f", "S16_LE", "-c", "2", "-r", "44100", "-d", "1", "/dev/null").CombinedOutput()
	c, err := parseHWParams(string(out))
	if err != nil {
		return caps{}, fmt.Errorf("probing %s: %w: %s", device, err, strings.TrimSpace(string(out)))
	}
	return c, nil
}

func parseHWParams(dump string) (caps, error) {
	var c caps
	found := 0
	for _, line := range strings.Split(dump, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "FORMAT":
			c.formats = strings.Fields(value)
			found++
		case "CHANNELS":
			c.minCh, c.maxCh = parseRange(value)
			found++
		case "RATE":
			c.minRate, c.maxRate = parseRange(value)
			found++
		}
	}
	if found < 3 {
		return caps{}, fmt.Errorf("no hardware parameters reported")
	}
	return c, nil
}

// parseRange reads ALSA intervals such as "2", "[1 2]" or "(44099 44101]".
func parseRange(s string) (lo, hi int) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, 0
	}
	num := func(t string) int {
		n, _ := strconv.Atoi(strings.Trim(t, "[]()"))
		return n
	}
	lo, hi = num(f[0]), num(f[len(f)-1])
	if strings.HasPrefix(f[0], "(") {
		lo++
	}
	if strings.HasSuffix(f[len(f)-1], ")") {
		hi--
	}
	return lo, hi
}

// conversion carries samples into a format the device accepts without
// changing them: wider words get zero low bits, and mono is copied to both
// channels. The sample rate is never changed.
type conversion struct {
	alsa     string
	bits     int // significant bits on the device side
	width    int // bytes per sample on the device side
	channels int
	src      format
}

// Device formats that can hold samples of a given bit depth, best first.
var containers = map[int][]struct {
	alsa        string
	bits, width int
}{
	16: {{"S16_LE", 16, 2}, {"S24_LE", 24, 4}, {"S32_LE", 32, 4}, {"S24_3LE", 24, 3}},
	24: {{"S24_LE", 24, 4}, {"S32_LE", 32, 4}, {"S24_3LE", 24, 3}},
	32: {{"S32_LE", 32, 4}},
}

func (c caps) fit(f format) (conversion, error) {
	if f.rate < c.minRate || f.rate > c.maxRate {
		return conversion{}, fmt.Errorf("device does not play %d Hz (%d–%d Hz); not resampling", f.rate, c.minRate, c.maxRate)
	}
	ch := f.channels
	if ch < c.minCh || ch > c.maxCh {
		if ch != 1 || c.minCh > 2 || c.maxCh < 2 {
			return conversion{}, fmt.Errorf("device does not take %d channels (%d–%d)", ch, c.minCh, c.maxCh)
		}
		ch = 2
	}
	for _, k := range containers[f.bits] {
		if slices.Contains(c.formats, k.alsa) {
			return conversion{alsa: k.alsa, bits: k.bits, width: k.width, channels: ch, src: f}, nil
		}
	}
	return conversion{}, fmt.Errorf("device takes none of the formats that hold %d-bit samples: %v", f.bits, c.formats)
}

func (cv conversion) identity() bool {
	return cv.channels == cv.src.channels && cv.width == cv.src.container() && cv.bits == cv.src.bits
}

func (cv conversion) apply(b []byte) []byte {
	if cv.identity() {
		return b
	}
	sw := cv.src.container()
	shift := cv.bits - cv.src.bits
	copies := cv.channels / cv.src.channels
	out := make([]byte, 0, len(b)/sw*cv.width*copies)
	var word [4]byte
	for i := 0; i+sw <= len(b); i += sw {
		var v int32
		if sw == 2 {
			v = int32(int16(binary.LittleEndian.Uint16(b[i:])))
		} else {
			v = int32(binary.LittleEndian.Uint32(b[i:]))
		}
		binary.LittleEndian.PutUint32(word[:], uint32(v<<shift))
		for range copies {
			out = append(out, word[:cv.width]...)
		}
	}
	return out
}
