package audiodec

import (
	"errors"
	"fmt"
	"io"
)

// Issue describes damage a decoder worked around in an original. Decoding goes on:
// the damaged section is replaced by silence of the same length where the stream
// says how long it was (FLAC frames, MP3 frames failing their CRC), dropped
// (bytes between MP3 frames, Vorbis pages failing their CRC), or ends the track
// (truncation).
type Issue struct {
	Codec string // FLAC, MP3, WAV ...
	Kind  string // IssueResync, IssueTruncated or IssueChecksum
	// At is the track position, in seconds, where the damage starts.
	At float64
	// Offset is the byte offset in the file of the first undecodable byte, or -1
	// when the decoder cannot tell.
	Offset int64
	// Bytes is how much of the file was skipped, when known.
	Bytes int64
	// Missing is the length of audio, in seconds, lost to the damage: replaced by
	// silence, or absent after an MP3 resync and at a truncated end. Zero when the
	// decoder cannot tell, as for Vorbis.
	Missing float64
	// Err is the decoder error that revealed the damage, if any.
	Err error
}

// Issue kinds. Keep the set small: it is a metric label.
const (
	IssueResync    = "resync"    // undecodable data skipped, decoding resumed after it
	IssueTruncated = "truncated" // the file ends before its audio does
	IssueChecksum  = "checksum"  // frames failed their checksum and were played as silence
)

func (i Issue) String() string {
	s := fmt.Sprintf("%s %s at=%.3fs", i.Codec, i.Kind, i.At)
	if i.Offset >= 0 {
		s += fmt.Sprintf(" offset=%d", i.Offset)
	}
	if i.Bytes > 0 {
		s += fmt.Sprintf(" skipped=%dB", i.Bytes)
	}
	if i.Missing > 0 {
		s += fmt.Sprintf(" missing=%.3fs", i.Missing)
	}
	if i.Err != nil {
		s += ": " + i.Err.Error()
	}
	return s
}

// Options adjust how an original is decoded.
type Options struct {
	// Report, if set, receives each piece of damage that decoding worked around.
	// It is called synchronously from Read or Seek and must not block for long.
	Report func(Issue)
}

func (o Options) report(i Issue) {
	if o.Report != nil {
		o.Report(i)
	}
}

// bufReader is a buffered ReadSeeker that knows its position and keeps transport
// failures apart from damaged content: a decoder must never resynchronise past
// bytes it could not fetch.
type bufReader struct {
	r        io.ReadSeeker
	buf      []byte
	start    int64 // file offset of buf[0]
	off, end int   // read position and fill level within buf
	err      error // first read failure other than EOF
}

func newBufReader(r io.ReadSeeker, at int64, size int) (*bufReader, error) {
	b := &bufReader{r: r, buf: make([]byte, size)}
	return b, b.seek(at)
}

func (b *bufReader) pos() int64 { return b.start + int64(b.off) }

func (b *bufReader) seek(at int64) error {
	if at >= b.start && at <= b.start+int64(b.end) {
		b.off = int(at - b.start)
		return nil
	}
	if _, err := b.r.Seek(at, io.SeekStart); err != nil {
		b.err = err
		return err
	}
	b.start, b.off, b.end = at, 0, 0
	return nil
}

func (b *bufReader) Read(p []byte) (int, error) {
	if b.off == b.end {
		if b.err != nil {
			return 0, b.err
		}
		b.start += int64(b.end)
		b.off, b.end = 0, 0
		n, err := b.r.Read(b.buf)
		b.end = n
		if n == 0 {
			if err == nil {
				err = io.ErrNoProgress
			}
			if !errors.Is(err, io.EOF) {
				b.err = err
			}
			return 0, err
		}
	}
	n := copy(p, b.buf[b.off:b.end])
	b.off += n
	return n, nil
}

// failed reports a transport error seen while reading, as opposed to bad content.
func (b *bufReader) failed() error { return b.err }
