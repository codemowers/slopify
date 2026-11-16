package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const sourceBlockSize = 128 << 10

// sourceCacheBlocks are kept behind the reader (or in total, without read-ahead)
// for decoders that seek backwards, such as FLAC's binary search.
const sourceCacheBlocks = 4

// readAheadRequestBlocks bounds one background request to 1 MiB.
const readAheadRequestBlocks = 8

// sourceError is a failed request for an original; its reason is a metric label.
type sourceError struct{ reason, msg string }

func (e *sourceError) Error() string { return e.msg }

func sourceFailure(reason, msg string) error { return &sourceError{reason, msg} }

// errorReason is the bounded metric reason of err, or fallback.
func errorReason(err error, fallback string) string {
	var s *sourceError
	if errors.As(err, &s) {
		return s.reason
	}
	return fallback
}

// readAheadBudget bounds downloaded audio held across every source of a backend.
type readAheadBudget struct {
	mu          sync.Mutex
	used, limit int64
	changed     chan struct{} // closed and replaced whenever bytes are released
}

func newReadAheadBudget(limit int64) *readAheadBudget {
	if metricsEnabled {
		metricBufferLimitBytes.WithLabelValues(audio(stageDownload, "slopify")...).Set(float64(limit))
	}
	return &readAheadBudget{limit: limit, changed: make(chan struct{})}
}

// tryTake reserves n bytes, or returns a channel closed when some are released.
func (b *readAheadBudget) tryTake(n int64) (<-chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit && b.used > 0 {
		return b.changed, false
	}
	b.used += n
	if metricsEnabled {
		metricBufferBytes.WithLabelValues(audio(stageDownload, "slopify")...).Set(float64(b.used))
	}
	return nil, true
}

// take reserves n bytes even past the limit: a block the decoder is waiting for.
func (b *readAheadBudget) take(n int64) {
	b.mu.Lock()
	b.used += n
	if metricsEnabled {
		metricBufferBytes.WithLabelValues(audio(stageDownload, "slopify")...).Set(float64(b.used))
	}
	b.mu.Unlock()
}

func (b *readAheadBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *readAheadBudget) release(n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	if metricsEnabled {
		metricBufferBytes.WithLabelValues(audio(stageDownload, "slopify")...).Set(float64(b.used))
	}
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// rangeSource serves a remote original through bounded range requests.
// Upstream reads are consumed completely, letting Go reuse HTTPS connections
// during FLAC's binary seek. Without read-ahead at most 512 KiB is cached; with
// it, a background downloader fetches the rest of the file ahead of the reader
// within a shared budget and closes complete once it reaches the end.
type rangeSource struct {
	url    string
	client *http.Client

	mu                sync.Mutex
	size              int64
	etag, contentType string
	blocks            map[int64][]byte
	order             []int64 // least recently used first
	inflight          map[int64]chan struct{}
	pos               int64 // block the reader is in
	held              int64 // bytes charged to budget
	budget            *readAheadBudget
	stop              context.CancelFunc
	wake              chan struct{} // closed and replaced when pos moves or a block arrives
	complete          chan struct{}
	completed, closed bool
}

func newRangeSource(url string, client *http.Client) *rangeSource {
	return &rangeSource{url: url, client: client, blocks: map[int64][]byte{}, inflight: map[int64]chan struct{}{},
		wake: make(chan struct{}), complete: make(chan struct{})}
}

// fetch requests n blocks from offset. size and etag are what is known so far.
func (s *rangeSource) fetch(ctx context.Context, offset int64, n int, size int64, etag string) (blocks [][]byte, gotSize int64, gotETag, contentType string, err error) {
	end := offset + int64(n)*sourceBlockSize - 1
	if size > 0 {
		end = min(end, size-1)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", s.url, nil)
	if err != nil {
		return nil, 0, "", "", sourceFailure("url", "invalid source URL")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	req.Header.Set("Accept-Encoding", "identity")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	fetchAt := time.Now()
	defer func() {
		if metricsEnabled {
			metricFetch.WithLabelValues(audio(stageDownload, "slopify")...).Observe(time.Since(fetchAt).Seconds())
		}
	}()
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, "", "", sourceFailure("network", "reading source range failed")
	}
	defer resp.Body.Close()
	var first, last int64
	if resp.StatusCode != http.StatusPartialContent {
		return nil, 0, "", "", sourceFailure("status", fmt.Sprintf("source range: HTTP %d", resp.StatusCode))
	}
	c, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &gotSize)
	if err != nil || c != 3 || first != offset || gotSize <= 0 || last != min(end, gotSize-1) || last < first || size > 0 && gotSize != size {
		return nil, 0, "", "", sourceFailure("range", "invalid source content range")
	}
	gotETag = resp.Header.Get("ETag")
	if size > 0 && etag != gotETag {
		return nil, 0, "", "", sourceFailure("changed", "source changed during seek")
	}
	for at := first; at <= last; at += sourceBlockSize {
		b := make([]byte, min(sourceBlockSize, last-at+1))
		if _, err := io.ReadFull(resp.Body, b); err != nil {
			return nil, 0, "", "", sourceFailure("range", "truncated source range")
		}
		blocks = append(blocks, b)
	}
	if metricsEnabled {
		metricBytes.WithLabelValues(audio(stageDownload, "slopify")...).Add(float64(last - first + 1))
	}
	if extra, _ := resp.Body.Read(make([]byte, 1)); extra != 0 {
		return nil, 0, "", "", sourceFailure("range", "oversized source range")
	}
	return blocks, gotSize, gotETag, resp.Header.Get("Content-Type"), nil
}

// block returns the block at offset, fetching it now unless it is cached or
// already on its way.
func (s *rangeSource) block(ctx context.Context, offset int64) ([]byte, error) {
	s.mu.Lock()
	for {
		if b, ok := s.blocks[offset]; ok {
			s.touch(offset)
			s.mu.Unlock()
			return b, nil
		}
		ch, ok := s.inflight[offset]
		if !ok {
			break
		}
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.mu.Lock()
	}
	ch := make(chan struct{})
	s.inflight[offset] = ch
	size, etag := s.size, s.etag
	s.mu.Unlock()
	blocks, gotSize, gotETag, contentType, err := s.fetch(ctx, offset, 1, size, etag)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, offset)
	close(ch)
	if err != nil {
		return nil, err
	}
	if s.size == 0 {
		s.size, s.etag, s.contentType = gotSize, gotETag, contentType
	}
	b := blocks[0]
	if s.budget != nil {
		s.budget.take(int64(len(b)))
	}
	s.store(offset, b)
	return b, nil
}

// read is block for the decoder's position, which steers eviction and read-ahead.
func (s *rangeSource) read(ctx context.Context, offset int64) ([]byte, error) {
	s.mu.Lock()
	if offset != s.pos {
		s.pos = offset
		s.evict()
		s.signal()
	}
	if _, cached := s.blocks[offset]; !cached && s.budget != nil {
		if metricsEnabled {
			metricUnderruns.WithLabelValues(audio(stageDownload, "slopify")...).Inc()
		}
	}
	s.mu.Unlock()
	return s.block(ctx, offset)
}

// store keeps a block already charged to the budget. Lock held.
func (s *rangeSource) store(offset int64, b []byte) {
	if s.closed {
		if s.budget != nil {
			s.budget.release(int64(len(b)))
		}
		return
	}
	if s.budget != nil {
		s.held += int64(len(b))
	}
	s.blocks[offset] = b
	s.order = append(s.order, offset)
	s.evict()
	s.signal()
}

func (s *rangeSource) touch(offset int64) {
	for i, o := range s.order {
		if o == offset {
			s.order = append(append(s.order[:i:i], s.order[i+1:]...), offset)
			return
		}
	}
}

// evict keeps sourceCacheBlocks blocks behind the reader, or in total without
// read-ahead, dropping the least recently used. Lock held.
func (s *rangeSource) evict() {
	behind := 0
	for _, o := range s.order {
		if s.budget == nil || o < s.pos {
			behind++
		}
	}
	for i := 0; behind > sourceCacheBlocks && i < len(s.order); {
		o := s.order[i]
		if s.budget != nil && o >= s.pos {
			i++
			continue
		}
		if s.budget != nil {
			n := int64(len(s.blocks[o]))
			s.held -= n
			s.budget.release(n)
		}
		delete(s.blocks, o)
		s.order = append(s.order[:i], s.order[i+1:]...)
		behind--
	}
}

func (s *rangeSource) signal() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// startReadAhead downloads the rest of the file in the background. The size
// must be known, and calling it again does nothing.
func (s *rangeSource) startReadAhead(budget *readAheadBudget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if budget == nil || s.budget != nil || s.closed || s.size == 0 {
		return
	}
	s.budget = budget
	for _, b := range s.blocks {
		s.held += int64(len(b))
		budget.take(int64(len(b)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	go s.readAhead(ctx)
}

func (s *rangeSource) readAhead(ctx context.Context) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		// The first missing block from the reader on, and how many follow it.
		offset, n, pending := int64(-1), 0, false
		for o := s.pos; o < s.size; o += sourceBlockSize {
			_, cached := s.blocks[o]
			_, fetching := s.inflight[o]
			pending = pending || fetching
			if cached || fetching {
				if offset >= 0 {
					break
				}
				continue
			}
			if offset < 0 {
				offset = o
			}
			if n++; n == readAheadRequestBlocks {
				break
			}
		}
		if offset < 0 {
			if !pending && !s.completed {
				s.completed = true
				close(s.complete)
			}
			wake := s.wake
			s.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		want := min(int64(n)*sourceBlockSize, s.size-offset)
		freed, ok := s.budget.tryTake(want)
		if !ok {
			wake := s.wake
			s.mu.Unlock()
			select {
			case <-freed:
			case <-wake:
			case <-ctx.Done():
				return
			}
			continue
		}
		ch := make(chan struct{})
		for i := range n {
			s.inflight[offset+int64(i)*sourceBlockSize] = ch
		}
		size, etag := s.size, s.etag
		s.mu.Unlock()
		blocks, _, _, _, err := s.fetch(ctx, offset, n, size, etag)
		s.mu.Lock()
		for i := range n {
			delete(s.inflight, offset+int64(i)*sourceBlockSize)
		}
		close(ch)
		if err != nil {
			s.budget.release(want)
			s.signal()
			s.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			errorAt(stageDownload, "slopify", errorReason(err, "network"))
			slog.Debug("read-ahead", "err", err)
			select { // The decoder still fetches what it needs itself.
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		for i, b := range blocks {
			o := offset + int64(i)*sourceBlockSize
			if _, dup := s.blocks[o]; dup || o < s.pos {
				s.budget.release(int64(len(b))) // Fetched meanwhile, or already passed.
				continue
			}
			s.store(o, b)
		}
		s.mu.Unlock()
	}
}

// close stops read-ahead and returns its memory to the budget.
func (s *rangeSource) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	if s.budget != nil {
		s.budget.release(s.held)
		s.held = 0
	}
	s.blocks, s.order = map[int64][]byte{}, nil
	s.signal()
}

type sourceReader struct {
	bytesRead uint64
	source    *rangeSource
	ctx       context.Context
	offset    int64
	failed    error // a fetch failure: the original was not read, so it is not damaged
}

func (r *sourceReader) Read(b []byte) (int, error) {
	if r.offset >= r.source.size {
		return 0, io.EOF
	}
	start := r.offset / sourceBlockSize * sourceBlockSize
	data, err := r.source.read(r.ctx, start)
	if err != nil {
		r.failed = err
		return 0, err
	}
	n := copy(b, data[r.offset-start:])
	r.offset += int64(n)
	r.bytesRead += uint64(n)
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
