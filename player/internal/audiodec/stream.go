package audiodec

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"

	"github.com/mewkiz/flac"
)

const streamEnd = int64(1) << 62

// OpenStream decodes live audio from a stream that cannot seek: FLAC or WAV by
// content, or raw big-endian LPCM (audio/L16 and audio/L24) by its Content-Type.
func OpenStream(r io.Reader, contentType string) (Decoder, error) {
	buffered := bufio.NewReader(r)
	if media, params, err := mime.ParseMediaType(contentType); err == nil && strings.HasPrefix(media, "audio/l") {
		return openLPCM(buffered, media, params)
	}
	magic, err := buffered.Peek(12)
	if err != nil {
		return nil, err
	}
	switch {
	case string(magic[:4]) == "fLaC":
		stream, err := flac.New(buffered)
		if err != nil {
			return nil, fmt.Errorf("flac: %w", err)
		}
		return newFLACDecoder(stream)
	case (string(magic[:4]) == "RIFF" || string(magic[:4]) == "RF64") && string(magic[8:12]) == "WAVE":
		return openWAV(&forwardSeeker{r: buffered})
	}
	return nil, fmt.Errorf("%w: stream format", ErrUnsupported)
}

// forwardSeeker lets the WAV parser walk a stream: it can skip ahead and sees an
// endless length, which is what a streaming writer's data chunk size means.
type forwardSeeker struct {
	r   io.Reader
	pos int64
}

func (f *forwardSeeker) Read(b []byte) (int, error) {
	n, err := f.r.Read(b)
	f.pos += int64(n)
	return n, err
}

func (f *forwardSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekEnd:
		return streamEnd, nil
	case io.SeekCurrent:
		offset += f.pos
	}
	if offset < f.pos {
		return 0, errors.New("stream cannot seek backwards")
	}
	if _, err := io.CopyN(io.Discard, f.r, offset-f.pos); err != nil {
		return 0, err
	}
	f.pos = offset
	return offset, nil
}

type lpcmDecoder struct {
	r      io.Reader
	format Format
	size   int
	buf    []byte
}

func openLPCM(r io.Reader, media string, params map[string]string) (Decoder, error) {
	d := &lpcmDecoder{r: r}
	switch media {
	case "audio/l16":
		d.size = 2
	case "audio/l24":
		d.size = 3
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, media)
	}
	rate, err := strconv.Atoi(params["rate"])
	if err != nil || rate < 8000 || rate > 384000 {
		return nil, fmt.Errorf("%w: lpcm rate", ErrUnsupported)
	}
	channels := 1 // RFC 3551's default.
	if text, ok := params["channels"]; ok {
		if channels, err = strconv.Atoi(text); err != nil || channels < 1 || channels > 8 {
			return nil, fmt.Errorf("%w: lpcm channels", ErrUnsupported)
		}
	}
	d.format = Format{Rate: rate, Channels: channels, Bits: d.size * 8, Codec: "LPCM"}
	return d, nil
}

func (d *lpcmDecoder) Format() Format { return d.format }

func (d *lpcmDecoder) Read(dst []int32) (int, error) {
	frame := d.size * d.format.Channels
	want := len(dst) / d.format.Channels * frame
	if want == 0 {
		return 0, nil
	}
	if cap(d.buf) < want {
		d.buf = make([]byte, want)
	}
	b := d.buf[:want]
	got, err := io.ReadFull(d.r, b)
	got -= got % frame
	if got == 0 {
		if err == io.ErrUnexpectedEOF {
			return 0, io.EOF
		}
		return 0, err
	}
	n := got / d.size
	for i := range n {
		p := b[i*d.size:]
		if d.size == 2 {
			dst[i] = int32(int16(binary.BigEndian.Uint16(p))) << 16
		} else {
			dst[i] = int32(uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8)
		}
	}
	return n, nil
}

func (d *lpcmDecoder) Seek(float64) (float64, error) {
	return 0, errors.New("live stream cannot seek")
}
