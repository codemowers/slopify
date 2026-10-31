package audiometer

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"
)

func TestPeakEnvelope(t *testing.T) {
	m := New()
	data := make([]byte, 20*2*4)
	binary.LittleEndian.PutUint32(data[4:], math.Float32bits(.5))
	if err := m.ReadPeaks(bytes.NewReader(data), 100, 2); err != io.EOF {
		t.Fatal(err)
	}
	w := m.Snapshot()
	if w.Sequence != 1 || w.Peak != .5 || w.DBFS == nil || math.Abs(*w.DBFS+6.020599913) > 1e-6 {
		t.Fatalf("wrong envelope: %+v", w)
	}
	if err := m.ReadPeaks(bytes.NewReader(make([]byte, len(data))), 100, 2); err != io.EOF {
		t.Fatal(err)
	}
	if m.Snapshot().DBFS != nil || m.Snapshot().Peak != 0 {
		t.Fatal("silence must be represented by zero amplitude")
	}
}
