//go:build no_wav

package audiodec

import "io"

const wavEnabled = false

func openWAV(io.ReadSeeker) (Decoder, error)              { return nil, disabledCodec("WAV") }
func openWAVWith(io.ReadSeeker, Options) (Decoder, error) { return nil, disabledCodec("WAV") }
