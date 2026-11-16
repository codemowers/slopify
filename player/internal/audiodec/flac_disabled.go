//go:build no_flac

package audiodec

import "io"

const flacEnabled = false

func openFLAC(io.ReadSeeker, int64, Options) (Decoder, error) { return nil, disabledCodec("FLAC") }
func openFLACStream(io.Reader) (Decoder, error)               { return nil, disabledCodec("FLAC") }
