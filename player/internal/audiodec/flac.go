package audiodec

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
)

type flacDecoder struct {
	stream *flac.Stream
	format Format
	frame  *frame.Frame // current frame, partly consumed
	offset int          // next frame index within it
}

func openFLAC(r io.ReadSeeker) (Decoder, error) {
	stream, err := flac.NewSeek(r)
	if err != nil {
		return nil, fmt.Errorf("flac: %w", err)
	}
	return newFLACDecoder(stream)
}

func newFLACDecoder(stream *flac.Stream) (Decoder, error) {
	info := stream.Info
	if info.SampleRate == 0 || info.NChannels == 0 || info.BitsPerSample == 0 || info.BitsPerSample > 32 {
		return nil, fmt.Errorf("%w: flac stream shape", ErrUnsupported)
	}
	return &flacDecoder{stream: stream, format: Format{Rate: int(info.SampleRate), Channels: int(info.NChannels), Bits: int(info.BitsPerSample), Codec: "FLAC"}}, nil
}

func (d *flacDecoder) Format() Format { return d.format }

func (d *flacDecoder) next() error {
	f, err := d.stream.ParseNext()
	if err != nil {
		return err
	}
	if len(f.Subframes) != d.format.Channels || int(f.BitsPerSample) != d.format.Bits || int(f.SampleRate) != d.format.Rate {
		return errors.New("flac: stream shape changed between frames")
	}
	for _, sub := range f.Subframes {
		if len(sub.Samples) != int(f.BlockSize) {
			return errors.New("flac: truncated frame")
		}
	}
	d.frame, d.offset = f, 0
	return nil
}

func (d *flacDecoder) Read(dst []int32) (int, error) {
	channels := d.format.Channels
	n := 0
	for len(dst)-n >= channels {
		if d.frame == nil || d.offset == int(d.frame.BlockSize) {
			if err := d.next(); err != nil {
				if n > 0 && errors.Is(err, io.EOF) {
					return n, nil
				}
				return n, err
			}
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
	start, err := d.stream.Seek(target)
	if err != nil {
		return 0, fmt.Errorf("flac seek: %w", err)
	}
	d.frame = nil
	if err := d.next(); err != nil {
		return 0, err
	}
	if target < start || target-start > uint64(d.frame.BlockSize) {
		return 0, errors.New("flac seek: landed outside the target frame")
	}
	d.offset = int(target - start)
	return float64(target) / float64(d.format.Rate), nil
}
