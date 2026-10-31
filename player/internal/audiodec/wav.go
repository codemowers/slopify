package audiodec

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	wavPCM        = 1
	wavExtensible = 0xfffe
)

type wavDecoder struct {
	r           io.ReadSeeker
	format      Format
	container   int
	start, size int64
	remaining   int64
	buf         []byte
}

func openWAV(r io.ReadSeeker) (Decoder, error) {
	if _, err := r.Seek(12, io.SeekStart); err != nil {
		return nil, err
	}
	d := &wavDecoder{r: r}
	var haveFormat bool
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("%w: wav data chunk missing", ErrUnsupported)
		}
		size := int64(binary.LittleEndian.Uint32(h[4:]))
		switch string(h[:4]) {
		case "fmt ":
			if size < 16 || size > 40 {
				return nil, fmt.Errorf("%w: wav format chunk", ErrUnsupported)
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, err
			}
			tag := binary.LittleEndian.Uint16(b)
			if tag == wavExtensible && size >= 26 {
				tag = binary.LittleEndian.Uint16(b[24:])
			}
			if tag != wavPCM {
				return nil, fmt.Errorf("%w: non-integer wav", ErrUnsupported)
			}
			channels := int(binary.LittleEndian.Uint16(b[2:]))
			container := int(binary.LittleEndian.Uint16(b[14:]))
			if channels < 1 || channels > 8 || (container != 8 && container != 16 && container != 24 && container != 32) {
				return nil, fmt.Errorf("%w: wav sample shape", ErrUnsupported)
			}
			d.format = Format{Rate: int(binary.LittleEndian.Uint32(b[4:])), Channels: channels, Bits: container, Codec: "WAV"}
			d.container = container / 8
			haveFormat = true
			if size%2 == 1 {
				r.Seek(1, io.SeekCurrent)
			}
		case "data":
			if !haveFormat || d.format.Rate == 0 {
				return nil, fmt.Errorf("%w: wav data before format", ErrUnsupported)
			}
			start, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, err
			}
			end, err := r.Seek(0, io.SeekEnd)
			if err != nil {
				return nil, err
			}
			// Streaming writers leave the size as zero or all ones.
			if size == 0 || size > end-start {
				size = end - start
			}
			if _, err := r.Seek(start, io.SeekStart); err != nil {
				return nil, err
			}
			d.start, d.size, d.remaining = start, size, size
			return d, nil
		default:
			if _, err := r.Seek(size+size%2, io.SeekCurrent); err != nil {
				return nil, err
			}
		}
	}
}

func (d *wavDecoder) Format() Format { return d.format }

func (d *wavDecoder) Read(dst []int32) (int, error) {
	frameBytes := d.container * d.format.Channels
	frames := len(dst) / d.format.Channels
	if frames == 0 {
		return 0, nil
	}
	want := min(int64(frames*frameBytes), d.remaining/int64(frameBytes)*int64(frameBytes))
	if want == 0 {
		return 0, io.EOF
	}
	if int64(cap(d.buf)) < want {
		d.buf = make([]byte, want)
	}
	b := d.buf[:want]
	got, err := io.ReadFull(d.r, b)
	got -= got % frameBytes
	if got == 0 {
		if err == io.ErrUnexpectedEOF {
			return 0, io.EOF
		}
		return 0, err
	}
	d.remaining -= int64(got)
	n := got / d.container
	for i := 0; i < n; i++ {
		p := b[i*d.container:]
		switch d.container {
		case 1: // WAV stores 8-bit samples unsigned.
			dst[i] = int32(int8(p[0]^0x80)) << 24
		case 2:
			dst[i] = int32(int16(binary.LittleEndian.Uint16(p))) << 16
		case 3:
			dst[i] = int32(uint32(p[0])<<8 | uint32(p[1])<<16 | uint32(p[2])<<24)
		case 4:
			dst[i] = int32(binary.LittleEndian.Uint32(p))
		}
	}
	return n, nil
}

func (d *wavDecoder) Seek(seconds float64) (float64, error) {
	frameBytes := int64(d.container * d.format.Channels)
	offset := int64(math.Max(seconds, 0)*float64(d.format.Rate)) * frameBytes
	offset = min(offset, d.size/frameBytes*frameBytes)
	if _, err := d.r.Seek(d.start+offset, io.SeekStart); err != nil {
		return 0, err
	}
	d.remaining = d.size - offset
	return float64(offset/frameBytes) / float64(d.format.Rate), nil
}
