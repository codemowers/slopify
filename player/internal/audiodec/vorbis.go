//go:build !no_vorbis && !mips.softfloat && !mipsle.softfloat && !mips64.softfloat && !mips64le.softfloat

package audiodec

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/jfreymuth/oggvorbis"
)

// Vorbis is the one decoder here that uses floating point. The format is lossy
// and its specification has no exact output, only real-number maths that the
// reference decoder evaluates in float32; an integer decoder would be no more
// faithful. Samples are converted to 32-bit words once, at the end.
type vorbisDecoder struct {
	r      *oggvorbis.Reader
	format Format
	buf    []float32

	opts   Options
	pos    int64 // frames Read has returned since the last seek target
	failed int   // consecutive errors without audio in between
}

// After a page fails its CRC the library drops it and decodes on. How much audio
// went with it is not exposed (Position only corrects itself at the end of the
// stream), so the damage is reported without a length and nothing is filled in:
// like an MP3 resync, later audio plays that much early. Errors that keep coming
// without audio are not damage that decoding can get past.
const vorbisMaxConsecutiveErrors = 16

// isVorbis reports whether the first Ogg page begins a Vorbis identification
// header. Ogg also carries Opus and FLAC, which are left to other decoders.
func isVorbis(r io.ReadSeeker) (bool, error) {
	var head [27 + 255]byte
	n, err := io.ReadFull(r, head[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	if n < 27 || string(head[:4]) != "OggS" {
		return false, nil
	}
	at := 27 + int(head[26])
	return n >= at+7 && string(head[at:at+7]) == "\x01vorbis", nil
}

func openVorbis(r io.ReadSeeker, opts Options) (Decoder, error) {
	ok, err := isVorbis(r)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: ogg stream is not vorbis", ErrUnsupported)
	}
	vr, err := oggvorbis.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("vorbis: %w", err)
	}
	// Vorbis orders surround channels differently from WAV and ALSA; until they
	// are remapped, only mono and stereo are played.
	if vr.SampleRate() <= 0 || vr.Channels() < 1 || vr.Channels() > 2 {
		return nil, fmt.Errorf("%w: vorbis stream shape", ErrUnsupported)
	}
	return &vorbisDecoder{r: vr, opts: opts,
		format: Format{Rate: vr.SampleRate(), Channels: vr.Channels(), Bits: vorbisPrecision, Codec: "Vorbis", Lossy: true}}, nil
}

// vorbisPrecision is nominal: float32 holds a 24-bit mantissa.
const vorbisPrecision = 24

func (d *vorbisDecoder) Format() Format { return d.format }

func (d *vorbisDecoder) Read(dst []int32) (int, error) {
	channels := d.format.Channels
	dst = dst[:len(dst)/channels*channels]
	if len(dst) == 0 {
		return 0, nil
	}
	if cap(d.buf) < len(dst) {
		d.buf = make([]float32, len(dst))
	}
	n := 0
	for n < len(dst) {
		buf := d.buf[:len(dst)-n]
		got, err := d.r.Read(buf)
		convertVorbis(dst[n:n+got], buf[:got])
		n += got
		d.pos += int64(got / channels)
		if got > 0 {
			d.failed = 0
		}
		if errors.Is(err, io.EOF) {
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		if err != nil {
			if d.failed++; d.failed > vorbisMaxConsecutiveErrors {
				return n, fmt.Errorf("vorbis: %w", err)
			}
			d.opts.report(Issue{Codec: "Vorbis", Kind: IssueResync, At: float64(d.pos) / float64(d.format.Rate), Offset: -1, Err: err})
			continue
		}
		if got == 0 {
			break
		}
	}
	return n, nil
}

// convertVorbis rounds to 32-bit words. The reader already clips overshoot to
// ±1.0, and +1.0 itself saturates to the largest word.
func convertVorbis(dst []int32, src []float32) {
	for i, v := range src {
		x := math.Round(float64(v) * (1 << 31))
		switch {
		case x >= math.MaxInt32:
			dst[i] = math.MaxInt32
		case x <= math.MinInt32:
			dst[i] = math.MinInt32
		default:
			dst[i] = int32(x)
		}
	}
}

// Seek is sample-exact, but scans page headers from the start of the stream,
// reading the original up to the target.
func (d *vorbisDecoder) Seek(seconds float64) (float64, error) {
	target := int64(math.Max(seconds, 0) * float64(d.format.Rate))
	if total := d.r.Length(); total > 0 && target >= total {
		target = total
	}
	if err := d.r.SetPosition(target); err != nil {
		return 0, fmt.Errorf("vorbis seek: %w", err)
	}
	d.pos, d.failed = target, 0
	return float64(target) / float64(d.format.Rate), nil
}

const vorbisEnabled = true
