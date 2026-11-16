package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os/exec"
	"strings"
	"unicode/utf8"
)

const maxErrorSamples = 3
const maxProbeStderr = 1024

type directoryIssue struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
	Stderr    string `json:"stderr,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

type directoryReport struct {
	dir     string
	metrics *indexMetrics
	issues  int
	samples []directoryIssue
	files   int
	updated bool
	failed  bool
}

func errorReason(err error, stderr string) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	}
	text := strings.ToLower(err.Error() + " " + stderr)
	switch {
	case strings.Contains(text, "permission denied"), strings.Contains(text, "operation not permitted"):
		return "permission"
	case strings.Contains(text, "no such file"):
		return "not_found"
	case strings.Contains(text, "invalid data"), strings.Contains(text, "invalid character"), strings.Contains(text, "moov atom not found"), strings.Contains(text, "unexpected end of json"):
		return "invalid_media"
	default:
		return "other"
	}
}

func (r *directoryReport) add(operation, path string, err error) {
	issue := directoryIssue{Operation: operation, Path: path, Error: err.Error()}
	var pe *probeError
	if errors.As(err, &pe) {
		issue.Stderr = pe.stderr
		var exit *exec.ExitError
		if errors.As(pe.err, &exit) {
			code := exit.ExitCode()
			issue.ExitCode = &code
		}
		if r.metrics != nil && operation == "probe" {
			r.metrics.probeFailures.Inc()
		}
	}
	issue.Reason = errorReason(err, issue.Stderr)
	if r.metrics != nil {
		r.metrics.errors.WithLabelValues(operation, issue.Reason).Inc()
	}
	r.issues++
	if len(r.samples) < maxErrorSamples {
		r.samples = append(r.samples, issue)
	} else if operation != "probe" {
		for i := len(r.samples) - 1; i >= 0; i-- {
			if r.samples[i].Operation == "probe" {
				r.samples[i] = issue
				break
			}
		}
	}
}

func (r *directoryReport) finish() {
	args := []any{"dir", r.dir, "files", r.files, "updated", r.updated}
	if r.issues > 0 {
		// JSON keeps field names and numeric exit codes readable with both
		// text and JSON slog handlers, rather than printing pointer values.
		samples, _ := json.Marshal(r.samples)
		args = append(args, "errors", r.issues, "samples", string(samples))
		if r.failed {
			slog.Error("directory indexing failed", args...)
		} else {
			slog.Warn("directory indexed with errors", args...)
		}
	} else if r.updated {
		slog.Info("directory indexed", args...)
	}
}

func (r *directoryReport) result() error {
	if r.issues == 0 {
		return nil
	}
	return fmt.Errorf("directory %q: %d indexing errors", r.dir, r.issues)
}

type probeError struct {
	err    error
	stderr string
}

func (e *probeError) Error() string { return "ffprobe: " + e.err.Error() }
func (e *probeError) Unwrap() error { return e.err }

// Keep stderr bounded while continuing to drain the pipe so ffprobe can't block.
type stderrBuffer struct{ data []byte }

func (b *stderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := maxProbeStderr - len(b.data); room > 0 {
		b.data = append(b.data, p[:min(room, n)]...)
	}
	return n, nil
}

func (ix *indexer) probeCommand(ctx context.Context, rel string) ([]byte, error) {
	f, err := ix.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json=string_validation=ignore",
		"-show_entries", "format=duration,bit_rate:format_tags:stream=codec_type,codec_name,bit_rate,sample_rate,channels,bits_per_raw_sample,bits_per_sample,sample_fmt:stream_tags", "/dev/fd/3")
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	stderr := &stderrBuffer{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		// Prefer timeout/cancellation over the less useful "signal: killed".
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, &probeError{err: err, stderr: strings.TrimSpace(string(stderr.data))}
	}
	return decodeLegacyProbeText(out), nil
}

// Preserve valid UTF-8, but decode invalid bytes as ISO-8859-1 before JSON
// unmarshalling can irreversibly replace them. Some ID3 tags contain Latin-1
// bytes despite declaring UTF-8. ffprobe's default JSON writer also replaces
// these bytes, so probeCommand explicitly disables that validation.
func decodeLegacyProbeText(raw []byte) []byte {
	if utf8.Valid(raw) {
		return raw
	}
	out := make([]byte, 0, len(raw))
	for len(raw) > 0 {
		r, n := utf8.DecodeRune(raw)
		if r == utf8.RuneError && n == 1 {
			out = utf8.AppendRune(out, rune(raw[0]))
		} else {
			out = append(out, raw[:n]...)
		}
		raw = raw[n:]
	}
	return out
}
