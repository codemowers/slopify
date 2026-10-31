//go:build linux

// Package alsa drives kernel PCM devices with ioctls, without CGo or alsa-lib.
// Only hw devices are used, so the kernel performs no implicit conversion.
package alsa

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Sample formats, as numbered by the kernel.
const (
	FormatS16LE  = 2
	FormatS24LE  = 6
	FormatS32LE  = 10
	FormatS243LE = 32
)

const (
	accessRWInterleaved = 3
	paramAccess         = 0
	paramFormat         = 1
	paramChannels       = 10
	paramRate           = 11
	paramPeriodTime     = 12
	paramPeriodSize     = 13
	paramBufferTime     = 16
	paramBufferSize     = 17
	firstInterval       = 8

	bufferMinMicros = 200_000
	bufferMaxMicros = 500_000
	periodMinMicros = 20_000
	periodMaxMicros = 100_000
)

type mask struct{ bits [8]uint32 }

type interval struct {
	min, max uint32
	flags    uint32
}

// These mirror the UAPI structures; uintptr matches the kernel's unsigned long
// on both 32-bit and 64-bit ARM.
type hwParams struct {
	flags     uint32
	masks     [3]mask
	mres      [5]mask
	intervals [12]interval
	ires      [9]interval
	rmask     uint32
	cmask     uint32
	info      uint32
	msbits    uint32
	rateNum   uint32
	rateDen   uint32
	fifoSize  uintptr
	reserved  [64]byte
}

type swParams struct {
	tstampMode       int32
	periodStep       uint32
	sleepMin         uint32
	availMin         uintptr
	xferAlign        uintptr
	startThreshold   uintptr
	stopThreshold    uintptr
	silenceThreshold uintptr
	silenceSize      uintptr
	boundary         uintptr
	proto            uint32
	tstampType       uint32
	reserved         [56]byte
}

type xferi struct {
	result int
	buf    unsafe.Pointer
	frames uintptr
}

func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'A'<<8 | nr }

var (
	iocHwRefine = ioc(3, 0x10, unsafe.Sizeof(hwParams{}))
	iocHwParams = ioc(3, 0x11, unsafe.Sizeof(hwParams{}))
	iocSwParams = ioc(3, 0x13, unsafe.Sizeof(swParams{}))
	iocDelay    = ioc(2, 0x21, unsafe.Sizeof(int(0)))
	iocPrepare  = ioc(0, 0x40, 0)
	iocResume   = ioc(0, 0x47, 0)
	iocDrop     = ioc(0, 0x43, 0)
	iocDrain    = ioc(0, 0x44, 0)
	iocWriteI   = ioc(1, 0x50, unsafe.Sizeof(xferi{}))
	iocReadI    = ioc(2, 0x51, unsafe.Sizeof(xferi{}))
)

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
		switch e {
		case 0:
			return nil
		case unix.EINTR:
		default:
			return e
		}
	}
}

func anyParams() *hwParams {
	p := &hwParams{rmask: ^uint32(0)}
	for i := range p.masks {
		p.masks[i].bits = [8]uint32{^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0)}
	}
	for i := range p.intervals {
		p.intervals[i] = interval{max: ^uint32(0)}
	}
	return p
}

func (p *hwParams) setMask(param, value int) {
	p.masks[param].bits = [8]uint32{}
	p.masks[param].bits[value/32] = 1 << (value % 32)
}

func (p *hwParams) setInterval(param int, lo, hi uint32) {
	p.intervals[param-firstInterval] = interval{min: lo, max: hi}
}

func (p *hwParams) get(param int) uint32 { return p.intervals[param-firstInterval].min }

// ParseDevice resolves hw:N,D, hw:ID,D[,S] and hw:CARD=ID,DEV=D to numbers.
// Device names an output for display.
type Device struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Describe gives a playback device's PCM name and /dev node.
func Describe(spec string) (Device, error) {
	card, device, err := ParseDevice(spec)
	if err != nil {
		return Device{}, err
	}
	d := Device{Path: fmt.Sprintf("/dev/snd/pcmC%dD%dp", card, device)}
	info, err := os.ReadFile(fmt.Sprintf("/proc/asound/card%d/pcm%dp/info", card, device))
	if err != nil {
		return d, nil
	}
	for _, line := range strings.Split(string(info), "\n") {
		if name, ok := strings.CutPrefix(line, "name: "); ok {
			d.Name = name
		}
	}
	return d, nil
}

func ParseDevice(spec string) (card, device int, err error) {
	rest, ok := strings.CutPrefix(spec, "hw:")
	if !ok {
		return 0, 0, errors.New("ALSA output requires a hw: device")
	}
	name, dev := rest, "0"
	if parts := strings.Split(rest, ","); len(parts) > 1 {
		name, dev = parts[0], parts[1]
	}
	name, dev = strings.TrimPrefix(name, "CARD="), strings.TrimPrefix(dev, "DEV=")
	if device, err = strconv.Atoi(dev); err != nil || device < 0 {
		return 0, 0, fmt.Errorf("invalid ALSA device %q", spec)
	}
	if card, err = strconv.Atoi(name); err == nil {
		return card, device, nil
	}
	target, err := os.ReadFile("/proc/asound/" + name + "/id")
	if err != nil || strings.TrimSpace(string(target)) != name {
		return 0, 0, fmt.Errorf("unknown ALSA card %q", name)
	}
	link, err := os.Readlink("/proc/asound/" + name)
	if err != nil {
		return 0, 0, fmt.Errorf("unknown ALSA card %q", name)
	}
	if card, err = strconv.Atoi(strings.TrimPrefix(link, "card")); err != nil {
		return 0, 0, fmt.Errorf("unknown ALSA card %q", name)
	}
	return card, device, nil
}

// Config is an opened device's negotiated shape.
type Config struct {
	Format, Rate, Channels, PeriodFrames, BufferFrames int
}

// PCM is one open playback or capture substream.
type PCM struct {
	fd       int
	config   Config
	capture  bool
	started  bool
	frameLen int
}

// Open negotiates exactly the requested format. It does not fall back: an
// unsupported sample rate is an error, never a silent conversion.
func Open(spec string, capture bool, format, rate, channels int) (*PCM, error) {
	return OpenBuffered(spec, capture, format, rate, channels, bufferMinMicros, bufferMaxMicros, periodMinMicros, periodMaxMicros)
}

// OpenLowLatency trades underrun margin for delay, for live LAN sources whose
// picture may be waiting on the sound.
func OpenLowLatency(spec string, format, rate, channels int) (*PCM, error) {
	return OpenBuffered(spec, false, format, rate, channels, 150_000, 250_000, 20_000, 50_000)
}

// OpenBuffered is Open with an explicit preferred buffer and period range.
func OpenBuffered(spec string, capture bool, format, rate, channels int, bufferMin, bufferMax, periodMin, periodMax uint32) (*PCM, error) {
	card, device, err := ParseDevice(spec)
	if err != nil {
		return nil, err
	}
	direction := "p"
	if capture {
		direction = "c"
	}
	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%s", card, device, direction)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	pcm := &PCM{fd: fd, capture: capture}
	if err := pcm.configure(format, rate, channels, bufferMin, bufferMax, periodMin, periodMax); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return pcm, nil
}

func (c *PCM) configure(format, rate, channels int, bufferMin, bufferMax, periodMin, periodMax uint32) error {
	p := anyParams()
	p.setMask(paramAccess, accessRWInterleaved)
	p.setMask(paramFormat, format)
	p.setInterval(paramChannels, uint32(channels), uint32(channels))
	p.setInterval(paramRate, uint32(rate), uint32(rate))
	if err := ioctl(c.fd, iocHwRefine, unsafe.Pointer(p)); err != nil {
		return fmt.Errorf("device does not accept format %d, %d Hz, %d channels: %w", format, rate, channels, err)
	}
	// Prefer a buffer that rides out scheduling stalls; devices may not offer it.
	tuned := *p
	tuned.setInterval(paramBufferTime, bufferMin, bufferMax)
	tuned.setInterval(paramPeriodTime, periodMin, periodMax)
	tuned.rmask = ^uint32(0)
	if ioctl(c.fd, iocHwRefine, unsafe.Pointer(&tuned)) == nil {
		*p = tuned
	}
	p.rmask = ^uint32(0)
	if err := ioctl(c.fd, iocHwParams, unsafe.Pointer(p)); err != nil {
		return fmt.Errorf("configuring device: %w", err)
	}
	c.config = Config{Format: format, Rate: rate, Channels: channels,
		PeriodFrames: int(p.get(paramPeriodSize)), BufferFrames: int(p.get(paramBufferSize))}
	if c.config.PeriodFrames <= 0 || c.config.BufferFrames <= 0 {
		return errors.New("device reported an empty buffer")
	}
	c.frameLen = channels * bytesPerSample(format)
	sw := swParams{availMin: uintptr(c.config.PeriodFrames), stopThreshold: uintptr(c.config.BufferFrames),
		startThreshold: uintptr(c.config.BufferFrames - c.config.PeriodFrames), periodStep: 1}
	if capture := c.capture; capture {
		sw.startThreshold = 1
	}
	if err := ioctl(c.fd, iocSwParams, unsafe.Pointer(&sw)); err != nil {
		return fmt.Errorf("configuring start threshold: %w", err)
	}
	if err := ioctl(c.fd, iocPrepare, nil); err != nil {
		return fmt.Errorf("preparing device: %w", err)
	}
	return nil
}

func bytesPerSample(format int) int {
	switch format {
	case FormatS16LE:
		return 2
	case FormatS243LE:
		return 3
	}
	return 4
}

func (c *PCM) Config() Config { return c.config }

// WriteFrames blocks at the playback rate. Underruns are recovered and reported
// through the returned count so that callers can log them; audio is not altered.
func (c *PCM) WriteFrames(data []byte) (underruns int, err error) {
	if len(data)%c.frameLen != 0 {
		return 0, errors.New("partial frame")
	}
	for len(data) > 0 {
		x := xferi{buf: unsafe.Pointer(&data[0]), frames: uintptr(len(data) / c.frameLen)}
		switch err := ioctl(c.fd, iocWriteI, unsafe.Pointer(&x)); err {
		case nil:
			data = data[x.result*c.frameLen:]
		case unix.EPIPE:
			underruns++
			if err := ioctl(c.fd, iocPrepare, nil); err != nil {
				return underruns, fmt.Errorf("recovering from underrun: %w", err)
			}
		case unix.ESTRPIPE:
			for ioctl(c.fd, iocResume, nil) == unix.EAGAIN {
				unix.Nanosleep(&unix.Timespec{Nsec: 100_000_000}, nil)
			}
			if err := ioctl(c.fd, iocPrepare, nil); err != nil {
				return underruns, fmt.Errorf("recovering from suspend: %w", err)
			}
		default:
			return underruns, fmt.Errorf("writing PCM: %w", err)
		}
	}
	return underruns, nil
}

// ReadFrames fills data with captured frames; it exists for loopback tests.
func (c *PCM) ReadFrames(data []byte) (int, error) {
	x := xferi{buf: unsafe.Pointer(&data[0]), frames: uintptr(len(data) / c.frameLen)}
	if err := ioctl(c.fd, iocReadI, unsafe.Pointer(&x)); err != nil {
		return 0, fmt.Errorf("reading PCM: %w", err)
	}
	return x.result, nil
}

// Delay returns the frames written but not yet heard; zero if not running.
func (c *PCM) Delay() int {
	var frames int
	if ioctl(c.fd, iocDelay, unsafe.Pointer(&frames)) != nil {
		return 0
	}
	return frames
}

// Drain plays out what has been written. Drop abandons it.
func (c *PCM) Drain() error { return ioctl(c.fd, iocDrain, nil) }
func (c *PCM) Drop() error  { return ioctl(c.fd, iocDrop, nil) }
func (c *PCM) Close() error { return unix.Close(c.fd) }
