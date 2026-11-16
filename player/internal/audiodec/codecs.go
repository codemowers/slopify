package audiodec

import (
	"fmt"
	"strings"
)

// Enabled reports compiled-in native decoders. PCM transports remain available.
func Enabled(codec string) bool {
	switch strings.ToLower(codec) {
	case "flac":
		return flacEnabled
	case "wav", "pcm":
		return wavEnabled
	case "mp3":
		return mp3Enabled
	case "vorbis":
		return vorbisEnabled
	case "alac":
		return alacEnabled
	}
	return false
}
func disabledCodec(name string) error {
	return fmt.Errorf("%w: %s decoder disabled at build time", ErrUnsupported, name)
}
