//go:build !no_alac

package audiodec

import (
	"errors"
	"fmt"
	"math/bits"
)

const alacMaxFrame = 1 << 16

// ALAC decodes Apple Lossless packets using integer arithmetic only. It has no
// container knowledge, so a network receiver or a file demuxer can feed it alike.
// Mono and stereo are supported. A value is not safe for concurrent use.
type ALAC struct {
	config ALACConfig
	errs   [2][]int32
	out    [2][]int32
	extra  [2][]int32
	coefs  [2][32]int32
}

// NewALAC validates config and allocates all the buffers decoding needs.
func NewALAC(config ALACConfig) (*ALAC, error) {
	switch config.BitDepth {
	case 16, 20, 24, 32:
	default:
		return nil, fmt.Errorf("alac: unsupported bit depth %d", config.BitDepth)
	}
	if config.Channels != 1 && config.Channels != 2 {
		return nil, fmt.Errorf("alac: unsupported channel count %d", config.Channels)
	}
	if config.FrameLength == 0 || config.FrameLength > alacMaxFrame {
		return nil, fmt.Errorf("alac: unsupported frame length %d", config.FrameLength)
	}
	if config.MaxFrames < config.FrameLength {
		config.MaxFrames = config.FrameLength
	}
	if config.MaxFrames > alacMaxFrame {
		return nil, fmt.Errorf("alac: unsupported frame limit %d", config.MaxFrames)
	}
	a := &ALAC{config: config}
	for ch := range config.Channels {
		a.errs[ch] = make([]int32, config.MaxFrames)
		a.out[ch] = make([]int32, config.MaxFrames)
		a.extra[ch] = make([]int32, config.MaxFrames)
	}
	return a, nil
}

// Format describes the decoded audio.
func (a *ALAC) Format() Format {
	return Format{Rate: a.config.SampleRate, Channels: a.config.Channels, Bits: a.config.BitDepth, Codec: "ALAC"}
}

// alacBits reads big-endian bit fields. Reading past the end sets over and yields
// zeros, so a truncated packet is reported once rather than checked at every field.
type alacBits struct {
	b    []byte
	pos  int
	over bool
}

func (r *alacBits) peek(n int) uint32 {
	if n == 0 {
		return 0
	}
	var v uint64
	i := r.pos >> 3
	for j := range 5 {
		v <<= 8
		if i+j < len(r.b) {
			v |= uint64(r.b[i+j])
		}
	}
	return uint32(v >> (40 - n - r.pos&7) & (1<<n - 1))
}

func (r *alacBits) skip(n int) {
	r.pos += n
	if r.pos > len(r.b)*8 {
		r.over = true
	}
}

func (r *alacBits) read(n int) uint32 {
	v := r.peek(n)
	r.skip(n)
	return v
}

func (r *alacBits) signed(n int) int32 { return alacExtend(r.read(n), n) }

func alacExtend(v uint32, n int) int32 {
	shift := 32 - n
	return int32(v<<shift) >> shift
}

func (r *alacBits) scalar(k, bps int) uint32 {
	var x uint32
	for x < 9 && !r.over && r.read(1) == 1 {
		x++
	}
	if x > 8 {
		return r.read(bps)
	}
	if k != 1 {
		extra := r.peek(k)
		x = x<<k - x
		if extra > 1 {
			x += extra - 1
			r.skip(k)
		} else {
			r.skip(k - 1)
		}
	}
	return x
}

func alacLog2(v uint32) int {
	if v == 0 {
		return 0
	}
	return bits.Len32(v) - 1
}

// rice undoes the adaptive Golomb-Rice coding of the prediction residual.
func (a *ALAC) rice(r *alacBits, out []int32, bps int, mult uint32) error {
	history := uint32(a.config.InitialHistory)
	limit := int(a.config.Limit)
	var modifier uint32
	for i := 0; i < len(out); i++ {
		x := r.scalar(min(alacLog2(history>>9+3), limit), bps) + modifier
		modifier = 0
		out[i] = int32(x>>1) ^ -int32(x&1)
		if x > 0xffff {
			history = 0xffff
		} else {
			history += x*mult - history*mult>>9
		}
		if history < 128 && i+1 < len(out) {
			run := int(r.scalar(min(7-alacLog2(history)+int(history+16)>>6, limit), 16))
			if run > 0 {
				if run >= len(out)-i {
					return errors.New("alac: zero run overruns the frame")
				}
				clear(out[i+1 : i+1+run])
				i += run
			}
			if run <= 0xffff {
				modifier = 1
			}
			history = 0
		}
		if r.over {
			return errors.New("alac: truncated residual")
		}
	}
	return nil
}

func alacSign(v int32) int32 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// predict reverses the adaptive linear predictor. An order of 31 is the fixed
// first-order predictor; errs and out may be the same slice.
func alacPredict(errs, out []int32, bps int, coefs []int32, quant int) {
	n := len(out)
	if n == 0 {
		return
	}
	order := len(coefs)
	out[0] = errs[0]
	first := func(end int) {
		for i := 1; i < end; i++ {
			out[i] = alacExtend(uint32(out[i-1]+errs[i]), bps)
		}
	}
	if order == 0 {
		copy(out[1:], errs[1:])
		return
	}
	if order == 31 {
		first(n)
		return
	}
	first(min(order+1, n))
	var round int64
	if quant > 0 {
		round = 1 << (quant - 1)
	}
	for i := order + 1; i < n; i++ {
		base := out[i-order-1]
		pred := out[i-order : i]
		var sum int32
		for j, coef := range coefs {
			sum += (pred[j] - base) * coef
		}
		errv := errs[i]
		out[i] = alacExtend(uint32(int32((int64(sum)+round)>>quant)+base+errv), bps)
		sign := alacSign(errv)
		if sign == 0 {
			continue
		}
		for j := 0; j < order && errv*sign > 0; j++ {
			val := base - pred[j]
			s := alacSign(val) * sign
			coefs[j] -= s
			errv -= (val * s) >> quant * int32(j+1)
		}
	}
}

// Decode writes one packet to dst as interleaved, left-justified samples and returns
// the frames written. dst must hold MaxFrames frames (FrameLength unless set).
func (a *ALAC) Decode(packet []byte, dst []int32) (int, error) {
	channels := a.config.Channels
	if len(dst) < int(a.config.MaxFrames)*channels {
		return 0, errors.New("alac: output buffer too small")
	}
	r := alacBits{b: packet}
	frames := -1
	for !r.over {
		if frames >= 0 && len(packet)*8-r.pos < 3 {
			// PipeWire and PulseAudio RAOP senders drop the end tag that would
			// spill into one more byte. The audio element itself is complete.
			a.interleave(dst, frames)
			return frames, nil
		}
		switch tag := r.read(3); tag {
		case 0, 1:
			if frames >= 0 || int(tag)+1 != channels {
				return 0, errors.New("alac: unsupported channel element")
			}
			n, err := a.element(&r)
			if err != nil {
				return 0, err
			}
			frames = n
		case 4: // Data stream element.
			r.skip(4)
			aligned := r.read(1) == 1
			count := int(r.read(8))
			if count == 255 {
				count += int(r.read(8))
			}
			if aligned {
				r.skip(-r.pos & 7)
			}
			r.skip(count * 8)
		case 6: // Fill element.
			count := int(r.read(4))
			if count == 15 {
				count += int(r.read(8)) - 1
			}
			r.skip(count * 8)
		case 7: // End of frame.
			if frames < 0 {
				return 0, errors.New("alac: packet has no audio")
			}
			a.interleave(dst, frames)
			return frames, nil
		default:
			return 0, fmt.Errorf("alac: unsupported element %d", tag)
		}
	}
	return 0, errors.New("alac: truncated packet")
}

func (a *ALAC) element(r *alacBits) (int, error) {
	channels := a.config.Channels
	depth := a.config.BitDepth
	r.skip(16) // Element instance tag and unused bits.
	hasSize := r.read(1) == 1
	extra := int(r.read(2)) << 3
	compressed := r.read(1) == 0
	frames := int(a.config.FrameLength)
	if hasSize {
		size := r.read(32)
		if size == 0 || size > a.config.MaxFrames {
			return 0, fmt.Errorf("alac: frame of %d samples exceeds %d", size, a.config.MaxFrames)
		}
		frames = int(size)
	}
	if !compressed {
		for i := range frames {
			for ch := range channels {
				a.out[ch][i] = r.signed(depth)
			}
		}
		if r.over {
			return 0, errors.New("alac: truncated samples")
		}
		return frames, nil
	}
	bps := depth - extra + channels - 1
	if extra >= depth || bps > 32 {
		return 0, fmt.Errorf("alac: %d extra bits at %d-bit depth", extra, depth)
	}
	shift := int(r.read(8))
	weight := int32(r.read(8))
	var types, quants, mults, orders [2]int
	for ch := range channels {
		types[ch] = int(r.read(4))
		quants[ch] = int(r.read(4))
		mults[ch] = int(r.read(3))
		orders[ch] = int(r.read(5))
		for i := orders[ch] - 1; i >= 0; i-- {
			a.coefs[ch][i] = r.signed(16)
		}
	}
	if extra > 0 {
		for i := range frames {
			for ch := range channels {
				a.extra[ch][i] = int32(r.read(extra))
			}
		}
	}
	for ch := range channels {
		errs := a.errs[ch][:frames]
		if err := a.rice(r, errs, bps, uint32(mults[ch])*uint32(a.config.HistoryMult)/4); err != nil {
			return 0, err
		}
		switch types[ch] {
		case 0:
		case 15: // The residual was itself first-order predicted.
			alacPredict(errs, errs, bps, make([]int32, 31), 0)
		default:
			return 0, fmt.Errorf("alac: unsupported prediction type %d", types[ch])
		}
		alacPredict(errs, a.out[ch][:frames], bps, a.coefs[ch][:orders[ch]], quants[ch])
	}
	if channels == 2 && weight != 0 {
		left, right := a.out[0][:frames], a.out[1][:frames]
		for i := range left {
			l := left[i] - right[i]*weight>>shift
			left[i], right[i] = right[i]+l, l
		}
	}
	if extra > 0 {
		for ch := range channels {
			for i := range frames {
				a.out[ch][i] = a.out[ch][i]<<extra | a.extra[ch][i]
			}
		}
	}
	return frames, nil
}

func (a *ALAC) interleave(dst []int32, frames int) {
	shift := 32 - a.config.BitDepth
	channels := a.config.Channels
	for ch := range channels {
		for i, v := range a.out[ch][:frames] {
			dst[i*channels+ch] = v << shift
		}
	}
}

const alacEnabled = true
