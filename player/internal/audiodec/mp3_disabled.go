//go:build no_mp3

package audiodec

import "io"

const mp3Enabled = false

func openMP3Original(io.ReadSeeker, int64, Options) (Decoder, error) {
	return nil, disabledCodec("MP3")
}
