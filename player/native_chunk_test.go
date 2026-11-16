package main

import (
	"errors"
	"io"
	"testing"
)

type shortPCMReader struct {
	remaining, position int
	end                 error
}

func (r *shortPCMReader) Read(dst []int32) (int, error) {
	n := min(len(dst), 2304, r.remaining)
	for i := 0; i < n; i++ {
		dst[i] = int32(r.position + i)
	}
	r.position += n
	r.remaining -= n
	if r.remaining == 0 {
		return n, r.end
	}
	return n, nil
}

func TestNativeChunkFillsShortDecoderReads(t *testing.T) {
	for _, ending := range []error{io.EOF, io.ErrUnexpectedEOF} {
		r := &shortPCMReader{remaining: 2*nativeChunkSamples + 100, end: ending}
		buffer := make([]int32, nativeChunkSamples)
		for chunk := 0; chunk < 3; chunk++ {
			n, err := readNativeChunk(r, buffer)
			want := nativeChunkSamples
			if chunk == 2 {
				want = 100
			}
			if n != want || (chunk < 2 && err != nil) || (chunk == 2 && !errors.Is(err, ending)) {
				t.Fatalf("chunk %d: %d %v", chunk, n, err)
			}
			for i, v := range buffer[:n] {
				if v != int32(chunk*nativeChunkSamples+i) {
					t.Fatal("lost or reordered PCM")
				}
			}
		}
	}
}
