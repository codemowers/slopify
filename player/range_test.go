package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRangeSource(url string) *rangeSource {
	return &rangeSource{url: url, client: &http.Client{Timeout: 20 * time.Second}, blocks: make(map[int64][]byte)}
}

func TestSourceReaderBoundedRangesAndConnectionReuse(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefg"), sourceBlockSize)
	var connections, requests atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"source-v1"`)
		if !strings.HasPrefix(r.Header.Get("Range"), "bytes=") {
			t.Error("unbounded source request")
		}
		http.ServeContent(w, r, "track.flac", time.Time{}, bytes.NewReader(data))
	}))
	upstream.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()
	source := newTestRangeSource(upstream.URL)
	if _, err := source.block(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	reader := &sourceReader{source: source, ctx: context.Background()}
	for _, offset := range []int64{200000, 400000, 600000, 10000} {
		if _, err := reader.Seek(offset, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4096)
		if _, err := io.ReadFull(reader, b); err != nil || !bytes.Equal(b, data[offset:offset+4096]) {
			t.Fatalf("range mismatch at %d: %v", offset, err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("did not reuse upstream connection: %d connections for %d reads", connections.Load(), requests.Load())
	}
	if _, err := reader.Seek(999999999, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 16)); err != io.EOF {
		t.Fatal("read past the end:", err)
	}
	if requests.Load() > 6 {
		t.Fatal("range reader fetched unnecessary blocks", requests.Load())
	}
}

func TestRangeSourceRejectsInvalidOrTruncatedRange(t *testing.T) {
	for _, body := range []string{"bad range", "truncated"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-99/100")
			if body == "bad range" {
				w.Header().Set("Content-Range", "invalid")
			}
			w.WriteHeader(206)
			w.Write([]byte(body))
		}))
		_, err := newTestRangeSource(srv.URL).block(context.Background(), 0)
		srv.Close()
		if err == nil {
			t.Fatal("invalid range accepted", body)
		}
	}
}
