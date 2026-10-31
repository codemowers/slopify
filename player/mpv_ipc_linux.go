//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type mpvMessage struct {
	RequestID uint64          `json:"request_id"`
	Error     string          `json:"error"`
	Event     string          `json:"event"`
	Name      string          `json:"name"`
	Reason    string          `json:"reason"`
	EntryID   int64           `json:"playlist_entry_id"`
	Data      json.RawMessage `json:"data"`
}

// A private inherited socket avoids exposing mpv's unrestricted command API or
// signed media URLs through a filesystem socket or process arguments.
type mpvClient struct {
	cmd     *exec.Cmd
	conn    net.Conn
	replies chan mpvMessage
	events  chan mpvMessage
	done    chan struct{}
	exited  chan struct{}
	mu      sync.Mutex
	next    uint64
	once    sync.Once
}

func startMPV(binary string, args, env []string) (*mpvClient, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "mpv-parent")
	child := os.NewFile(uintptr(fds[1]), "mpv-child")
	defer parent.Close()
	defer child.Close()
	conn, err := net.FileConn(parent)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, append(args, "--input-ipc-client=fd://3")...)
	cmd.ExtraFiles = []*os.File{child}
	cmd.Env = append(os.Environ(), env...)
	// mpv can print signed URLs on errors. Only structured, bounded lifecycle
	// errors are exposed by this backend.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err = cmd.Start(); err != nil {
		conn.Close()
		return nil, err
	}
	p := &mpvClient{cmd: cmd, conn: conn, replies: make(chan mpvMessage, 1), events: make(chan mpvMessage, 64), done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		if err != nil && cmd.ProcessState != nil {
			slog.Warn("mpv process exited", "status", cmd.ProcessState.String())
		}
		conn.Close()
		close(p.exited)
	}()
	go p.read()
	return p, nil
}

func (p *mpvClient) read() {
	defer close(p.done)
	defer p.conn.Close()
	scanner := bufio.NewScanner(p.conn)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		var m mpvMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			slog.Warn("mpv IPC invalid JSON")
			return
		}
		ch := p.events
		if m.RequestID != 0 {
			ch = p.replies
		}
		select {
		case ch <- m:
		default:
			slog.Warn("mpv IPC queue overflow", "reply", m.RequestID != 0)
			// Never silently lose an EOF or format-change event. A wedged
			// consumer becomes a recoverable backend failure.
			return
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("mpv IPC reader failed", "type", fmt.Sprintf("%T", err))
	}
}

func (p *mpvClient) call(ctx context.Context, command ...any) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.next++
	label := fmt.Sprint(command[0])
	if label == "get_property" || label == "set_property" {
		// These names are fixed by the backend. Never include loadfile's URL.
		label += " " + fmt.Sprint(command[1])
	}
	b, err := json.Marshal(map[string]any{"command": command, "request_id": p.next})
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	p.conn.SetWriteDeadline(deadline)
	if _, err = p.conn.Write(append(b, '\n')); err != nil {
		return nil, errors.New("mpv IPC write failed")
	}
	for {
		select {
		case <-ctx.Done():
			// A timed-out command may have executed. Close the connection;
			// never reuse it with uncertain command ordering.
			p.conn.Close()
			return nil, fmt.Errorf("mpv %s: %w", label, ctx.Err())
		case <-p.done:
			return nil, errors.New("mpv IPC disconnected")
		case m := <-p.replies:
			if m.RequestID != p.next {
				continue
			}
			if m.Error != "success" {
				return nil, fmt.Errorf("mpv command rejected: %s", m.Error)
			}
			return m.Data, nil
		}
	}
}

func (p *mpvClient) close() {
	p.once.Do(func() {
		p.conn.Close() // mpv exits when its inherited IPC connection closes
		select {
		case <-p.exited:
		case <-time.After(time.Second):
			p.cmd.Process.Kill()
			<-p.exited
		}
		<-p.done
	})
}
