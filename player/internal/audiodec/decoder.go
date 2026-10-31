// Package audiodec decodes FLAC, WAV and MP3 originals in pure Go with integer
// arithmetic only. Samples are returned as interleaved, left-justified signed
// 32-bit words: widening is lossless and Format.Bits reports the source
// precision. Nothing is resampled. Time offsets are the only floating-point values.
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
	Codec                string // FLAC, WAV or MP3
	Lossy                bool
}

// Decoder produces samples from a single original.
type Decoder interface {
	Format() Format
	// Read fills dst with whole frames and returns the sample count. It returns
	// io.EOF once the stream has ended.
	Read(dst []int32) (int, error)
	// Seek moves near the given offset in seconds and returns where it landed.
	// FLAC and WAV are exact. MP3 has no timestamps: its result is an estimate,
	// close to exact for constant bitrate and rougher for variable bitrate.
	Seek(seconds float64) (float64, error)
}

const (
	maxID3Tags   = 8
	mp3ScanLimit = 1 << 20 // bytes searched for the first frame after any tags
	mp3ScanStep  = 64 << 10
	mp3ScanMatch = 3 // consecutive consistent frames that identify a real start
)

// Open identifies an original by its content, not its name. ID3 tags are skipped
// unread: display metadata comes from the Slopify catalog.
func Open(r io.ReadSeeker) (Decoder, error) {
	audio, magic, err := skipID3(r)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	switch {
	case string(magic[:4]) == "fLaC":
		return openFLAC(r)
	case string(magic[:4]) == "RIFF" && string(magic[8:12]) == "WAVE":
		return openWAV(r)
	}
	start, err := findMP3Start(r, audio)
	if err != nil {
		return nil, err
	}
	return openMP3(r, start)
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

// findMP3Start scans for consecutive Layer III frames, tolerating padding or junk
// between tags and audio. It reads only as far as it needs.
func findMP3Start(r io.ReadSeeker, from int64) (int64, error) {
	if _, err := r.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	var buf []byte
	atEnd := false
	for pos := 0; pos < mp3ScanLimit; pos++ {
		// Enough for the longest run of frames being checked.
		for !atEnd && len(buf) < pos+mp3ScanMatch*2048 {
			chunk := make([]byte, mp3ScanStep)
			n, err := io.ReadFull(r, chunk)
			buf = append(buf, chunk[:n]...)
			atEnd = err != nil
		}
		if pos+4 > len(buf) {
			break
		}
		if mp3FramesAt(buf, pos) {
			return from + int64(pos), nil
		}
	}
	return 0, ErrUnsupported
}

func mp3FramesAt(buf []byte, pos int) bool {
	first, ok := parseMP3Header(buf[pos:])
	if !ok {
		return false
	}
	at := pos
	for range mp3ScanMatch {
		if at+4 > len(buf) {
			return at > pos // Very short file: what exists was consistent.
		}
		h, ok := parseMP3Header(buf[at:])
		if !ok || h.version != first.version || h.rateIndex != first.rateIndex || h.channel != first.channel {
			return false
		}
		at += h.size
	}
	return true
}

// leftJustify widens a sample of the given precision to a full-scale word.
func leftJustify(v int32, bits int) int32 { return v << (32 - bits) }
