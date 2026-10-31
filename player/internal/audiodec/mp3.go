package audiodec

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"sync"
)

// MPEG-1, 2 and 2.5 Layer III in Q28 fixed point: every audio calculation is
// integer, for CPUs where floating point is slow or absent. Intermediate values
// wrap rather than fault on hostile input; that can only produce noise.

//go:generate sh -c "go run mp3tables_gen.go > mp3tables.go"

const (
	mp3Lines     = 576
	mp3Delay     = 528 + 1 // inherent decoder delay, located by gapless metadata
	mp3Reservoir = 511     // largest main_data_begin
	mp3Preroll   = 4       // frames decoded silently after a seek
	mp3Precision = 24      // reported decoded precision; output words are 32-bit
)

var (
	mp3Bitrate = [2][16]int{
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
	}
	mp3Rates = [3][3]int{{44100, 48000, 32000}, {22050, 24000, 16000}, {11025, 12000, 8000}}
	mp3Long  = [3][3][23]int{
		{
			{0, 4, 8, 12, 16, 20, 24, 30, 36, 44, 52, 62, 74, 90, 110, 134, 162, 196, 238, 288, 342, 418, 576},
			{0, 4, 8, 12, 16, 20, 24, 30, 36, 42, 50, 60, 72, 88, 106, 128, 156, 190, 230, 276, 330, 384, 576},
			{0, 4, 8, 12, 16, 20, 24, 30, 36, 44, 54, 66, 82, 102, 126, 156, 194, 240, 296, 364, 448, 550, 576},
		},
		{
			{0, 6, 12, 18, 24, 30, 36, 44, 54, 66, 80, 96, 116, 140, 168, 200, 238, 284, 336, 396, 464, 522, 576},
			{0, 6, 12, 18, 24, 30, 36, 44, 54, 66, 80, 96, 114, 136, 162, 194, 232, 278, 332, 394, 464, 540, 576},
			{0, 6, 12, 18, 24, 30, 36, 44, 54, 66, 80, 96, 116, 140, 168, 200, 238, 284, 336, 396, 464, 522, 576},
		},
		{
			{0, 6, 12, 18, 24, 30, 36, 44, 54, 66, 80, 96, 116, 140, 168, 200, 238, 284, 336, 396, 464, 522, 576},
			{0, 6, 12, 18, 24, 30, 36, 44, 54, 66, 80, 96, 116, 140, 168, 200, 238, 284, 336, 396, 464, 522, 576},
			{0, 12, 24, 36, 48, 60, 72, 88, 108, 132, 160, 192, 232, 280, 336, 400, 476, 566, 568, 570, 572, 574, 576},
		},
	}
	mp3Short = [3][3][14]int{
		{
			{0, 4, 8, 12, 16, 22, 30, 40, 52, 66, 84, 106, 136, 192},
			{0, 4, 8, 12, 16, 22, 28, 38, 50, 64, 80, 100, 126, 192},
			{0, 4, 8, 12, 16, 22, 30, 42, 58, 78, 104, 138, 180, 192},
		},
		{
			{0, 4, 8, 12, 18, 24, 32, 42, 56, 74, 100, 132, 174, 192},
			{0, 4, 8, 12, 18, 26, 36, 48, 62, 80, 104, 136, 180, 192},
			{0, 4, 8, 12, 18, 26, 36, 48, 62, 80, 104, 134, 174, 192},
		},
		{
			{0, 4, 8, 12, 18, 26, 36, 48, 62, 80, 104, 134, 174, 192},
			{0, 4, 8, 12, 18, 26, 36, 48, 62, 80, 104, 134, 174, 192},
			{0, 8, 16, 24, 36, 52, 72, 96, 124, 160, 162, 164, 166, 192},
		},
	}
	mp3Pretab  = [22]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 3, 3, 3, 2, 0}
	mp3Slen1   = [16]int{0, 0, 0, 0, 3, 1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4}
	mp3Slen2   = [16]int{0, 1, 2, 3, 0, 1, 2, 3, 1, 2, 3, 1, 2, 3, 2, 3}
	mp3LSFSfbs = [6][3][4]int{
		{{6, 5, 5, 5}, {9, 9, 9, 9}, {6, 9, 9, 9}},
		{{6, 5, 7, 3}, {9, 9, 12, 6}, {6, 9, 12, 6}},
		{{11, 10, 0, 0}, {18, 18, 0, 0}, {15, 18, 0, 0}},
		{{7, 7, 7, 0}, {12, 12, 12, 0}, {6, 15, 12, 0}},
		{{6, 6, 6, 3}, {12, 9, 9, 6}, {6, 12, 9, 6}},
		{{8, 8, 5, 0}, {15, 12, 9, 0}, {6, 18, 9, 0}},
	}
)

// x^(4/3) = mantissa/2^32 * 2^exponent, computed once with integer arithmetic.
var (
	mp3PowOnce     sync.Once
	mp3PowMantissa [8207]uint32
	mp3PowExponent [8207]int8
)

func mp3InitPow() {
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
}

// mp3Requantize returns is^(4/3) * 2^(exp/4) in Q28.
func mp3Requantize(is, exp int) int32 {
	negative := is < 0
	if negative {
		is = -is
	}
	if is == 0 || is >= len(mp3PowMantissa) {
		return 0
	}
	q := exp >> 2
	v := uint64(mp3PowMantissa[is]) * uint64(mp3Root[exp&3]) >> 32 // Q31
	shift := int(mp3PowExponent[is]) + q - 3
	var out uint64
	switch {
	case shift >= 32:
		out = 1<<31 - 1
	case shift >= 0:
		out = min(v<<uint(shift), 1<<31-1)
	case shift > -40:
		out = (v + 1<<uint(-shift-1)) >> uint(-shift)
	}
	if negative {
		return -int32(out)
	}
	return int32(out)
}

type mp3Header struct {
	version, rateIndex, sfTable  int // version: 0 MPEG-1, 1 MPEG-2, 2 MPEG-2.5
	padding, protected           bool
	mode, modeExt                int
	rate, size, samples, channel int
	sideSize, bitrate            int
}

func (h mp3Header) lsf() bool { return h.version != 0 }

func parseMP3Header(b []byte) (mp3Header, bool) {
	var h mp3Header
	if b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return h, false
	}
	switch b[1] >> 3 & 3 {
	case 3:
		h.version = 0
	case 2:
		h.version = 1
	case 0:
		h.version = 2
	default:
		return h, false
	}
	if b[1]>>1&3 != 1 { // Layer III only.
		return h, false
	}
	bitrateIndex, rateIndex := int(b[2]>>4), int(b[2]>>2&3)
	if bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return h, false
	}
	h.protected = b[1]&1 == 0
	h.padding = b[2]>>1&1 == 1
	h.rateIndex = rateIndex
	h.rate = mp3Rates[h.version][rateIndex]
	h.mode, h.modeExt = int(b[3]>>6), int(b[3]>>4&3)
	h.channel = 2
	if h.mode == 3 {
		h.channel = 1
	}
	table := 0
	if h.lsf() {
		table = 1
	}
	bitrate := mp3Bitrate[table][bitrateIndex] * 1000
	h.bitrate = bitrate
	if h.lsf() {
		h.samples = 576
		h.size = 72 * bitrate / h.rate
		h.sideSize = 17
		if h.channel == 1 {
			h.sideSize = 9
		}
	} else {
		h.samples = 1152
		h.size = 144 * bitrate / h.rate
		h.sideSize = 32
		if h.channel == 1 {
			h.sideSize = 17
		}
	}
	if h.padding {
		h.size++
	}
	if h.size < 4+h.sideSize+btoi(h.protected)*2 {
		return h, false
	}
	return h, true
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mp3Bits reads big-endian bits; past the end it yields zeros.
type mp3Bits struct {
	b   []byte
	pos int
}

func (r *mp3Bits) bit() int {
	i := r.pos >> 3
	v := 0
	if i < len(r.b) {
		v = int(r.b[i]>>(7-uint(r.pos&7))) & 1
	}
	r.pos++
	return v
}

func (r *mp3Bits) read(n int) int {
	v := 0
	for ; n > 0; n-- {
		v = v<<1 | r.bit()
	}
	return v
}

type mp3Granule struct {
	part23, bigValues, globalGain, compress    int
	windowSwitch, mixed, preflag, scale, table bool
	blockType                                  int
	tables                                     [3]int
	subblockGain                               [3]int
	region0, region1                           int
	scfsiLong                                  [4]bool
	scalefac                                   [39]int // long, or sfb*3+win for short
	short                                      bool
	nonzero                                    int
	lines                                      [mp3Lines]int32
}

type mp3Decoder struct {
	r       io.ReadSeeker
	br      *bufio.Reader
	start   int64 // first audio byte
	format  Format
	first   mp3Header
	tag     mp3Tag
	bitrate int
	res     []byte
	gr      [2][2]mp3Granule
	overlap [2][32][18]int32
	v       [2][1024]int32
	voff    [2]int

	skip, tail  int // gapless frames to drop at the start, and hold back at the end
	out         []int32
	eof         bool
	discard     int // frames to decode silently
	initialSkip int
	frameIndex  int
}

func openMP3(r io.ReadSeeker, audio int64) (Decoder, error) {
	mp3PowOnce.Do(mp3InitPow)
	d := &mp3Decoder{r: r, start: audio}
	if _, err := r.Seek(audio, io.SeekStart); err != nil {
		return nil, err
	}
	d.br = bufio.NewReaderSize(r, 16<<10)
	head, err := d.br.Peek(4)
	if err != nil {
		return nil, fmt.Errorf("%w: short mp3", ErrUnsupported)
	}
	h, ok := parseMP3Header(head)
	if !ok {
		return nil, fmt.Errorf("%w: not Layer III", ErrUnsupported)
	}
	d.first = h
	d.bitrate = h.bitrate
	d.format = Format{Rate: h.rate, Channels: h.channel, Bits: mp3Precision, Codec: "MP3", Lossy: true}
	frame, err := d.br.Peek(h.size)
	if err == nil {
		if tag, isTag := mp3ParseTag(h, frame); isTag {
			d.tag = tag
			d.skip, d.tail = tag.skip, tag.tail
			d.initialSkip = tag.skip
			d.br.Discard(h.size) // The tag frame carries no audio.
			d.start += int64(h.size)
		}
	}
	return d, nil
}

// mp3Tag is what a Xing/Info frame says about the stream.
type mp3Tag struct {
	skip, tail int
	frames     int64
	bytes      int64
	toc        []byte
	cbr        bool // an Info frame marks constant bitrate, a Xing frame variable
}

// mp3ParseTag recognises a Xing/Info frame: its seek table and encoder padding.
func mp3ParseTag(h mp3Header, frame []byte) (tag mp3Tag, isTag bool) {
	p := 4 + h.sideSize + btoi(h.protected)*2
	if len(frame) < p+8 || string(frame[p:p+4]) != "Xing" && string(frame[p:p+4]) != "Info" {
		return tag, false
	}
	tag.cbr = string(frame[p:p+4]) == "Info"
	flags := binary.BigEndian.Uint32(frame[p+4:])
	p += 8
	if flags&1 != 0 && len(frame) >= p+4 {
		tag.frames = int64(binary.BigEndian.Uint32(frame[p:]))
		p += 4
	}
	if flags&2 != 0 && len(frame) >= p+4 {
		tag.bytes = int64(binary.BigEndian.Uint32(frame[p:]))
		p += 4
	}
	if flags&4 != 0 && len(frame) >= p+100 {
		tag.toc = bytes.Clone(frame[p : p+100])
		p += 100
	}
	if flags&8 != 0 {
		p += 4
	}
	// Encoder (9), revision (1), lowpass (1), peak (4), gains (4), flags (1),
	// bitrate (1), then 12-bit delay and 12-bit padding.
	if len(frame) < p+24 {
		return tag, true
	}
	if v := string(frame[p : p+4]); v != "LAME" && v != "Lavc" && v != "Lavf" {
		return tag, true
	}
	t := frame[p+21:]
	delay := int(t[0])<<4 | int(t[1])>>4
	padding := int(t[1]&0x0f)<<8 | int(t[2])
	tag.skip, tag.tail = delay+mp3Delay, max(padding-mp3Delay, 0)
	return tag, true
}

func (d *mp3Decoder) Format() Format { return d.format }

// nextFrame finds a frame consistent with the stream, resynchronising if needed.
func (d *mp3Decoder) nextFrame(buf []byte) (mp3Header, []byte, error) {
	for {
		head, err := d.br.Peek(4)
		if err != nil {
			return mp3Header{}, nil, io.EOF
		}
		h, ok := parseMP3Header(head)
		if !ok || h.version != d.first.version || h.rateIndex != d.first.rateIndex || h.channel != d.first.channel {
			d.br.Discard(1)
			continue
		}
		frame := buf[:h.size]
		if _, err := io.ReadFull(d.br, frame); err != nil {
			return mp3Header{}, nil, io.EOF
		}
		return h, frame, nil
	}
}

func (d *mp3Decoder) Read(dst []int32) (int, error) {
	channels := d.format.Channels
	tail := d.tail * channels
	var frame [2048]byte
	for !d.eof && len(d.out)-tail < channels {
		h, data, err := d.nextFrame(frame[:])
		if err != nil {
			d.eof = true
			break
		}
		d.decodeFrame(h, data)
	}
	available := len(d.out) - tail
	if available < channels {
		return 0, io.EOF
	}
	n := min(available, len(dst)/channels*channels)
	copy(dst, d.out[:n])
	d.out = append(d.out[:0], d.out[n:]...)
	return n, nil
}

func (d *mp3Decoder) decodeFrame(h mp3Header, frame []byte) {
	side := frame[4:]
	if h.protected {
		side = side[2:]
	}
	r := mp3Bits{b: side[:h.sideSize]}
	granules := 2
	var mainBegin int
	if h.lsf() {
		granules = 1
		mainBegin = r.read(8)
		r.read(h.channel) // private bits
	} else {
		mainBegin = r.read(9)
		if h.channel == 1 {
			r.read(5)
		} else {
			r.read(3)
		}
	}
	if !h.lsf() {
		for ch := 0; ch < h.channel; ch++ {
			for band := 0; band < 4; band++ {
				d.gr[1][ch].scfsiLong[band] = r.bit() == 1
			}
		}
	}
	for g := 0; g < granules; g++ {
		for ch := 0; ch < h.channel; ch++ {
			mp3ReadSide(&r, &d.gr[g][ch], h)
		}
	}

	main := side[h.sideSize:]
	valid := len(d.res) >= mainBegin
	var bs mp3Bits
	if valid {
		// The reservoir precedes this frame's main data in one contiguous buffer.
		start := len(d.res) - mainBegin
		d.res = append(d.res, main...)
		bs = mp3Bits{b: d.res[start:]}
	} else {
		d.res = append(d.res, main...)
	}

	base := len(d.out)
	for g := 0; g < granules; g++ {
		for ch := 0; ch < h.channel; ch++ {
			gr := &d.gr[g][ch]
			clear(gr.lines[:])
			gr.nonzero = 0
			if valid {
				begin := bs.pos
				d.readScalefactors(&bs, g, ch, h)
				d.readHuffman(&bs, gr, begin+gr.part23, h)
				bs.pos = begin + gr.part23
				d.requantize(gr, h)
			}
		}
		if valid && h.channel == 2 {
			d.stereo(g, h)
		}
		for ch := 0; ch < h.channel; ch++ {
			gr := &d.gr[g][ch]
			mp3Antialias(gr)
			d.hybrid(gr, ch)
		}
		d.synthesis(g, h.channel)
	}
	if over := len(d.res) - mp3Reservoir; over > 0 {
		d.res = append(d.res[:0], d.res[over:]...)
	}
	d.frameIndex++
	if d.discard > 0 {
		d.discard--
		d.out = d.out[:base]
		return
	}
	if d.skip > 0 {
		drop := min(d.skip*h.channel, len(d.out)-base)
		d.skip -= drop / h.channel
		d.out = append(d.out[:base], d.out[base+drop:]...)
	}
}

func mp3ReadSide(r *mp3Bits, gr *mp3Granule, h mp3Header) {
	gr.part23 = r.read(12)
	gr.bigValues = min(r.read(9), mp3Lines/2)
	gr.globalGain = r.read(8)
	if h.lsf() {
		gr.compress = r.read(9)
	} else {
		gr.compress = r.read(4)
	}
	gr.windowSwitch = r.bit() == 1
	gr.preflag, gr.mixed, gr.short = false, false, false
	gr.blockType = 0
	if gr.windowSwitch {
		gr.blockType = r.read(2)
		gr.mixed = r.bit() == 1
		gr.tables[0], gr.tables[1] = r.read(5), r.read(5)
		gr.tables[2] = 0
		for w := 0; w < 3; w++ {
			gr.subblockGain[w] = r.read(3)
		}
		gr.short = gr.blockType == 2
		if gr.short && !gr.mixed {
			gr.region0 = 8
		} else {
			gr.region0 = 7
		}
		gr.region1 = 20 - gr.region0
	} else {
		for i := range gr.tables {
			gr.tables[i] = r.read(5)
		}
		gr.region0, gr.region1 = r.read(4), r.read(3)
		gr.subblockGain = [3]int{}
	}
	if !h.lsf() {
		gr.preflag = r.bit() == 1
	}
	gr.scale = r.bit() == 1
	gr.table = r.bit() == 1
}

func (d *mp3Decoder) readScalefactors(r *mp3Bits, g, ch int, h mp3Header) {
	gr := &d.gr[g][ch]
	if h.lsf() {
		d.readScalefactorsLSF(r, gr, ch, h)
		return
	}
	slen1, slen2 := mp3Slen1[gr.compress], mp3Slen2[gr.compress]
	if gr.short {
		i := 0
		if gr.mixed {
			for ; i < 8; i++ {
				gr.scalefac[i] = r.read(slen1)
			}
			for sfb := 3; sfb < 6; sfb++ {
				for w := 0; w < 3; w++ {
					gr.scalefac[i] = r.read(slen1)
					i++
				}
			}
		} else {
			for ; i < 18; i++ {
				gr.scalefac[i] = r.read(slen1)
			}
		}
		for ; i < len(gr.scalefac); i++ {
			gr.scalefac[i] = 0
		}
		end := 36
		if gr.mixed {
			end = 35
		}
		for i = end - 18; i < end; i++ {
			gr.scalefac[i] = r.read(slen2)
		}
		return
	}
	bands := [5]int{0, 6, 11, 16, 21}
	for b := 0; b < 4; b++ {
		bits := slen1
		if b >= 2 {
			bits = slen2
		}
		for i := bands[b]; i < bands[b+1]; i++ {
			if g == 1 && gr.scfsiLong[b] {
				gr.scalefac[i] = d.gr[0][ch].scalefac[i]
			} else {
				gr.scalefac[i] = r.read(bits)
			}
		}
	}
	gr.scalefac[21] = 0
}

func (d *mp3Decoder) readScalefactorsLSF(r *mp3Bits, gr *mp3Granule, ch int, h mp3Header) {
	var slen [4]int
	var table int
	sc := gr.compress
	if h.modeExt&1 == 0 || ch == 0 {
		switch {
		case sc < 400:
			slen = [4]int{(sc >> 4) / 5, (sc >> 4) % 5, (sc & 15) >> 2, sc & 3}
		case sc < 500:
			sc -= 400
			slen = [4]int{(sc >> 2) / 5, (sc >> 2) % 5, sc & 3, 0}
			table = 1
		default:
			sc -= 500
			slen = [4]int{sc / 3, sc % 3, 0, 0}
			table = 2
			gr.preflag = true
		}
	} else {
		ic := sc >> 1
		switch {
		case ic < 180:
			slen = [4]int{ic / 36, ic % 36 / 6, ic % 36 % 6, 0}
			table = 3
		case ic < 244:
			ic -= 180
			slen = [4]int{(ic & 63) >> 4, (ic & 15) >> 2, ic & 3, 0}
			table = 4
		default:
			ic -= 244
			slen = [4]int{ic / 3, ic % 3, 0, 0}
			table = 5
		}
	}
	shape := 0
	if gr.short {
		shape = 1
		if gr.mixed {
			shape = 2
		}
	}
	i := 0
	for p, count := range mp3LSFSfbs[table][shape] {
		for j := 0; j < count && i < len(gr.scalefac); j++ {
			gr.scalefac[i] = r.read(slen[p])
			i++
		}
	}
	for ; i < len(gr.scalefac); i++ {
		gr.scalefac[i] = 0
	}
}

func mp3HuffmanPair(r *mp3Bits, table, end int) (x, y int, ok bool) {
	t := &huffmanMain[table]
	if t.treelen == 0 {
		return 0, 0, true
	}
	point := 0
	for bits := 32; ; bits-- {
		if bits <= 0 || point >= t.treelen || r.pos > end+32 {
			return 0, 0, false
		}
		node := t.hufftable[point]
		if node&0xff00 == 0 {
			x, y = int(node>>4&0xf), int(node&0xf)
			break
		}
		if r.bit() != 0 {
			for point < t.treelen && t.hufftable[point]&0xff >= 250 {
				point += int(t.hufftable[point] & 0xff)
			}
			if point >= t.treelen {
				return 0, 0, false
			}
			point += int(t.hufftable[point] & 0xff)
		} else {
			for point < t.treelen && t.hufftable[point]>>8 >= 250 {
				point += int(t.hufftable[point] >> 8)
			}
			if point >= t.treelen {
				return 0, 0, false
			}
			point += int(t.hufftable[point] >> 8)
		}
	}
	return x, y, true
}

// readHuffman leaves raw signed values in gr.lines, nonzero count in gr.nonzero.
func (d *mp3Decoder) readHuffman(r *mp3Bits, gr *mp3Granule, end int, h mp3Header) {
	long := &mp3Long[h.version][h.rateIndex]
	region1, region2 := 36, mp3Lines
	if h.version == 2 && h.rateIndex == 2 { // 8 kHz doubles the first short region.
		region1 = 72
	}
	if !gr.short {
		region1 = long[min(gr.region0+1, 22)]
		region2 = long[min(gr.region0+gr.region1+2, 22)]
	}
	i := 0
	for i < gr.bigValues*2 && r.pos < end {
		table := gr.tables[0]
		if i >= region2 {
			table = gr.tables[2]
		} else if i >= region1 {
			table = gr.tables[1]
		}
		if table >= 32 {
			return
		}
		x, y, ok := mp3HuffmanPair(r, table, end)
		if !ok {
			gr.nonzero = i
			return
		}
		linbits := huffmanMain[table].linbits
		if linbits != 0 && x == 15 {
			x += r.read(linbits)
		}
		if x != 0 && r.bit() == 1 {
			x = -x
		}
		if linbits != 0 && y == 15 {
			y += r.read(linbits)
		}
		if y != 0 && r.bit() == 1 {
			y = -y
		}
		gr.lines[i], gr.lines[i+1] = int32(x), int32(y)
		i += 2
	}
	table := 32 + btoi(gr.table)
	for i <= mp3Lines-4 && r.pos < end {
		_, v, ok := mp3HuffmanPair(r, table, end)
		if !ok {
			break
		}
		quad := [4]int{v >> 3 & 1, v >> 2 & 1, v >> 1 & 1, v & 1}
		for j := range quad {
			if quad[j] != 0 && r.bit() == 1 {
				quad[j] = -quad[j]
			}
		}
		if r.pos > end { // The final word overran this part; it is not audio.
			break
		}
		for j := range quad {
			gr.lines[i+j] = int32(quad[j])
		}
		i += 4
	}
	gr.nonzero = i
}

// requantize scales raw values and reorders short bands into frequency order.
func (d *mp3Decoder) requantize(gr *mp3Granule, h mp3Header) {
	long := &mp3Long[h.version][h.rateIndex]
	short := &mp3Short[h.version][h.rateIndex]
	gain := gr.globalGain - 210
	scale := 2
	if gr.scale {
		scale = 4
	}
	i := 0
	if !gr.short || gr.mixed {
		sfbs, limit := 22, mp3Lines
		if gr.short {
			sfbs = mp3MixedLongBands(h)
			limit = long[sfbs]
		}
		for sfb := 0; sfb < sfbs && i < limit; sfb++ {
			sf := gr.scalefac[sfb]
			if gr.preflag {
				sf += mp3Pretab[sfb]
			}
			exp := gain - scale*sf
			for ; i < long[sfb+1] && i < limit; i++ {
				gr.lines[i] = mp3Requantize(int(gr.lines[i]), exp)
			}
		}
	}
	if !gr.short {
		return
	}
	var tmp [mp3Lines]int32
	sfb, offset := 0, 0
	if gr.mixed {
		sfb, offset = 3, mp3MixedLongBands(h)
	}
	for ; sfb < 13 && i < mp3Lines; sfb++ {
		width := short[sfb+1] - short[sfb]
		base := short[sfb] * 3
		for w := 0; w < 3; w++ {
			exp := gain - 8*gr.subblockGain[w]
			if sfb < 12 {
				first := 0
				if gr.mixed {
					first = 3
				}
				exp -= scale * gr.scalefac[offset+(sfb-first)*3+w]
			}
			for j := 0; j < width && i < mp3Lines; j++ {
				tmp[base+w+3*j] = mp3Requantize(int(gr.lines[i]), exp)
				i++
			}
		}
	}
	start := 0
	if gr.mixed {
		start = long[mp3MixedLongBands(h)]
	}
	copy(gr.lines[start:], tmp[start:])
	gr.nonzero = mp3Lines
}

// mp3MixedLongBands is the number of long scalefactor bands in a mixed block.
func mp3MixedLongBands(h mp3Header) int {
	if h.lsf() {
		return 6
	}
	return 8
}

func mp3Mul(a, c int32) int32 { return int32((int64(a)*int64(c) + 1<<29) >> 30) }

func (d *mp3Decoder) stereo(g int, h mp3Header) {
	left, right := &d.gr[g][0], &d.gr[g][1]
	ms := h.mode == 1 && h.modeExt&2 != 0
	if h.mode == 1 && h.modeExt&1 != 0 {
		d.intensity(left, right, ms, h)
		return
	}
	if ms {
		mp3MS(left, right, 0, mp3Lines)
	}
}

func mp3MS(left, right *mp3Granule, from, to int) {
	for i := from; i < to; i++ {
		l, r := left.lines[i], right.lines[i]
		left.lines[i] = mp3Mul(l+r, mp3InvSqrt2)
		right.lines[i] = mp3Mul(l-r, mp3InvSqrt2)
	}
}

// intensity handles bands above the right channel's last nonzero line; lower
// bands stay mid/side when that is enabled. Positions come from the right channel.
func (d *mp3Decoder) intensity(left, right *mp3Granule, ms bool, h mp3Header) {
	type band struct{ from, to, step, pos int }
	var bands []band
	long := &mp3Long[h.version][h.rateIndex]
	short := &mp3Short[h.version][h.rateIndex]
	position := func(i int) int { return right.scalefac[min(i, len(right.scalefac)-1)] }
	if right.short {
		i := 0
		if right.mixed {
			for sfb := 0; sfb < mp3MixedLongBands(h); sfb++ {
				bands = append(bands, band{long[sfb], long[sfb+1], 1, position(sfb)})
			}
			i = 3
		}
		for sfb := i; sfb < 13; sfb++ {
			width := short[sfb+1] - short[sfb]
			for w := 0; w < 3; w++ {
				last := min(sfb, 11)
				index := (last-i)*3 + w
				if right.mixed {
					index += mp3MixedLongBands(h)
				}
				start := short[sfb]*3 + w
				bands = append(bands, band{start, start + 3*width, 3, position(index)})
			}
		}
	} else {
		for sfb := 0; sfb < 22; sfb++ {
			bands = append(bands, band{long[sfb], long[sfb+1], 1, position(min(sfb, 20))})
		}
	}
	illegal := 7
	if h.lsf() {
		illegal = 1 << 30 // MPEG-2 positions have no fixed illegal value here.
	}
	active := true
	for b := len(bands) - 1; b >= 0; b-- {
		x := bands[b]
		for i := x.from; i < x.to && i < mp3Lines; i += x.step {
			if right.lines[i] != 0 {
				active = false
			}
		}
		if !active || x.pos >= illegal {
			if ms {
				for i := x.from; i < x.to && i < mp3Lines; i += x.step {
					mp3MS(left, right, i, i+1)
				}
			}
			continue
		}
		var lr, rr int32
		if h.lsf() {
			lr, rr = mp3LSFRatio(x.pos, left.compress&1 == 1)
		} else {
			lr, rr = mp3Intensity[min(x.pos, 6)][0], mp3Intensity[min(x.pos, 6)][1]
		}
		for i := x.from; i < x.to && i < mp3Lines; i += x.step {
			v := left.lines[i]
			left.lines[i], right.lines[i] = mp3Mul(v, lr), mp3Mul(v, rr)
		}
	}
}

func mp3LSFRatio(pos int, root bool) (l, r int32) {
	base := int32(mp3InvRoot4)
	if root {
		base = mp3InvSqrt2
	}
	v := int32(1 << 30)
	for i := 0; i < (pos+1)/2 && v != 0; i++ {
		v = mp3Mul(v, base)
	}
	if pos%2 == 1 {
		return v, 1 << 30
	}
	return 1 << 30, v
}

func mp3Antialias(gr *mp3Granule) {
	limit := 32
	if gr.short {
		if !gr.mixed {
			return
		}
		limit = 2
	}
	for sb := 1; sb < limit; sb++ {
		for i := 0; i < 8; i++ {
			li, ui := 18*sb-1-i, 18*sb+i
			lo, up := int64(gr.lines[li]), int64(gr.lines[ui])
			gr.lines[li] = int32((lo*int64(mp3CS[i]) - up*int64(mp3CA[i]) + 1<<29) >> 30)
			gr.lines[ui] = int32((up*int64(mp3CS[i]) + lo*int64(mp3CA[i]) + 1<<29) >> 30)
		}
	}
}

func (d *mp3Decoder) hybrid(gr *mp3Granule, ch int) {
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
func (d *mp3Decoder) synthesis(g, channels int) {
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

// Seek jumps to an estimated byte offset, so its cost does not grow with the
// position. MP3 frames carry no timestamps: what it returns is the estimate.
func (d *mp3Decoder) Seek(seconds float64) (float64, error) {
	seconds = max(seconds, 0)
	rate, samples := int64(d.format.Rate), int64(d.first.samples)
	frameSeconds := float64(samples) / float64(rate)
	d.res = d.res[:0]
	d.overlap = [2][32][18]int32{}
	d.v = [2][1024]int32{}
	d.voff = [2]int{}
	d.out = d.out[:0]
	d.eof = false
	// The first frames come from the start exactly, gapless trim included.
	if seconds <= frameSeconds*mp3Preroll {
		if _, err := d.r.Seek(d.start, io.SeekStart); err != nil {
			return 0, err
		}
		d.br.Reset(d.r)
		target := int64(seconds*float64(rate)) + int64(d.initialSkip)
		d.discard = int(target / samples)
		d.skip = int(target % samples)
		return seconds, nil
	}
	end, err := d.r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	size := end - d.start
	if d.tag.bytes > 0 {
		size = min(size, d.tag.bytes)
	}
	// Decoding begins a few frames early so that the audio has settled at the target.
	from := seconds - frameSeconds*mp3Preroll
	offset := d.bytesAt(from, size)
	start, err := findMP3Start(d.r, d.start+offset)
	if err != nil {
		d.eof = true // Past the last frame: the track has simply ended.
		return seconds, nil
	}
	if _, err := d.r.Seek(start, io.SeekStart); err != nil {
		return 0, err
	}
	d.br.Reset(d.r)
	d.discard, d.skip = mp3Preroll, 0
	return d.secondsAt(start-d.start, size) + frameSeconds*mp3Preroll, nil
}

func (d *mp3Decoder) duration() float64 {
	return float64(d.tag.frames) * float64(d.first.samples) / float64(d.format.Rate)
}

func (d *mp3Decoder) usesTOC() bool { return !d.tag.cbr && len(d.tag.toc) == 100 && d.tag.frames > 0 }

// trimSeconds is the encoder delay that output time excludes and bytes include.
func (d *mp3Decoder) trimSeconds() float64 { return float64(d.initialSkip) / float64(d.format.Rate) }

// bytesAt maps a time to a byte offset through the seek table, or by bitrate.
func (d *mp3Decoder) bytesAt(seconds float64, size int64) int64 {
	if !d.usesTOC() {
		return min(int64((seconds+d.trimSeconds())*float64(d.bitrate)/8), size)
	}
	percent := min(seconds/d.duration()*100, 99.999)
	i := int(percent)
	lo, hi := float64(d.tag.toc[i]), 256.0
	if i < 99 {
		hi = float64(d.tag.toc[i+1])
	}
	return int64((lo + (hi-lo)*(percent-float64(i))) / 256 * float64(size))
}

// secondsAt is the inverse of bytesAt, for reporting where a seek landed.
func (d *mp3Decoder) secondsAt(offset, size int64) float64 {
	if !d.usesTOC() {
		return float64(offset)*8/float64(d.bitrate) - d.trimSeconds()
	}
	position := float64(offset) / float64(size) * 256
	for i := 99; i >= 0; i-- {
		lo := float64(d.tag.toc[i])
		if position < lo {
			continue
		}
		hi := 256.0
		if i < 99 {
			hi = float64(d.tag.toc[i+1])
		}
		fraction := 0.0
		if hi > lo {
			fraction = min((position-lo)/(hi-lo), 1)
		}
		return (float64(i) + fraction) / 100 * d.duration()
	}
	return 0
}
