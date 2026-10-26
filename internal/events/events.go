// Package events carries change notifications between services over
// Server-Sent Events.
package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// Changed means the sidecar of Path was rewritten or removed.
	Changed = "changed"
	// Removed means Path and everything below it is gone.
	Removed = "removed"
)

// Event is a library change published by the indexer.
type Event struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// Hub fans messages out to subscribers. Slow subscribers are disconnected
// rather than blocking the publisher; they resynchronize on reconnect.
type Hub[T any] struct {
	mu   sync.Mutex
	subs map[chan T]struct{}
}

func NewHub[T any]() *Hub[T] { return &Hub[T]{subs: map[chan T]struct{}{}} }

func (h *Hub[T]) Publish(m T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- m:
		default:
			delete(h.subs, c)
			close(c)
		}
	}
}

// Subscribe returns a channel of published messages, closed when the
// subscriber falls behind or cancel is called.
func (h *Hub[T]) Subscribe() (<-chan T, func()) {
	c := make(chan T, 256)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[c]; ok {
			delete(h.subs, c)
			close(c)
		}
	}
}

// Stream prepares w for Server-Sent Events and returns a function writing one
// JSON-encoded event of the given type, reporting whether the client is
// still connected.
func Stream(w http.ResponseWriter) func(event string, v any) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	return func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		return rc.Flush() == nil
	}
}

func (h *Hub[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, cancel := h.Subscribe()
	defer cancel()
	send := Stream(w)
	// An initial comment tells the subscriber it is connected, so it can
	// resynchronize knowing no later message will be missed.
	fmt.Fprint(w, ": connected\n\n")
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case m, ok := <-c:
			if !ok || !send("", m) {
				return
			}
		}
	}
}

// Subscribe connects to an SSE endpoint, calls connected once the stream is
// open and then handle for every event, until the stream ends.
func Subscribe(ctx context.Context, url string, connected func(), handle func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := subscriberClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe %s: %s", url, resp.Status)
	}
	connected()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return err
		}
		handle(e)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("subscribe %s: stream ended", url)
}

// A stream may remain open indefinitely, but headers and individual reads must
// make progress. Hub heartbeats keep healthy, quiet subscriptions alive.
var subscriberClient = &http.Client{Transport: subscriberTransport()}

func subscriberTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 5 * time.Second
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return idleReadConn{conn}, nil
	}
	return transport
}

type idleReadConn struct{ net.Conn }

func (c idleReadConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}
