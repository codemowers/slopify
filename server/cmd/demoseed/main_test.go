package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestFetchAtomicAndIdempotent(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "corrupt"}[corrupt], func(t *testing.T) {
			data := "recording"
			sum := sha256.Sum256([]byte(data))
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if corrupt {
					io.WriteString(w, "bad")
				} else {
					io.WriteString(w, data)
				}
			}))
			defer server.Close()
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			tr := track{URL: server.URL, Path: "Artist/Album/Track.flac", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
			err = fetch(context.Background(), server.Client(), root, tr)
			if corrupt {
				if err == nil {
					t.Fatal("accepted corrupt recording")
				}
				if _, err := root.Stat(tr.Path); !os.IsNotExist(err) {
					t.Fatalf("published partial recording: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = fetch(context.Background(), server.Client(), root, tr); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("downloaded again: %d requests", calls)
			}
			if err = root.WriteFile(tr.Path, []byte("user media"), 0644); err != nil {
				t.Fatal(err)
			}
			if err = fetch(context.Background(), server.Client(), root, tr); err == nil {
				t.Fatal("overwrote different existing media")
			}
		})
	}
}
