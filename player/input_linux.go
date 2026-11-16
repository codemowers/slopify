//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codemowers/slopify/player/internal/cleantext"
	"golang.org/x/sys/unix"
)

func decodeKnobEvent(b []byte, device *inputDevice) (knobEvent, bool) {
	if len(b) < 8 {
		return knobEvent{}, false
	}
	typ, code, value := binary.NativeEndian.Uint16(b), binary.NativeEndian.Uint16(b[2:]), int32(binary.NativeEndian.Uint32(b[4:]))
	if device.rotary && typ == 2 && code == 0 && (value == 1 || value == -1) {
		return knobEvent{delta: int(value)}, true
	}
	if typ == 1 && (value == 1 || value == 2) {
		switch device.keys[code] {
		case actionVolumeUp:
			return knobEvent{delta: 1}, true
		case actionVolumeDown:
			return knobEvent{delta: -1}, true
		}
	}
	if action := device.keys[code]; action != actionNone && typ == 1 && value == 1 {
		return knobEvent{action: action}, true
	}
	return knobEvent{}, false
}

// inputPath finds the node of a kernel input device name, looking again on each
// open because event numbers change. Paths pass through; an unknown name gives "".
func inputPath(sysClass, device string) string {
	if strings.HasPrefix(device, "/") {
		return device
	}
	dirs, _ := filepath.Glob(filepath.Join(sysClass, "event[0-9]*"))
	for _, dir := range dirs {
		name, err := os.ReadFile(filepath.Join(dir, "device", "name"))
		if err == nil && strings.TrimSpace(string(name)) == device {
			return "/dev/input/" + filepath.Base(dir)
		}
	}
	return ""
}

// The kernel rotary-encoder/gpio-key drivers handle GPIO edges and debounce.
// Only explicitly configured input devices are opened, never keyboard scans.
func followInput(ctx context.Context, path string, device *inputDevice, out chan<- knobEvent) {
	lastPress := map[inputAction]time.Time{}
	for ctx.Err() == nil {
		fd, err := unix.Open(inputPath("/sys/class/input", path), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			slog.Info("input connected", "device", path)
			pressed := map[uint16]bool{}
			for ctx.Err() == nil {
				fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				_, err = unix.Poll(fds, 200)
				if err == unix.EINTR {
					continue
				}
				if err != nil || fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
					break
				}
				if fds[0].Revents&unix.POLLIN == 0 {
					continue
				}
				b := make([]byte, binary.Size(unix.Timeval{})+8)
				n, err := unix.Read(fd, b)
				if err == unix.EAGAIN {
					continue
				}
				if err != nil || n != len(b) {
					break
				}
				event := b[len(b)-8:]
				if binary.NativeEndian.Uint16(event) == 1 {
					code := binary.NativeEndian.Uint16(event[2:])
					value := int32(binary.NativeEndian.Uint32(event[4:]))
					if value == 0 {
						delete(pressed, code)
					}
					if value == 2 && !pressed[code] {
						continue
					}
					if value == 1 {
						pressed[code] = true
					}
				}
				e, ok := decodeKnobEvent(b[len(b)-8:], device)
				if !ok {
					continue
				}
				if e.action != actionNone {
					if time.Since(lastPress[e.action]) < 300*time.Millisecond {
						continue
					}
					lastPress[e.action] = time.Now()
				}
				select {
				case out <- e:
				case <-ctx.Done():
				}
			}
			unix.Close(fd)
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func inputInfo(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "input-info takes no arguments")
		return 1
	}
	names := []string{}
	seen := map[string]bool{}
	devices, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, device := range devices {
		if len(names) >= 64 {
			break
		}
		f, err := os.Open(device)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, 4096))
		f.Close()
		if err != nil {
			continue
		}
		name := cleantext.Line(string(b), 256)
		if name != "" && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(names); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
