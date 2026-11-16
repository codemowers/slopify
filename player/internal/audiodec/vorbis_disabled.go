//go:build no_vorbis || mips.softfloat || mipsle.softfloat || mips64.softfloat || mips64le.softfloat

package audiodec

import "io"

const vorbisEnabled = false

func openVorbis(io.ReadSeeker, Options) (Decoder, error) { return nil, disabledCodec("Vorbis") }
