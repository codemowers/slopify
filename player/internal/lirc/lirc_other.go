//go:build !linux

package lirc

import "errors"

// Send is only available on Linux.
func (t *Transmitter) Send(uint8) error { return errors.New("infrared needs Linux") }
