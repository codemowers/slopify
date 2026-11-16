//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/codemowers/slopify/player/internal/linuxioctl"
	"golang.org/x/sys/unix"
)

// Learning reads already-decoded scancodes. It never changes keymaps or decoder
// configuration. An exclusive evdev grab suppresses playback/volume actions for
// the bounded capture, while LIRC still reports complete 64-bit scancodes.
func irLearn(ctx context.Context, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("ir-learn requires root")
	}
	fl := flag.NewFlagSet("ir-learn", flag.ContinueOnError)
	key := fl.String("receiver", "", "physical receiver key")
	protocol := fl.String("protocol", "", "configured remote protocol")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if _, err := irKeymap(*protocol, nil); err != nil {
		return err
	}
	var receiver irReceiver
	for _, r := range irReceivers("/sys/class/rc") {
		if r.Key == *key {
			if receiver.ID != "" {
				return fmt.Errorf("receiver identity is ambiguous")
			}
			receiver = r
		}
	}
	if receiver.LIRC == "" {
		return fmt.Errorf("configured receiver has no LIRC scancode interface")
	}
	input, err := unix.Open(receiver.Input, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(input)
	grab := uint(linuxioctl.Request(1, 'E', 0x90, 4))
	if err := unix.IoctlSetInt(input, grab, 1); err != nil {
		return fmt.Errorf("receiver is busy: %w", err)
	}
	defer unix.IoctlSetInt(input, grab, 0)
	fd, err := unix.Open(receiver.LIRC, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// LIRC_SET_REC_MODE, LIRC_MODE_SCANCODE.
	if err := unix.IoctlSetPointerInt(fd, uint(linuxioctl.Request(1, 'i', 0x12, 4)), 8); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	buf := make([]byte, 24*16)
	var captured *uint64
	var lastSignal time.Time
	finish := func() error {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Scancode string `json:"scancode"`
		}{fmt.Sprintf("0x%x", *captured)})
	}
	for ctx.Err() == nil {
		if captured != nil && time.Since(lastSignal) >= 400*time.Millisecond {
			return finish()
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("receiver disconnected")
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return err
		}
		for offset := 0; offset+24 <= n; offset += 24 {
			b := buf[offset : offset+24]
			lastSignal = time.Now()
			scan, ok := learnedIRScancode(*protocol, binary.NativeEndian.Uint16(b[8:]), binary.NativeEndian.Uint16(b[10:]), binary.NativeEndian.Uint64(b[16:]))
			if ok && captured == nil {
				captured = &scan
			}
		}
	}
	if captured != nil {
		return finish()
	}
	return fmt.Errorf("no matching IR code received; save the receiver/protocol, then press the remote button during learning")
}

func learnedIRScancode(protocol string, flags, rcProtocol uint16, scan uint64) (uint64, bool) {
	if flags&2 != 0 {
		return 0, false
	} // LIRC_SCANCODE_FLAG_REPEAT
	switch protocol {
	case "rc5":
		return scan, rcProtocol >= 2 && rcProtocol <= 4
	case "rc6":
		return scan, rcProtocol >= 15 && rcProtocol <= 19
	case "nec":
		return scan, rcProtocol >= 9 && rcProtocol <= 11
	case "recs80":
		return scan &^ 0x200, rcProtocol == 64 && scan <= 0x3ff
	case "recs80ext":
		return scan &^ 0x400, rcProtocol == 64 && scan >= 0x800 && scan <= 0xfff
	case "kaseikyo-denon":
		// Reject another pulse-distance remote and corrupted Denon frames.
		parity := byte(scan>>16) ^ byte(scan>>24) ^ byte(scan>>32)
		return scan, rcProtocol == 64 && scan <= 0xffffffffffff && scan&0xffff == 0x3254 && parity == byte(scan>>40)
	}
	return 0, false
}
