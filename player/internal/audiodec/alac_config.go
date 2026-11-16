package audiodec

import (
	"encoding/binary"
	"errors"
)

// ALACConfig is the ALACSpecificConfig that accompanies a stream, carried by an
// MP4 or CAF magic cookie, or spelled out in an RAOP SDP fmtp line.
type ALACConfig struct {
	FrameLength                        uint32
	BitDepth, Channels                 int
	HistoryMult, InitialHistory, Limit uint8
	SampleRate                         int
	// MaxFrames admits packets that state a size larger than FrameLength, up to
	// this many frames. Some RAOP senders fill packets to the MTU regardless of
	// what they announced. Zero means FrameLength.
	MaxFrames uint32
}

// ParseALACConfig reads a 24-byte ALACSpecificConfig, tolerating the 12-byte "frma"
// and "alac" atom headers that containers keep in front of it.
func ParseALACConfig(b []byte) (ALACConfig, error) {
	if len(b) >= 12 && string(b[4:8]) == "frma" {
		b = b[12:]
	}
	if len(b) >= 36 && string(b[4:8]) == "alac" {
		b = b[12:]
	}
	if len(b) < 24 {
		return ALACConfig{}, errors.New("alac: short config")
	}
	return ALACConfig{
		FrameLength: binary.BigEndian.Uint32(b), BitDepth: int(b[5]),
		HistoryMult: b[6], InitialHistory: b[7], Limit: b[8], Channels: int(b[9]),
		SampleRate: int(binary.BigEndian.Uint32(b[20:])),
	}, nil
}
