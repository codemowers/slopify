//go:build linux

package lirc

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func (t *Transmitter) open() error {
	fd, err := unix.Open(t.Path, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", t.Path, err)
	}
	features, err := unix.IoctlGetUint32(fd, getFeatures)
	if err != nil || features&canSendPulse == 0 {
		unix.Close(fd)
		return fmt.Errorf("%s cannot transmit", t.Path)
	}
	if err := unix.IoctlSetPointerInt(fd, setSendMode, modeScancode); err != nil {
		unix.Close(fd)
		return fmt.Errorf("%s has no scancode transmit: %w", t.Path, err)
	}
	t.fd, t.ok = fd, true
	return nil
}

// Send blocks while the kernel transmits the command.
func (t *Transmitter) Send(command uint8) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ok {
		if err := t.open(); err != nil {
			return err
		}
	}
	frame := necxFrame(t.Address, command)
	n, err := unix.Write(t.fd, frame)
	if err == nil && n != len(frame) {
		err = errors.New("short write")
	}
	if err != nil {
		unix.Close(t.fd)
		t.ok = false
		return fmt.Errorf("sending to %s: %w", t.Path, err)
	}
	return nil
}
