package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThirtyLANSourcesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.json")
	s := newSpeakerSources(path)
	for i := 1; i <= 30; i++ {
		s.Sources = append(s.Sources, &speakerSource{ID: fmt.Sprintf("lan_%d", i), Key: strings.Repeat("k", 512), Name: strings.Repeat("n", 160)})
	}
	s.Selected = "lan_30"
	s.save()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 16384 {
		t.Fatal("fixture must exceed the previous settings size limit")
	}
	restored := newSpeakerSources(path)
	if len(restored.Sources) != 31 || restored.Selected != "lan_30" {
		t.Fatalf("lost sources or selection: %d %s", len(restored.Sources), restored.Selected)
	}
	for _, id := range []string{"lan_0", "lan_31", "lan_01", "lan_+1", "30", "lan_"} {
		if validSourceID(id) {
			t.Fatalf("accepted invalid source ID %q", id)
		}
	}
}
