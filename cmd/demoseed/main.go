// demoseed imports the pinned, redistributable demo recordings. It is shipped
// only in the indexer image, the service permitted to write the media tree.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed tracks.json
var manifest []byte

type track struct {
	URL     string `json:"url"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA1    string `json:"sha1,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Source  string `json:"source"`
	License string `json:"license"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rootPath := os.Getenv("MEDIA_ROOT")
	if rootPath == "" {
		rootPath = "/media"
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		log.Fatal(err)
	}
	defer root.Close()
	var tracks []track
	if err := json.Unmarshal(manifest, &tracks); err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" {
			return errors.New("unsafe redirect")
		}
		return nil
	}}
	jobs := make(chan track, len(tracks))
	errs := make(chan error, len(tracks))
	for _, t := range tracks {
		jobs <- t
	}
	close(jobs)
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			for t := range jobs {
				if ctx.Err() != nil {
					errs <- ctx.Err()
					return
				}
				var err error
				for attempt := 0; attempt < 3; attempt++ {
					err = fetch(ctx, client, root, t)
					if err == nil || ctx.Err() != nil {
						break
					}
					log.Printf("retry %s: %v", t.Path, err)
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
					}
				}
				if err != nil {
					errs <- err
				}
			}
		})
	}
	workers.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if err := errors.Join(failures...); err != nil {
		log.Fatal(err)
	}
	log.Printf("demo library ready: %d recordings", len(tracks))
}

func checksum(t track) (hash.Hash, string) {
	if t.SHA256 != "" {
		return sha256.New(), t.SHA256
	}
	return sha1.New(), t.SHA1 // Archive.org publishes SHA-1 for its original files.
}

func matches(r io.Reader, t track) bool {
	h, want := checksum(t)
	n, err := io.Copy(h, io.LimitReader(r, t.Size+1))
	return err == nil && n == t.Size && hex.EncodeToString(h.Sum(nil)) == want
}

func fetch(ctx context.Context, client *http.Client, root *os.Root, t track) error {
	if t.Size <= 0 || t.Size > 512<<20 || !strings.HasPrefix(t.URL, "https://") ||
		!filepath.IsLocal(t.Path) {
		return fmt.Errorf("invalid manifest entry: %s", t.Path)
	}
	if f, err := root.Open(t.Path); err == nil {
		valid := matches(f, t)
		f.Close()
		if valid {
			log.Printf("already verified: %s", t.Path)
			return nil
		}
		// Never replace an existing media file, even if it differs from our manifest.
		return fmt.Errorf("existing file differs from demo manifest: %s", t.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.MkdirAll(path.Dir(t.Path), 0755); err != nil {
		return err
	}
	part := path.Join(path.Dir(t.Path), "."+path.Base(t.Path)+"."+rand.Text()+".part")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Slopify-demo-seed/1.0 (https://github.com/codemowers/slopify)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", t.Path, resp.StatusCode)
	}
	f, err := root.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(part)
	h, want := checksum(t)
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, t.Size+1))
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != t.Size || hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("size/checksum mismatch: %s", t.Path)
	}
	// Publishing by hard link fails if another importer or user created the file.
	// The deferred removal drops only our unique hidden temporary name.
	if err := root.Link(part, t.Path); err != nil {
		if errors.Is(err, os.ErrExist) {
			f, openErr := root.Open(t.Path)
			if openErr != nil {
				return openErr
			}
			defer f.Close()
			if matches(f, t) {
				return nil
			}
		}
		return err
	}
	log.Printf("downloaded: %s (%d bytes)", t.Path, n)
	return nil
}
