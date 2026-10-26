package main

import (
	"os"
	"testing"

	"github.com/codemowers/slopify/internal/events"
	"github.com/codemowers/slopify/internal/sidecar"
)

func TestEmptyDirectoryPreservesMetadata(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := sidecar.Write(root, ".", &sidecar.Sidecar{Version: sidecar.Version, Entries: []sidecar.Entry{{Name: "gone.mp3"}}}); err != nil {
		t.Fatal(err)
	}
	const marker = ".slopify/keep.txt"
	if err := root.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	ix := &indexer{root: root, hub: events.NewHub[events.Event]()}
	for range 2 {
		if err := ix.indexDir("."); err != nil {
			t.Fatal(err)
		}
		s, err := sidecar.Read(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		if s.Version != sidecar.Version || s.Entries == nil || len(s.Entries) != 0 {
			t.Fatalf("expected empty sidecar, got %+v", s)
		}
		b, err := root.ReadFile(marker)
		if err != nil || string(b) != "keep" {
			t.Fatalf("metadata file changed: %q, %v", b, err)
		}
	}
}
