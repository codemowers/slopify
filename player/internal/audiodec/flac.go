//go:build !no_flac

package audiodec

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
)

const (
	flacBufferSize  = 64 << 10
	flacScanChunk   = 64 << 10
	flacBisectSpan  = 256 << 10 // bytes below which a seek walks frames rather than bisecting
	flacMaxGapSecs  = 600       // a resync never fills more silence than this
	flacMaxSkipRead = 1 << 30   // bytes a resync may scan, bounding pathological files
)

// flacDecoder decodes frames itself over a positioned reader, so that damaged data
// can be skipped: every frame carries its position and two checksums, so the next
// intact frame is found by scanning and the hole is filled with the exact silence
// it stood for. A live stream that cannot seek uses the library's parser instead,
// without resynchronisation.
type flacDecoder struct {
	stream *flac.Stream
	format Format
	frame  *frame.Frame // current frame, partly consumed
	offset int          // next frame index within it
	ended  bool         // sought to or past the last sample, or past the end of the data

	// Seekable originals only.
	src       *bufReader
	dataStart int64  // first frame header
	size      int64  // file length
	total     uint64 // samples per channel from StreamInfo; zero when unknown
	fixed     uint64 // block size of a fixed-block-size stream, for frame numbers
	next      uint64 // first sample of the frame expected next
	silence   uint64 // frames of silence to play before the current frame
	opts      Options
}

func openFLAC(r io.ReadSeeker, audio int64, opts Options) (Decoder, error) {
	stream, err := flac.NewSeek(r)
	if err != nil {
		return nil, fmt.Errorf("flac: %w", err)
	}
	d, err := newFLACDecoder(stream)
	if err != nil {
		return nil, err
	}
	if d.dataStart, err = flacDataStart(r, audio); err != nil {
		return nil, fmt.Errorf("flac: %w", err)
	}
	if d.size, err = r.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	if d.src, err = newBufReader(r, d.dataStart, flacBufferSize); err != nil {
		return nil, err
	}
	info := stream.Info
	d.total, d.opts = info.NSamples, opts
	if info.BlockSizeMin == info.BlockSizeMax {
		d.fixed = uint64(info.BlockSizeMax)
	}
	return d, nil
}

func newFLACDecoder(stream *flac.Stream) (*flacDecoder, error) {
	info := stream.Info
	if info.SampleRate == 0 || info.NChannels == 0 || info.BitsPerSample == 0 || info.BitsPerSample > 32 {
		return nil, fmt.Errorf("%w: flac stream shape", ErrUnsupported)
	}
	return &flacDecoder{stream: stream, format: Format{Rate: int(info.SampleRate), Channels: int(info.NChannels), Bits: int(info.BitsPerSample), Codec: "FLAC"}}, nil
}

// flacDataStart walks the metadata block headers that follow the signature.
func flacDataStart(r io.ReadSeeker, audio int64) (int64, error) {
	pos := audio + 4
	var head [4]byte
	for range 1 << 10 {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return 0, err
		}
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return 0, err
		}
		pos += 4 + (int64(head[1])<<16 | int64(head[2])<<8 | int64(head[3]))
		if head[0]&0x80 != 0 {
			return pos, nil
		}
	}
	return 0, errors.New("too many metadata blocks")
}

func (d *flacDecoder) Format() Format { return d.format }

// check rejects a frame that does not belong to this stream.
func (d *flacDecoder) check(f *frame.Frame) error {
	if len(f.Subframes) != d.format.Channels || int(f.BitsPerSample) != d.format.Bits || int(f.SampleRate) != d.format.Rate {
		return errors.New("flac: stream shape changed between frames")
	}
	if max := d.stream.Info.BlockSizeMax; max != 0 && f.BlockSize > max {
		return errors.New("flac: frame larger than the stream's maximum block size")
	}
	for _, sub := range f.Subframes {
		if len(sub.Samples) != int(f.BlockSize) {
			return errors.New("flac: truncated frame")
		}
	}
	return nil
}

// first returns the first sample of a frame of this stream.
func (d *flacDecoder) first(f *frame.Frame) uint64 {
	if !f.HasFixedBlockSize {
		return f.Num
	}
	if d.fixed == 0 {
		d.fixed = uint64(f.BlockSize)
	}
	return f.Num * d.fixed
}

func (d *flacDecoder) seconds(sample uint64) float64 {
	return float64(sample) / float64(d.format.Rate)
}

// advance makes f the current frame. It expects f to start at d.next, or later
// when silence fills the gap.
func (d *flacDecoder) advance(f *frame.Frame, start uint64) {
	d.frame, d.offset = f, 0
	d.next = start + uint64(f.BlockSize)
}

// nextFrame reads the frame expected at d.next, resynchronising past damage.
func (d *flacDecoder) nextFrame() error {
	if d.src == nil {
		f, err := d.stream.ParseNext()
		if err != nil {
			return err
		}
		if err := d.check(f); err != nil {
			return err
		}
		d.frame, d.offset = f, 0
		return nil
	}
	pos := d.src.pos()
	f, err := frame.Parse(d.src)
	if failed := d.src.failed(); failed != nil {
		return failed // never resynchronise past bytes that could not be read
	}
	if err == nil {
		err = d.check(f)
	}
	if err == nil {
		start := d.first(f)
		if start == d.next {
			d.advance(f, start)
			return nil
		}
		err = fmt.Errorf("flac: frame at sample %d, expected %d", start, d.next)
	}
	if d.total != 0 && d.next >= d.total {
		return io.EOF // everything decoded; whatever follows, such as a tag, is not audio
	}
	if errors.Is(err, io.EOF) && pos >= d.size {
		if d.total > d.next {
			d.opts.report(Issue{Codec: "FLAC", Kind: IssueTruncated, At: d.seconds(d.next), Offset: pos, Missing: d.seconds(d.total - d.next)})
		}
		return io.EOF
	}
	// Damage: find the next intact frame of this stream after the one that failed.
	f, at, scanErr := d.scan(pos+1, func(start uint64) bool {
		return start >= d.next && start-d.next <= uint64(flacMaxGapSecs*d.format.Rate) && (d.total == 0 || start < d.total)
	})
	if scanErr != nil {
		if failed := d.src.failed(); failed != nil {
			return failed
		}
		issue := Issue{Codec: "FLAC", Kind: IssueTruncated, At: d.seconds(d.next), Offset: pos, Bytes: d.size - pos, Err: err}
		if d.total > d.next {
			issue.Missing = d.seconds(d.total - d.next)
		}
		d.opts.report(issue)
		return io.EOF
	}
	start := d.first(f)
	d.opts.report(Issue{Codec: "FLAC", Kind: IssueResync, At: d.seconds(d.next), Offset: pos, Bytes: at - pos, Missing: d.seconds(start - d.next), Err: err})
	d.silence = start - d.next
	d.advance(f, start)
	return nil
}

// scan finds the first intact frame at or after from whose first sample is
// acceptable, leaving the reader just after it. Both checksums must match, so a
// stray sync pattern inside audio data is not mistaken for a frame.
func (d *flacDecoder) scan(from int64, accept func(uint64) bool) (*frame.Frame, int64, error) {
	buf := make([]byte, flacScanChunk)
	limit := min(d.size, from+flacMaxSkipRead)
	for chunk := from; chunk < limit-1; {
		if err := d.src.seek(chunk); err != nil {
			return nil, 0, err
		}
		n, _ := io.ReadFull(d.src, buf[:min(int64(len(buf)), limit-chunk)])
		if err := d.src.failed(); err != nil {
			return nil, 0, err
		}
		if n < 2 {
			break
		}
		for i := 0; i+1 < n; i++ {
			if buf[i] != 0xFF || buf[i+1]&0xFE != 0xF8 {
				continue
			}
			at := chunk + int64(i)
			if err := d.src.seek(at); err != nil {
				return nil, 0, err
			}
			f, err := frame.Parse(d.src)
			if failed := d.src.failed(); failed != nil {
				return nil, 0, failed
			}
			if err != nil || d.check(f) != nil || !accept(d.first(f)) {
				continue
			}
			return f, at, nil
		}
		chunk += int64(n - 1)
	}
	return nil, 0, io.EOF
}

func (d *flacDecoder) Read(dst []int32) (int, error) {
	if d.ended {
		return 0, io.EOF
	}
	channels := d.format.Channels
	n := 0
	for len(dst)-n >= channels {
		if d.silence == 0 && (d.frame == nil || d.offset == int(d.frame.BlockSize)) {
			if err := d.nextFrame(); err != nil {
				if errors.Is(err, io.EOF) {
					d.ended = true
					if n > 0 {
						return n, nil
					}
				}
				return n, err
			}
		}
		if d.silence > 0 {
			frames := min(uint64((len(dst)-n)/channels), d.silence)
			clear(dst[n : n+int(frames)*channels])
			n += int(frames) * channels
			d.silence -= frames
			continue
		}
		frames := min((len(dst)-n)/channels, int(d.frame.BlockSize)-d.offset)
		for i := 0; i < frames; i++ {
			for ch := 0; ch < channels; ch++ {
				dst[n] = leftJustify(d.frame.Subframes[ch].Samples[d.offset+i], d.format.Bits)
				n++
			}
		}
		d.offset += frames
	}
	return n, nil
}

func (d *flacDecoder) Seek(seconds float64) (float64, error) {
	target := uint64(math.Max(seconds, 0) * float64(d.format.Rate))
	// A position can reach the end of a track that has just finished elsewhere;
	// that is the end of the stream rather than a failure.
	if d.total != 0 && target >= d.total {
		d.frame, d.silence, d.ended = nil, 0, true
		return d.seconds(d.total), nil
	}
	if d.src == nil {
		return 0, errors.New("flac seek: stream cannot seek")
	}
	d.ended, d.frame, d.silence = false, nil, 0
	// Bisect on byte offsets using intact frames only, so damage anywhere else in
	// the file cannot prevent a seek, then walk the last stretch frame by frame.
	inStream := func(start uint64) bool { return d.total == 0 || start < d.total }
	lo, hi := d.dataStart, d.size
	for hi-lo > flacBisectSpan {
		mid := lo + (hi-lo)/2
		f, at, err := d.scan(mid, inStream)
		if err != nil {
			if failed := d.src.failed(); failed != nil {
				return 0, failed
			}
			hi = mid
			continue
		}
		if d.first(f) > target {
			hi = mid
		} else {
			lo = at
		}
	}
	f, at, err := d.scan(lo, inStream)
	if err != nil {
		if failed := d.src.failed(); failed != nil {
			return 0, failed
		}
		return 0, fmt.Errorf("flac seek: no intact frame after offset %d", lo)
	}
	start := d.first(f)
	if start > target {
		// Damage at the start of the data: play the hole as silence, as Read would.
		if at > d.dataStart {
			d.opts.report(Issue{Codec: "FLAC", Kind: IssueResync, At: d.seconds(target), Offset: d.dataStart, Bytes: at - d.dataStart, Missing: d.seconds(start - target)})
		}
		d.silence = start - target
		d.advance(f, start)
		return seconds, nil
	}
	d.advance(f, start)
	for d.next <= target {
		if err := d.nextFrame(); err != nil {
			if errors.Is(err, io.EOF) {
				d.frame, d.silence, d.ended = nil, 0, true
				return d.seconds(d.next), nil
			}
			return 0, err
		}
		if frameStart := d.next - uint64(d.frame.BlockSize); target < frameStart {
			d.silence = frameStart - target // the target lies in a filled gap
			return d.seconds(target), nil
		}
		d.silence = 0
	}
	d.offset = int(target - (d.next - uint64(d.frame.BlockSize)))
	return d.seconds(target), nil
}

const flacEnabled = true

func openFLACStream(r io.Reader) (Decoder, error) {
	stream, err := flac.New(r)
	if err != nil {
		return nil, fmt.Errorf("flac: %w", err)
	}
	return newFLACDecoder(stream)
}
