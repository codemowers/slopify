package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDecodeLegacyProbeText(t *testing.T) {
	raw := append([]byte("{\"title\":\"H"), 0xe4)
	raw = append(raw, []byte("rra Sannikov\",\"artist\":\"Dag")...)
	raw = append(raw, 0xf6)
	raw = append(raw, []byte("\",\"album\":\"Tõeline\"}")...)
	var tags map[string]string
	if err := json.Unmarshal(decodeLegacyProbeText(raw), &tags); err != nil {
		t.Fatal(err)
	}
	if tags["title"] != "Härra Sannikov" || tags["artist"] != "Dagö" || tags["album"] != "Tõeline" {
		t.Fatalf("incorrect legacy/UTF-8 decoding: %+v", tags)
	}
}

func TestProbeMislabelledLatin1Tags(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mp3")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "0.1", "-c:a", "libmp3lame",
		"-map_metadata", "-1", "-id3v2_version", "0", "-write_id3v1", "0", "-y", base)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, out)
	}
	audio, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	syncsafe := func(n int) []byte {
		return []byte{byte(n >> 21 & 127), byte(n >> 14 & 127), byte(n >> 7 & 127), byte(n & 127)}
	}
	var frames []byte
	for _, tag := range []struct {
		key string
		raw []byte
	}{
		{"TIT2", []byte{'H', 0xe4, 'r', 'r', 'a', ' ', 'S', 'a', 'n', 'n', 'i', 'k', 'o', 'v'}},
		{"TPE1", []byte{'D', 'a', 'g', 0xf6}},
		{"TALB", []byte("Tõeline")},
	} {
		// ID3 encoding marker 3 declares UTF-8, despite the legacy bytes.
		payload := append([]byte{3}, tag.raw...)
		frames = append(frames, tag.key...)
		frames = append(frames, syncsafe(len(payload))...)
		frames = append(frames, 0, 0)
		frames = append(frames, payload...)
	}
	file := append([]byte{'I', 'D', '3', 4, 0, 0}, syncsafe(len(frames))...)
	file = append(file, frames...)
	file = append(file, audio...)
	if err := os.WriteFile(filepath.Join(dir, "legacy.mp3"), file, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ix := &indexer{root: root}
	metadata, err := ix.probe("legacy.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Tags == nil || metadata.Tags.Title != "Härra Sannikov" || metadata.Tags.Artist != "Dagö" || metadata.Tags.Album != "Tõeline" {
		t.Fatalf("legacy tags were damaged: %+v", metadata.Tags)
	}
}
