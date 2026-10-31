package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const sourceBlockSize = 128 << 10
const sourceCacheBlocks = 4

// rangeSource serves a remote original through bounded range requests.
// Upstream reads are consumed completely, letting Go reuse HTTPS connections
// during FLAC's binary seek. At most 512 KiB is cached per seek.
type rangeSource struct {
	mu                sync.Mutex
	url               string
	client            *http.Client
	size              int64
	etag, contentType string
	blocks            map[int64][]byte
	order             []int64
}

func (s *rangeSource) block(ctx context.Context, offset int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.blocks[offset]; ok {
		return b, nil
	}
	end := offset + sourceBlockSize - 1
	if s.size > 0 {
		end = min(end, s.size-1)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", s.url, nil)
	if err != nil {
		return nil, errors.New("invalid source URL")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	req.Header.Set("Accept-Encoding", "identity")
	if s.etag != "" {
		req.Header.Set("If-Match", s.etag)
	}
	fetchAt := time.Now()
	defer func() { metricRangeFetch.Observe(time.Since(fetchAt).Seconds()) }()
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("reading source range failed")
	}
	defer resp.Body.Close()
	var first, last, size int64
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("source range: HTTP %d", resp.StatusCode)
	}
	n, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &size)
	if err != nil || n != 3 || first != offset || size <= 0 || last != min(end, size-1) || last < first || s.size > 0 && size != s.size {
		return nil, errors.New("invalid source content range")
	}
	if s.size > 0 && s.etag != resp.Header.Get("ETag") {
		return nil, errors.New("source changed during seek")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, sourceBlockSize+1))
	if err != nil || int64(len(b)) != last-first+1 {
		return nil, errors.New("truncated source range")
	}
	if s.size == 0 {
		s.size, s.etag, s.contentType = size, resp.Header.Get("ETag"), resp.Header.Get("Content-Type")
	}
	if len(s.order) == sourceCacheBlocks {
		delete(s.blocks, s.order[0])
		s.order = s.order[1:]
	}
	s.order = append(s.order, offset)
	s.blocks[offset] = b
	return b, nil
}

type sourceReader struct {
	source *rangeSource
	ctx    context.Context
	offset int64
}

func (r *sourceReader) Read(b []byte) (int, error) {
	if r.offset >= r.source.size {
		return 0, io.EOF
	}
	start := r.offset / sourceBlockSize * sourceBlockSize
	data, err := r.source.block(r.ctx, start)
	if err != nil {
		return 0, err
	}
	n := copy(b, data[r.offset-start:])
	r.offset += int64(n)
	return n, nil
}

func (r *sourceReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.offset
	case io.SeekEnd:
		offset += r.source.size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if offset < 0 {
		return 0, errors.New("negative source offset")
	}
	r.offset = offset
	return offset, nil
}
