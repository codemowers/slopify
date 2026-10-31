package pulse

import (
	"encoding/binary"
	"errors"
)

const (
	tagString      = 't'
	tagStringNull  = 'N'
	tagU32         = 'L'
	tagU8          = 'B'
	tagU64         = 'R'
	tagS64         = 'r'
	tagSampleSpec  = 'a'
	tagArbitrary   = 'x'
	tagTrue        = '1'
	tagFalse       = '0'
	tagTimeval     = 'T'
	tagUsec        = 'U'
	tagChannelMap  = 'm'
	tagCVolume     = 'v'
	tagProplist    = 'P'
	tagVolume      = 'V'
	tagFormatInfo  = 'f'
	maxChannels    = 32
	maxPropertyLen = 64 << 10
	maxProperties  = 256
)

var errMalformed = errors.New("malformed pulse message")

// reader decodes one control message. Errors stick, so that handlers can read a
// run of fields and check once.
type reader struct {
	b   []byte
	err error
}

func (r *reader) fail() { r.err = errMalformed; r.b = nil }

func (r *reader) bytes(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.fail()
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) tag(want byte) bool {
	v := r.bytes(1)
	if r.err != nil {
		return false
	}
	if v[0] != want {
		r.fail()
		return false
	}
	return true
}

func (r *reader) u32() uint32 {
	if !r.tag(tagU32) {
		return 0
	}
	v := r.bytes(4)
	if r.err != nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (r *reader) u8() uint8 {
	if !r.tag(tagU8) {
		return 0
	}
	v := r.bytes(1)
	if r.err != nil {
		return 0
	}
	return v[0]
}

func (r *reader) boolean() bool {
	v := r.bytes(1)
	if r.err != nil {
		return false
	}
	switch v[0] {
	case tagTrue:
		return true
	case tagFalse:
		return false
	}
	r.fail()
	return false
}

// str returns a string; a null string is empty.
func (r *reader) str() string {
	v := r.bytes(1)
	if r.err != nil {
		return ""
	}
	switch v[0] {
	case tagStringNull:
		return ""
	case tagString:
	default:
		r.fail()
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.fail()
	return ""
}

func (r *reader) arbitrary() []byte {
	if !r.tag(tagArbitrary) {
		return nil
	}
	v := r.bytes(4)
	if r.err != nil {
		return nil
	}
	return r.bytes(int(binary.BigEndian.Uint32(v)))
}

func (r *reader) usec() uint64 {
	if !r.tag(tagUsec) {
		return 0
	}
	v := r.bytes(8)
	if r.err != nil {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// timeval returns seconds and microseconds, echoed back in latency replies.
func (r *reader) timeval() (uint32, uint32) {
	if !r.tag(tagTimeval) {
		return 0, 0
	}
	v := r.bytes(8)
	if r.err != nil {
		return 0, 0
	}
	return binary.BigEndian.Uint32(v), binary.BigEndian.Uint32(v[4:])
}

func (r *reader) sampleSpec() (format, channels uint8, rate uint32) {
	if !r.tag(tagSampleSpec) {
		return 0, 0, 0
	}
	v := r.bytes(6)
	if r.err != nil {
		return 0, 0, 0
	}
	return v[0], v[1], binary.BigEndian.Uint32(v[2:])
}

func (r *reader) channelMap() []byte {
	if !r.tag(tagChannelMap) {
		return nil
	}
	n := r.bytes(1)
	if r.err != nil {
		return nil
	}
	if n[0] > maxChannels {
		r.fail()
		return nil
	}
	return append([]byte(nil), r.bytes(int(n[0]))...)
}

func (r *reader) cvolume() []uint32 {
	if !r.tag(tagCVolume) {
		return nil
	}
	n := r.bytes(1)
	if r.err != nil {
		return nil
	}
	if n[0] > maxChannels {
		r.fail()
		return nil
	}
	values := make([]uint32, n[0])
	for i := range values {
		v := r.bytes(4)
		if r.err != nil {
			return nil
		}
		values[i] = binary.BigEndian.Uint32(v)
	}
	return values
}

// proplistBody reads properties after their introducing tag.
func (r *reader) proplistBody() map[string]string {
	props := map[string]string{}
	for count := 0; ; count++ {
		key := r.str()
		if r.err != nil || key == "" {
			return props
		}
		length := r.u32()
		data := r.arbitrary()
		if r.err != nil || length != uint32(len(data)) || length > maxPropertyLen || count >= maxProperties {
			r.fail()
			return props
		}
		// Text properties end in NUL; binary ones are not useful here.
		if n := len(data); n > 0 && data[n-1] == 0 {
			props[key] = string(data[:n-1])
		}
	}
}

func (r *reader) proplist() map[string]string {
	if !r.tag(tagProplist) {
		return nil
	}
	return r.proplistBody()
}

// formatInfo skips a format and returns its encoding.
func (r *reader) formatInfo() uint8 {
	if !r.tag(tagFormatInfo) {
		return 0
	}
	encoding := r.u8()
	r.proplist()
	return encoding
}

// writer encodes one control message.
type writer struct{ b []byte }

func (w *writer) u32(v uint32) *writer {
	w.b = binary.BigEndian.AppendUint32(append(w.b, tagU32), v)
	return w
}

func (w *writer) u8(v uint8) *writer {
	w.b = append(w.b, tagU8, v)
	return w
}

func (w *writer) s64(v int64) *writer {
	w.b = binary.BigEndian.AppendUint64(append(w.b, tagS64), uint64(v))
	return w
}

func (w *writer) u64(v uint64) *writer {
	w.b = binary.BigEndian.AppendUint64(append(w.b, tagU64), v)
	return w
}

func (w *writer) usec(v uint64) *writer {
	w.b = binary.BigEndian.AppendUint64(append(w.b, tagUsec), v)
	return w
}

func (w *writer) boolean(v bool) *writer {
	if v {
		w.b = append(w.b, tagTrue)
	} else {
		w.b = append(w.b, tagFalse)
	}
	return w
}

func (w *writer) str(v string) *writer {
	w.b = append(append(append(w.b, tagString), v...), 0)
	return w
}

func (w *writer) null() *writer {
	w.b = append(w.b, tagStringNull)
	return w
}

func (w *writer) timeval(sec, usec uint32) *writer {
	w.b = append(w.b, tagTimeval)
	w.b = binary.BigEndian.AppendUint32(w.b, sec)
	w.b = binary.BigEndian.AppendUint32(w.b, usec)
	return w
}

func (w *writer) sampleSpec(format, channels uint8, rate uint32) *writer {
	w.b = append(w.b, tagSampleSpec, format, channels)
	w.b = binary.BigEndian.AppendUint32(w.b, rate)
	return w
}

func (w *writer) channelMap(m []byte) *writer {
	w.b = append(append(w.b, tagChannelMap, byte(len(m))), m...)
	return w
}

func (w *writer) cvolume(channels int, v uint32) *writer {
	w.b = append(w.b, tagCVolume, byte(channels))
	for range channels {
		w.b = binary.BigEndian.AppendUint32(w.b, v)
	}
	return w
}

func (w *writer) volume(v uint32) *writer {
	w.b = binary.BigEndian.AppendUint32(append(w.b, tagVolume), v)
	return w
}

// proplist writes properties; values gain the NUL that Pulse text values carry.
func (w *writer) proplist(props map[string]string) *writer {
	w.b = append(w.b, tagProplist)
	for k, v := range props {
		w.str(k)
		w.u32(uint32(len(v) + 1))
		w.b = append(w.b, tagArbitrary)
		w.b = binary.BigEndian.AppendUint32(w.b, uint32(len(v)+1))
		w.b = append(append(w.b, v...), 0)
	}
	return w.null()
}

func (w *writer) pcmFormatInfo() *writer {
	w.b = append(w.b, tagFormatInfo)
	w.u8(encodingPCM)
	return w.proplist(nil)
}
