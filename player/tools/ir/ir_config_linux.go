//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codemowers/slopify/player/internal/cleantext"
	"github.com/codemowers/slopify/player/internal/linuxioctl"
	"golang.org/x/sys/unix"
)

type irReceiver struct {
	Key          string `json:"key"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Input        string `json:"input"`
	Protocols    string `json:"protocols"`
	LIRC         string `json:"lirc,omitempty"`
	Carrier      bool   `json:"carrier"`
	CarrierRange bool   `json:"carrierRange"`
}

var irNodeName = regexp.MustCompile(`^(rc|event|lirc)[0-9]{1,4}$`)

func irText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return cleantext.Line(string(b), 256)
}
func irReceivers(root string) []irReceiver {
	result := []irReceiver{}
	dirs, _ := filepath.Glob(filepath.Join(root, "rc[0-9]*"))
	for _, dir := range dirs {
		if len(result) >= 32 {
			break
		}
		id := filepath.Base(dir)
		if !irNodeName.MatchString(id) {
			continue
		}
		nodes, _ := filepath.Glob(filepath.Join(dir, "input*", "event*"))
		if len(nodes) != 1 || !irNodeName.MatchString(filepath.Base(nodes[0])) {
			continue
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		// Remove /rc/rcN, whose index changes with probe order. The physical
		// device path (including a GPIO controller or USB port) identifies the receiver.
		physical := filepath.Dir(filepath.Dir(real))
		sum := sha256.Sum256([]byte(physical))
		r := irReceiver{Key: fmt.Sprintf("%x", sum[:8]), ID: id, Name: irText(filepath.Join(filepath.Dir(nodes[0]), "name")), Input: "/dev/input/" + filepath.Base(nodes[0]), Protocols: irText(filepath.Join(dir, "protocols"))}
		lirc, _ := filepath.Glob(filepath.Join(dir, "lirc[0-9]*"))
		if len(lirc) == 1 && irNodeName.MatchString(filepath.Base(lirc[0])) {
			r.LIRC = "/dev/" + filepath.Base(lirc[0])
			fd, err := unix.Open(r.LIRC, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err == nil {
				features, e := unix.IoctlGetUint32(fd, uint(linuxioctl.Request(2, 'i', 0, 4)))
				unix.Close(fd)
				if e == nil {
					r.Carrier = features&0x01000000 != 0
					r.CarrierRange = features&0x80000000 != 0
				}
			}
		}
		result = append(result, r)
	}
	return result
}

var irActions = []struct{ name, key string }{
	{"playpause", "KEY_PLAYPAUSE"}, {"play", "KEY_PLAY"}, {"pause", "KEY_PAUSECD"}, {"previous", "KEY_PREVIOUSSONG"}, {"next", "KEY_NEXTSONG"}, {"mute", "KEY_MUTE"}, {"volume_up", "KEY_VOLUMEUP"}, {"volume_down", "KEY_VOLUMEDOWN"},
}

func irKeymap(protocol string, codes map[string]string) ([]byte, error) {
	bits, toggle := 32, uint64(0)
	decoder, params := protocol, ""
	switch protocol {
	case "rc5", "rc6", "nec":
	case "recs80":
		bits, toggle = 10, 1<<9
		decoder = "pulse_distance"
		params = "header_pulse = 158\nheader_space = 7432\nbit_pulse = 158\nbit_0_space = 4902\nbit_1_space = 7432\ntrailer_pulse = 158\nbits = 10\nmargin = 150\n"
	case "recs80ext":
		bits, toggle = 12, 1<<10
		decoder = "pulse_distance"
		params = "header_pulse = 158\nheader_space = 3637\nbit_pulse = 158\nbit_0_space = 4902\nbit_1_space = 7432\ntrailer_pulse = 158\nbits = 12\nmargin = 150\n"
	case "kaseikyo-denon":
		bits = 48
		decoder = "pulse_distance"
		params = "header_pulse = 3456\nheader_space = 1728\nbit_pulse = 432\nbit_0_space = 432\nbit_1_space = 1296\ntrailer_pulse = 432\nbits = 48\nreverse = 1\nmargin = 150\n"
	default:
		return nil, fmt.Errorf("select NEC, RC5, RC6, RECS80, RECS80EXT or Denon Kaseikyo")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "[[protocols]]\nname = \"slopify\"\nprotocol = %q\n%s[protocols.scancodes]\n", decoder, params)
	used := map[uint64]bool{}
	for _, action := range irActions {
		text := codes[action.name]
		if text == "" {
			continue
		}
		base := 10
		if strings.HasPrefix(text, "0x") {
			base = 16
			text = text[2:]
		}
		value, err := strconv.ParseUint(text, base, bits)
		if err != nil || strings.HasPrefix(text, "+") {
			return nil, fmt.Errorf("invalid %s scancode", action.name)
		}
		// RECS80's toggle distinguishes successive presses, not different keys.
		value &^= toggle
		variants := []uint64{value}
		if toggle != 0 {
			variants = append(variants, value|toggle)
		}
		for _, scan := range variants {
			if used[scan] {
				return nil, fmt.Errorf("a scancode is assigned to more than one action")
			}
			used[scan] = true
			fmt.Fprintf(&b, "0x%x = %q\n", scan, action.key)
		}
	}
	return b.Bytes(), nil
}
func checkIRCarrier(r irReceiver, low, high uint) error {
	if low == 0 && high == 0 {
		return nil
	}
	if low == 0 {
		low = high
	}
	if high == 0 {
		high = low
	}
	if low < 10000 || high > 1000000 || low > high {
		return fmt.Errorf("invalid receive carrier range")
	}
	if !r.Carrier || (low != high && !r.CarrierRange) {
		return fmt.Errorf("this receiver cannot tune that carrier range; demodulating GPIO receivers have a hardware-fixed frequency")
	}
	return nil
}

func irCarrier(r irReceiver, low, high uint) error {
	if err := checkIRCarrier(r, low, high); err != nil {
		return err
	}
	if low == 0 && high == 0 {
		return nil
	}
	if low == 0 {
		low = high
	}
	if high == 0 {
		high = low
	}
	fd, err := unix.Open(r.LIRC, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if r.CarrierRange {
		if err = unix.IoctlSetPointerInt(fd, uint(linuxioctl.Request(1, 'i', 0x1f, 4)), int(low)); err != nil {
			return err
		}
	}
	return unix.IoctlSetPointerInt(fd, uint(linuxioctl.Request(1, 'i', 0x14, 4)), int(high))
}

type irCommandOutput struct{ bytes.Buffer }

func (b *irCommandOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := 4096 - b.Len(); left > 0 {
		b.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}
func irSetup(ctx context.Context, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("ir-setup requires root")
	}
	fl := flag.NewFlagSet("ir-setup", flag.ContinueOnError)
	key := fl.String("receiver", "", "physical receiver key from ir-info")
	protocol := fl.String("protocol", "rc5", "NEC/RC5/RC6, RECS80/RECS80EXT or Denon Kaseikyo decoder")
	low := fl.Uint("carrier-min-khz", 0, "optional tunable receiver carrier lower bound")
	high := fl.Uint("carrier-max-khz", 0, "optional tunable receiver carrier upper bound")
	codes := map[string]*string{}
	for _, action := range irActions {
		codes[action.name] = fl.String(action.name, "", "remote scancode, decimal or 0x hex")
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	values := map[string]string{}
	for name, p := range codes {
		values[name] = *p
	}
	data, err := irKeymap(*protocol, values)
	if err != nil {
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
	if receiver.ID == "" {
		return fmt.Errorf("configured IR receiver is unavailable")
	}
	if *low > 1000 || *high > 1000 {
		return fmt.Errorf("carrier must be 10–1000 kHz")
	}
	if err = checkIRCarrier(receiver, *low*1000, *high*1000); err != nil {
		return err
	}
	// Unique private temporary files avoid shared filenames and symlink races.
	f, err := os.CreateTemp("/var/run", "slopify-ir-*.toml")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/ir-keytable", "-s", receiver.ID, "-c", "-w", path)
	var output irCommandOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("loading kernel keymap: %w: %s", err, cleantext.Line(output.String(), 512))
	}
	// Protocol selection can reconfigure a hardware receiver. Apply the
	// requested carrier filter last so it is not replaced by protocol defaults.
	if err = irCarrier(receiver, *low*1000, *high*1000); err != nil {
		return err
	}
	fmt.Println(receiver.Input)
	return nil
}
func irCommand(ctx context.Context, args []string) int {
	var err error
	switch args[0] {
	case "ir-info":
		if len(args) != 1 {
			err = fmt.Errorf("ir-info takes no arguments")
		} else {
			err = json.NewEncoder(os.Stdout).Encode(irReceivers("/sys/class/rc"))
		}
	case "ir-learn":
		err = irLearn(ctx, args[1:])
	case "ir-setup":
		err = irSetup(ctx, args[1:])
	default:
		err = fmt.Errorf("unknown receiver setup command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
