//go:build linux

package alsa

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ifaceMixer     = 2
	elemInteger    = 2
	elemBoolean    = 1
	accessRead     = 1 << 0
	accessWrite    = 1 << 1
	accessTLVRead  = 1 << 4
	maxListedElems = 256
	playbackVolume = " Playback Volume"
	tlvContainer   = 0
	tlvDBScale     = 1
	tlvDBMute      = 0x10000
	maxTLVBytes    = 4096
	maxControlVals = 128
	wordSize       = unsafe.Sizeof(uintptr(0))
)

type elemID struct {
	numid     uint32
	iface     int32
	device    uint32
	subdevice uint32
	name      [44]byte
	index     uint32
}

type elemInfo struct {
	id       elemID
	kind     int32
	access   uint32
	count    uint32
	owner    int32
	value    [128]byte
	reserved [64]byte
}

// The kernel aligns its value union to eight bytes on every architecture.
type elemValue struct {
	id       elemID
	indirect uint32
	_        [4]byte
	value    [maxControlVals * wordSize]byte
	reserved [128]byte
}

type elemList struct {
	offset, space, used, count uint32
	ids                        *elemID
	reserved                   [50]byte
}

func iocControl(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'U'<<8 | nr }

var (
	iocElemList  = iocControl(3, 0x10, unsafe.Sizeof(elemList{}))
	iocElemInfo  = iocControl(3, 0x11, unsafe.Sizeof(elemInfo{}))
	iocElemRead  = iocControl(3, 0x12, unsafe.Sizeof(elemValue{}))
	iocElemWrite = iocControl(3, 0x13, unsafe.Sizeof(elemValue{}))
	iocTLVRead   = iocControl(3, 0x1a, 8)
)

// Scale is a control's dB range, in hundredths of a dB.
type Scale struct {
	Min, Step int
	Mute      bool
}

// IntegerControl describes a mixer control's range and channel count.
type IntegerControl struct {
	Min, Max, Channels int
	Scale              *Scale
}

// NamedControl is a writable playback volume, named as -volume-control takes it.
type NamedControl struct {
	Name     string `json:"name"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Channels int    `json:"channels"`
	DB       bool   `json:"db"`
}

// Card is a sound card with its volume candidates.
type Card struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Controls []NamedControl `json:"controls"`
}

// Mixer reads and writes named controls on a card. Each call opens the device,
// so a USB DAC can come and go without stale descriptors.
type Mixer struct{ card int }

// OpenMixer resolves a card number or ID such as sndrpihifiberry.
func OpenMixer(card string) (*Mixer, error) {
	number, _, err := ParseDevice("hw:" + card + ",0")
	if err != nil {
		return nil, err
	}
	m := &Mixer{card: number}
	fd, err := m.open()
	if err != nil {
		return nil, err
	}
	unix.Close(fd)
	return m, nil
}

func (m *Mixer) open() (int, error) {
	path := fmt.Sprintf("/dev/snd/controlC%d", m.card)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", path, err)
	}
	return fd, nil
}

func namedElem(name string) (elemID, error) {
	id := elemID{iface: ifaceMixer}
	if name == "" || len(name) >= len(id.name) {
		return id, errors.New("invalid ALSA control name")
	}
	copy(id.name[:], name)
	return id, nil
}

func (m *Mixer) info(fd int, name string) (*elemInfo, error) {
	id, err := namedElem(name)
	if err != nil {
		return nil, err
	}
	info := &elemInfo{id: id}
	if err := ioctl(fd, iocElemInfo, unsafe.Pointer(info)); err != nil {
		return nil, fmt.Errorf("ALSA control %q: %w", name, err)
	}
	if info.count == 0 || info.count > maxControlVals {
		return nil, fmt.Errorf("ALSA control %q has %d values", name, info.count)
	}
	return info, nil
}

func word(b []byte) int {
	if wordSize == 8 {
		return int(int64(binary.NativeEndian.Uint64(b)))
	}
	return int(int32(binary.NativeEndian.Uint32(b)))
}

func putWord(b []byte, v int) {
	if wordSize == 8 {
		binary.NativeEndian.PutUint64(b, uint64(int64(v)))
	} else {
		binary.NativeEndian.PutUint32(b, uint32(int32(v)))
	}
}

// Integer describes an integer control, including any dB metadata.
func (m *Mixer) Integer(name string) (IntegerControl, error) {
	fd, err := m.open()
	if err != nil {
		return IntegerControl{}, err
	}
	defer unix.Close(fd)
	info, err := m.info(fd, name)
	if err != nil {
		return IntegerControl{}, err
	}
	if info.kind != elemInteger {
		return IntegerControl{}, fmt.Errorf("ALSA control %q is not an integer", name)
	}
	c := IntegerControl{Min: word(info.value[0:]), Max: word(info.value[wordSize:]), Channels: int(info.count)}
	if info.access&accessTLVRead != 0 {
		c.Scale = readScale(fd, info.id.numid)
	}
	return c, nil
}

// Cards lists sound cards and the playback volume controls of each.
func Cards() ([]Card, error) {
	dirs, err := filepath.Glob("/proc/asound/card[0-9]*")
	if err != nil {
		return nil, err
	}
	cards := []Card{}
	for _, dir := range dirs {
		number, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "card"))
		if err != nil {
			continue
		}
		id, err := os.ReadFile(filepath.Join(dir, "id"))
		if err != nil {
			return nil, err
		}
		controls, err := (&Mixer{card: number}).controls()
		if err != nil {
			return nil, err
		}
		name := ""
		if d, err := Describe(fmt.Sprintf("hw:%d,0", number)); err == nil {
			name = d.Name
		}
		cards = append(cards, Card{ID: strings.TrimSpace(string(id)), Name: name, Controls: controls})
	}
	return cards, nil
}

func (m *Mixer) controls() ([]NamedControl, error) {
	fd, err := m.open()
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	ids := make([]elemID, maxListedElems)
	list := elemList{space: uint32(len(ids)), ids: &ids[0]}
	if err := ioctl(fd, iocElemList, unsafe.Pointer(&list)); err != nil {
		return nil, fmt.Errorf("listing ALSA controls: %w", err)
	}
	controls := []NamedControl{}
	for _, id := range ids[:list.used] {
		name, _, _ := strings.Cut(string(id.name[:]), "\x00")
		short, isVolume := strings.CutSuffix(name, playbackVolume)
		if id.iface != ifaceMixer || id.index != 0 || !isVolume {
			continue
		}
		info, err := m.info(fd, name)
		if err != nil || info.kind != elemInteger || info.access&(accessRead|accessWrite) != accessRead|accessWrite {
			continue
		}
		c := IntegerControl{Min: word(info.value[0:]), Max: word(info.value[wordSize:]), Channels: int(info.count)}
		if info.access&accessTLVRead != 0 {
			c.Scale = readScale(fd, info.id.numid)
		}
		controls = append(controls, NamedControl{Name: short, Min: c.Min, Max: c.Max, Channels: c.Channels, DB: c.Scale != nil})
	}
	sort.Slice(controls, func(i, j int) bool { return controls[i].Name < controls[j].Name })
	return controls, nil
}

// readScale finds a linear dB scale; other TLV shapes report no dB metadata.
func readScale(fd int, numid uint32) *Scale {
	buf := make([]uint32, maxTLVBytes/4)
	buf[0], buf[1] = numid, maxTLVBytes-8
	if ioctl(fd, iocTLVRead, unsafe.Pointer(&buf[0])) != nil {
		return nil
	}
	tlv := buf[2:]
	for len(tlv) >= 2 {
		kind, length := tlv[0], tlv[1]/4
		if int(length)+2 > len(tlv) {
			return nil
		}
		switch kind {
		case tlvContainer:
			tlv = tlv[2:]
			continue
		case tlvDBScale:
			if length < 2 {
				return nil
			}
			return &Scale{Min: int(int32(tlv[2])), Step: int(tlv[3] & 0xffff), Mute: tlv[3]&tlvDBMute != 0}
		}
		tlv = tlv[2+length:]
	}
	return nil
}

func (m *Mixer) read(name string, kind int32) ([]int, error) {
	fd, err := m.open()
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	info, err := m.info(fd, name)
	if err != nil {
		return nil, err
	}
	if info.kind != kind {
		return nil, fmt.Errorf("ALSA control %q has type %d", name, info.kind)
	}
	value := &elemValue{id: info.id}
	if err := ioctl(fd, iocElemRead, unsafe.Pointer(value)); err != nil {
		return nil, fmt.Errorf("reading ALSA control %q: %w", name, err)
	}
	result := make([]int, info.count)
	for i := range result {
		result[i] = word(value.value[uintptr(i)*wordSize:])
	}
	return result, nil
}

// write sets every channel of a control to the same value.
func (m *Mixer) write(name string, kind int32, v int) error {
	fd, err := m.open()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	info, err := m.info(fd, name)
	if err != nil {
		return err
	}
	if info.kind != kind {
		return fmt.Errorf("ALSA control %q has type %d", name, info.kind)
	}
	value := &elemValue{id: info.id}
	for i := uintptr(0); i < uintptr(info.count); i++ {
		putWord(value.value[i*wordSize:], v)
	}
	if err := ioctl(fd, iocElemWrite, unsafe.Pointer(value)); err != nil {
		return fmt.Errorf("writing ALSA control %q: %w", name, err)
	}
	return nil
}

func (m *Mixer) Values(name string) ([]int, error)   { return m.read(name, elemInteger) }
func (m *Mixer) SetValue(name string, v int) error   { return m.write(name, elemInteger, v) }
func (m *Mixer) Switches(name string) ([]int, error) { return m.read(name, elemBoolean) }
func (m *Mixer) SetSwitch(name string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	return m.write(name, elemBoolean, v)
}
