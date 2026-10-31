package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLocalTrackMetadata(t *testing.T) {
	var e entry
	if err := json.Unmarshal([]byte(`{"id":7,"path":"private/song.flac","title":"Song","artist":"Artist","album":"Album","duration":12.5,"codec":"flac","bits":24,"sampleRate":96000,"channels":2,"bitrate":123456,"size":987654,"kind":"audio"}`), &e); err != nil {
		t.Fatal(err)
	}
	m := entryMetadata(&e)
	if m["artist"] != "Artist" || m["album"] != "Album" || m["duration"] != "12.5" || m["sampleRate"] != "96000" || m["bits"] != "24" {
		t.Fatalf("metadata: %#v", m)
	}
	if _, ok := m["path"]; ok {
		t.Fatal("exposed path")
	}
	if _, ok := m["id"]; ok {
		t.Fatal("exposed internal ID")
	}
}
func TestLocalSenderMetadataBoundary(t *testing.T) {
	m := sourceMetadata(map[string]string{"media.artist": "Example", "media.title": "<script>text</script>", "media.filename": "https://secret/", "application.token": "secret", "media.album": strings.Repeat("é", 1100)})
	if len(m) != 3 || m["media.artist"] != "Example" {
		t.Fatalf("unexpected fields: %#v", m)
	}
	if len(m["media.album"]) > 2048 || !utf8.ValidString(m["media.album"]) {
		t.Fatal("unbounded or invalid metadata")
	}
}
