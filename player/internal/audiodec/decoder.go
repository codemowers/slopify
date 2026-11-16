// Package audiodec decodes FLAC, WAV, MP3 and Ogg Vorbis originals in pure Go.
// Samples are returned as interleaved, left-justified signed 32-bit words:
// widening is lossless and Format.Bits reports the source precision. Nothing is
// resampled. Decoding uses integer arithmetic only, except for Vorbis, whose
// reference maths is floating point; otherwise time offsets are the only
// floating-point values.
package audiodec

import (
	"errors"
	"fmt"
	"io"
)

// ErrUnsupported marks a container or codec this package does not decode.
var ErrUnsupported = errors.New("unsupported audio format")

// Format describes decoded audio. Bits is the decoded precision; for lossy
// sources it is nominal, since they have no sample format of their own.
type Format struct {
	Rate, Channels, Bits int
	Codec                string // FLAC, WAV, MP3, Vorbis or ALAC
	Lossy                bool
}

// Decoder produces samples from a single original.
type Decoder interface {
	Format() Format
	// Read fills dst with whole frames and returns the sample count. It returns
	// io.EOF once the stream has ended.
	Read(dst []int32) (int, error)
	// Seek moves near the given offset in seconds and returns where it landed.
	// FLAC, WAV and Vorbis are exact. MP3 has no timestamps: its result is an estimate,
	// close to exact for constant bitrate and rougher for variable bitrate.
	Seek(seconds float64) (float64, error)
}

// EncodedInput accounts for complete encoded frames and their decoded duration,
// excluding reader prefetch and container tags. Implemented where frame sizes
// and durations are known directly (MP3).
type EncodedInput interface {
	InputTotals() (bytes, frames uint64)
}

const maxID3Tags = 8

// Open identifies an original by its content, not its name. ID3 tags are skipped
// unread: display metadata comes from the Slopify catalog.
func Open(r io.ReadSeeker) (Decoder, error) { return OpenWith(r, Options{}) }

// OpenWith is Open with options, such as a receiver for damage worked around.
func OpenWith(r io.ReadSeeker, opts Options) (Decoder, error) {
	audio, magic, err := skipID3(r)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	switch {
	case string(magic[:4]) == "fLaC":
		return openFLAC(r, audio, opts)
	case string(magic[:4]) == "RIFF" && string(magic[8:12]) == "WAVE":
		return openWAVWith(r, opts)
	case string(magic[:4]) == "OggS":
		return openVorbis(r, opts)
	}
	return openMP3Original(r, audio, opts)
}

// skipID3 returns the offset after any leading ID3v2 tags and the bytes found
// there. A tag with an invalid size is not skipped; the frame scan handles it.
func skipID3(r io.ReadSeeker) (int64, [12]byte, error) {
	var head [12]byte
	var offset int64
	for range maxID3Tags {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return 0, head, err
		}
		if _, err := io.ReadFull(r, head[:]); err != nil {
			if offset == 0 {
				return 0, head, fmt.Errorf("%w: short file", ErrUnsupported)
			}
			return 0, [12]byte{}, nil // A tag ran past the end; let the scan decide.
		}
		if string(head[:3]) != "ID3" || (head[6]|head[7]|head[8]|head[9])&0x80 != 0 {
			return offset, head, nil
		}
		size := int64(head[6])<<21 | int64(head[7])<<14 | int64(head[8])<<7 | int64(head[9])
		offset += size + 10
		if head[5]&0x10 != 0 { // Footer.
			offset += 10
		}
	}
	return offset, head, nil
}

// leftJustify widens a sample of the given precision to a full-scale word.
func leftJustify(v int32, bits int) int32 { return v << (32 - bits) }
