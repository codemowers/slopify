//go:build !no_mp3

package audiodec

import (
	"math/big"
	"math/rand"
	"reflect"
	"testing"
)

// Compare against the original direct transforms, including full-width inputs
// to check wrapping and rounding, and repeated calls to check filter history.
func TestMP3TransformSymmetry(t *testing.T) {
	rng := rand.New(rand.NewSource(73))
	for _, full := range []bool{false, true} {
		sample := func() int32 {
			if full {
				return int32(rng.Uint32())
			}
			return int32(rng.Int31n(1<<28)) - 1<<27
		}
		var got, want mp3Decoder
		for iteration := 0; iteration < 32; iteration++ {
			for ch := 0; ch < 2; ch++ {
				for block := 0; block < 4; block++ {
					for _, mixed := range []bool{false, true} {
						gr := mp3Granule{blockType: block, short: block == 2, windowSwitch: true, mixed: mixed}
						for i := range gr.lines {
							gr.lines[i] = sample()
						}
						original := gr
						got.hybrid(&gr, ch)
						want.referenceHybrid(&original, ch)
						if gr.lines != original.lines || got.overlap != want.overlap {
							t.Fatalf("hybrid full=%v iteration=%d channel=%d block=%d mixed=%v", full, iteration, ch, block, mixed)
						}
					}
				}
			}
			for channels := 1; channels <= 2; channels++ {
				got.out, want.out = got.out[:0], want.out[:0]
				for ch := 0; ch < channels; ch++ {
					for i := range got.gr[0][ch].lines {
						got.gr[0][ch].lines[i] = sample()
					}
					want.gr[0][ch] = got.gr[0][ch]
				}
				got.synthesis(0, channels)
				want.referenceSynthesis(0, channels)
				if !reflect.DeepEqual(got.out, want.out) || got.v != want.v || got.voff != want.voff {
					t.Fatalf("synthesis full=%v iteration=%d channels=%d", full, iteration, channels)
				}
			}
		}
	}
}

// These direct matrix implementations are the reference from before the
// symmetry optimization, deliberately retaining every row and column.
func (d *mp3Decoder) referenceHybrid(gr *mp3Granule, ch int) {
	var out [36]int32
	for sb := 0; sb < 32; sb++ {
		bt := gr.blockType
		if gr.windowSwitch && gr.mixed && sb < 2 {
			bt = 0
		}
		in := gr.lines[sb*18 : sb*18+18]
		clear(out[:])
		if bt == 2 {
			for w := 0; w < 3; w++ {
				for p := 0; p < 12; p++ {
					var sum int64
					for m := 0; m < 6; m++ {
						sum += int64(in[w+3*m]) * int64(mp3Cos12[m][p])
					}
					out[6*w+p+6] += mp3Mul(int32((sum+1<<29)>>30), mp3Win[2][p])
				}
			}
		} else {
			nonzero := false
			for _, v := range in {
				if v != 0 {
					nonzero = true
					break
				}
			}
			if nonzero {
				win := &mp3Win[bt]
				for p := 0; p < 36; p++ {
					var sum int64
					for m := 0; m < 18; m++ {
						sum += int64(in[m]) * int64(mp3Cos36[m][p])
					}
					out[p] = mp3Mul(int32((sum+1<<29)>>30), win[p])
				}
			}
		}
		store := &d.overlap[ch][sb]
		for i := 0; i < 18; i++ {
			v := out[i] + store[i]
			store[i] = out[i+18]
			if sb&1 == 1 && i&1 == 1 { // Frequency inversion.
				v = -v
			}
			in[i] = v
		}
	}
}

// synthesis runs the polyphase filterbank, appending interleaved words.
func (d *mp3Decoder) referenceSynthesis(g, channels int) {
	base := len(d.out)
	d.out = append(d.out, make([]int32, mp3Lines*channels)...)
	for ch := 0; ch < channels; ch++ {
		lines := &d.gr[g][ch].lines
		v := &d.v[ch]
		off := d.voff[ch]
		for ss := 0; ss < 18; ss++ {
			off = (off - 64) & 1023
			for i := 0; i < 64; i++ {
				var sum int64
				row := &mp3Synth[i]
				for j := 0; j < 32; j++ {
					sum += int64(lines[j*18+ss]) * int64(row[j])
				}
				v[(off+i)&1023] = int32((sum + 1<<29) >> 30)
			}
			for i := 0; i < 32; i++ {
				var sum int64
				for j := 0; j < 8; j++ {
					sum += int64(v[(off+j*128+i)&1023]) * int64(mp3D[j*64+i])
					sum += int64(v[(off+j*128+96+i)&1023]) * int64(mp3D[j*64+32+i])
				}
				// Q28 * Q16 = Q44; output words are Q31.
				word := (sum + 1<<12) >> 13
				if word > 1<<31-1 {
					word = 1<<31 - 1
				} else if word < -(1 << 31) {
					word = -(1 << 31)
				}
				d.out[base+(32*ss+i)*channels+ch] = int32(word)
			}
		}
		d.voff[ch] = off
	}
}

func TestMP3GeneratedPowers(t *testing.T) {
	mantissa, exponent := referenceMP3Pow()
	if mantissa != mp3PowMantissa || exponent != mp3PowExponent {
		t.Fatal("generated requantization tables differ from integer cube roots")
	}
}

func referenceMP3Pow() ([8207]uint32, [8207]int8) {
	var mp3PowMantissa [8207]uint32
	var mp3PowExponent [8207]int8
	var n, y, y2, q, three, x4 big.Int
	three.SetInt64(3)
	for x := int64(1); x < int64(len(mp3PowMantissa)); x++ {
		x4.SetInt64(x)
		x4.Mul(&x4, &x4)
		x4.Mul(&x4, &x4)
		// Scale so that the cube root has at least 32 significant bits.
		shift := uint(32 - x4.BitLen()/3)
		n.Lsh(&x4, 3*shift)
		y.Lsh(big.NewInt(1), uint(n.BitLen()/3+1))
		for { // Integer Newton iteration converges down to the floor.
			y2.Mul(&y, &y)
			q.Quo(&n, &y2)
			q.Add(&q, &y)
			q.Add(&q, &y)
			q.Quo(&q, &three)
			if q.Cmp(&y) >= 0 {
				break
			}
			y.Set(&q)
		}
		exponent := y.BitLen() - int(shift)
		if extra := y.BitLen() - 32; extra > 0 {
			y.Rsh(&y, uint(extra))
		} else {
			y.Lsh(&y, uint(-extra))
		}
		mp3PowMantissa[x] = uint32(y.Uint64())
		mp3PowExponent[x] = int8(exponent)
	}
	return mp3PowMantissa, mp3PowExponent
}
