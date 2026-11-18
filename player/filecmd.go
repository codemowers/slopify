package main

// Offline subcommands for checking the native decoders against a library on disk,
// without a server: validate decodes as fast as possible, play writes to an output
// in real time. Both use exactly the decoders and ALSA path of normal playback.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codemowers/slopify/player/internal/alsa"
	"github.com/codemowers/slopify/player/internal/audiodec"
)

// fileCommand runs a subcommand named by the first argument and reports whether
// there was one, along with its exit status.
func fileCommand(args []string) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "input-info":
		return inputInfo(args[1:]), true
	case "validate":
		return validateCommand(ctx, args[1:]), true
	case "play":
		return playCommand(ctx, args[1:]), true
	}
	return 0, false
}

func validateCommand(ctx context.Context, args []string) int {
	fl := flag.NewFlagSet("validate", flag.ExitOnError)
	jobs := fl.Int("j", 2, "files decoded in parallel (1..64)")
	seek := fl.Float64("seek", 0, "start each file at this many seconds, as a resumed track would")
	verbose := fl.Bool("v", false, "also list files that decode cleanly")
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), "usage: %s validate [-j N] [-seek S] [-v] PATH...\n\n"+
			"Decodes files with the built-in decoders as fast as possible and lists the ones that\n"+
			"fail (FAIL) or decode only by working around damage (DAMAGED), with each damaged\n"+
			"section's position, byte offset and length. Directories are walked recursively for\n"+
			".flac, .wav, .mp3, .ogg and .oga, skipping dot entries. Exits 1 if any file has a\n"+
			"problem.\n\n", os.Args[0])
		fl.PrintDefaults()
	}
	fl.Parse(args)
	if fl.NArg() == 0 || *jobs < 1 || *jobs > 64 {
		fl.Usage()
		return 2
	}
	paths := make(chan string)
	var mu sync.Mutex
	var checked, failed, damaged int
	var wg sync.WaitGroup
	for range *jobs {
		wg.Go(func() {
			for p := range paths {
				r := decodeFile(ctx, p, *seek, nil)
				mu.Lock()
				checked++
				switch {
				case r.err != nil:
					failed++
				case len(r.issues) > 0:
					damaged++
				}
				if r.err != nil || len(r.issues) > 0 || *verbose {
					fmt.Println(r)
				}
				mu.Unlock()
			}
		})
	}
	walkErr := walkMedia(ctx, fl.Args(), paths)
	close(paths)
	wg.Wait()
	fmt.Fprintf(os.Stderr, "%d files, %d damaged, %d failed\n", checked, damaged, failed)
	if walkErr != nil {
		fmt.Fprintln(os.Stderr, walkErr)
		return 1
	}
	if failed > 0 || damaged > 0 || ctx.Err() != nil {
		return 1
	}
	return 0
}

func playCommand(ctx context.Context, args []string) int {
	fl := flag.NewFlagSet("play", flag.ExitOnError)
	fl.IntVar(&outputBufferMS, "output-buffer-ms", 0, outputBufferHelp)
	fl.BoolVar(&allowOutputTruncate, "allow-truncate", false, "permit truncating to 16-bit output if no precision-preserving ALSA format works")
	output := fl.String("output", "alsa:hw:0,0", "alsa:hw:CARD=...,DEV=... (snd-dummy works too) or null for real-time discard")
	seek := fl.Float64("seek", 0, "start the first file at this many seconds")
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), "usage: %s play [-output SPEC] [-output-buffer-ms MS] [-allow-truncate] [-seek S] PATH...\n\n"+
			"Plays files in real time through the built-in decoders, gaplessly while the format\n"+
			"is unchanged. The device is opened exclusively; truncation requires -allow-truncate.\n\n", os.Args[0])
		fl.PrintDefaults()
	}
	fl.Parse(args)
	if !validOutputBuffer(outputBufferMS) {
		fmt.Fprintln(os.Stderr, "output-buffer-ms must be 0 or 40 to 10000")
		return 2
	}
	if fl.NArg() == 0 {
		fl.Usage()
		return 2
	}
	kind, target, _ := strings.Cut(*output, ":")
	var open func(format) (nativeOutput, error)
	switch kind {
	case "null":
		open = func(f format) (nativeOutput, error) { return &nullOutput{f: f, start: time.Now()}, nil }
	case "alsa", "aplay":
		if target == "" {
			target = "hw:0,0"
		}
		if _, _, err := alsa.ParseDevice(target); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		open = func(f format) (nativeOutput, error) { return openALSAOutput(target, f, false) }
	default:
		fmt.Fprintf(os.Stderr, "output must be alsa:hw:... or null, not %q\n", *output)
		return 2
	}
	paths := make(chan string)
	go func() {
		if err := walkMedia(ctx, fl.Args(), paths); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		close(paths)
	}()
	var out nativeOutput
	var current format
	defer func() {
		if out != nil {
			if ctx.Err() == nil {
				out.Drain()
			}
			out.Close()
		}
	}()
	status, at := 0, *seek
	for p := range paths {
		var device string
		r := decodeFile(ctx, p, at, func(f format, samples []int32) error {
			if out == nil || f != current {
				if out != nil {
					if err := out.Drain(); err != nil {
						return err
					}
					out.Close()
					out = nil
				}
				opened, err := open(f)
				if err != nil {
					return err
				}
				out, current = opened, f
				pcm := out.Format()
				device = fmt.Sprintf("%s %d Hz %dch", pcm.Format, pcm.Rate, pcm.Channels)
				fmt.Fprintf(os.Stderr, "output %s\n", device)
			}
			return out.Write(samples)
		})
		at = 0
		fmt.Println(r)
		if r.err != nil {
			status = 1
		}
		if ctx.Err() != nil {
			return 1
		}
	}
	return status
}

// walkMedia sends explicit files as given and the media files below directories.
func walkMedia(ctx context.Context, roots []string, paths chan<- string) error {
	var errs []error
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !info.IsDir() {
			select {
			case paths <- root:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, err)
				return nil
			}
			if p != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".flac", ".wav", ".mp3", ".ogg", ".oga":
			default:
				return nil
			}
			select {
			case paths <- p:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

type decodeResult struct {
	path    string
	format  audiodec.Format
	start   float64
	seconds float64 // decoded, from start
	offset  int64   // file position when decoding stopped
	elapsed time.Duration
	issues  []audiodec.Issue
	err     error
}

func (r decodeResult) String() string {
	f := r.format
	shape := fmt.Sprintf("%s %dHz %dbit %dch", f.Codec, f.Rate, f.Bits, f.Channels)
	if f.Rate == 0 {
		shape = "-"
	}
	var b strings.Builder
	switch {
	case r.err != nil:
		fmt.Fprintf(&b, "FAIL    %s at=%.3fs offset=%d %s: %v", r.path, r.start+r.seconds, r.offset, shape, r.err)
	case len(r.issues) > 0:
		fmt.Fprintf(&b, "DAMAGED %s %.3fs %s", r.path, r.start+r.seconds, shape)
	default:
		fmt.Fprintf(&b, "OK      %s %.3fs %s in %s", r.path, r.start+r.seconds, shape, r.elapsed.Round(time.Millisecond))
	}
	for _, i := range r.issues {
		fmt.Fprintf(&b, "\n        %s", i)
	}
	return b.String()
}

// countingFile records how far decoding has read, to locate a failure in the file.
type countingFile struct {
	*os.File
	pos int64
}

func (c *countingFile) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	c.pos += int64(n)
	return n, err
}

func (c *countingFile) Seek(offset int64, whence int) (int64, error) {
	pos, err := c.File.Seek(offset, whence)
	if err == nil {
		c.pos = pos
	}
	return pos, err
}

// decodeFile decodes one file from at seconds, handing each block to write if set.
func decodeFile(ctx context.Context, path string, at float64, write func(format, []int32) error) (r decodeResult) {
	r.path = path
	began := time.Now()
	defer func() { r.elapsed = time.Since(began) }()
	file, err := os.Open(path)
	if err != nil {
		r.err = err
		return r
	}
	defer file.Close()
	src := &countingFile{File: file}
	dec, err := audiodec.OpenWith(src, audiodec.Options{Report: func(i audiodec.Issue) { r.issues = append(r.issues, i) }})
	if err != nil {
		r.err, r.offset = err, src.pos
		return r
	}
	r.format = dec.Format()
	if at > 0 {
		if r.start, err = dec.Seek(at); err != nil {
			r.err, r.offset = fmt.Errorf("seek to %.3fs: %w", at, err), src.pos
			return r
		}
	}
	f := format{rate: r.format.Rate, channels: r.format.Channels, bits: r.format.Bits}
	buffer := make([]int32, nativeChunkSamples)
	var samples int64
	for ctx.Err() == nil {
		n, err := dec.Read(buffer)
		samples += int64(n)
		r.seconds = float64(samples/int64(f.channels)) / float64(f.rate)
		if n > 0 && write != nil {
			if werr := write(f, buffer[:n]); werr != nil {
				r.err, r.offset = fmt.Errorf("output: %w", werr), src.pos
				return r
			}
		}
		if errors.Is(err, io.EOF) {
			return r
		}
		if err != nil {
			r.err, r.offset = err, src.pos
			return r
		}
	}
	r.err = ctx.Err()
	return r
}
