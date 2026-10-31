// Package lirc sends infrared commands through the kernel's LIRC device. The kernel
// encodes each scancode and generates the carrier; nothing here touches pins.
package lirc

import (
	"encoding/binary"
	"sync"
)

const (
	protoNECX = 10 // RC_PROTO_NECX

	modeScancode = 0x8 // LIRC_MODE_SCANCODE
	canSendPulse = 0x2 // LIRC_CAN_SEND_PULSE

	getFeatures = 0x80046900 // LIRC_GET_FEATURES
	setSendMode = 0x40046911 // LIRC_SET_SEND_MODE
)

// necxFrame is a struct lirc_scancode for an extended NEC command.
func necxFrame(address uint16, command uint8) []byte {
	frame := make([]byte, 24)
	binary.LittleEndian.PutUint16(frame[8:], 0) // flags
	binary.LittleEndian.PutUint16(frame[10:], protoNECX)
	binary.LittleEndian.PutUint64(frame[16:], uint64(address)<<8|uint64(command))
	return frame
}

// Transmitter sends NEC extended commands. It opens the device when first used and
// again after a failure, so the device may appear after the player starts.
type Transmitter struct {
	Path    string
	Address uint16

	mu sync.Mutex
	fd int
	ok bool
}
