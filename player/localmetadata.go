package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Only descriptive media properties cross into the unauthenticated LAN UI.
// Filenames, URLs, credentials and arbitrary application properties do not.
func mediaMetadataProperty(key string) bool {
	switch key {
	case "media.name", "media.title", "media.artist", "media.album", "media.album-artist", "media.genre", "media.date", "media.track-number", "media.disc-number", "media.copyright", "media.software", "media.language", "media.role", "media.icon-name":
		return true
	}
	return false
}
func metadataText(value string) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) > 2048 {
		value = value[:2048]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}
func sourceMetadata(props map[string]string) map[string]string {
	result := map[string]string{}
	for key, text := range props {
		if mediaMetadataProperty(key) && text != "" && text != "-" {
			result[key] = metadataText(text)
		}
	}
	return result
}

// Preserve every descriptive/technical field supplied in the queue entry, but
// not its internal ID or library path. The source format remains separate.
func entryMetadata(e *entry) map[string]string {
	result := map[string]string{}
	for key, value := range map[string]string{"title": e.Title, "artist": e.Artist, "album": e.Album, "kind": e.Kind, "codec": e.Codec, "videoCodec": e.VideoCodec} {
		if value != "" {
			result[key] = metadataText(value)
		}
	}
	for key, value := range map[string]int64{"size": e.Size, "bitrate": e.Bitrate, "sampleRate": int64(e.SampleRate), "channels": int64(e.Channels), "bits": int64(e.Bits)} {
		if value > 0 {
			result[key] = strconv.FormatInt(value, 10)
		}
	}
	if e.Duration > 0 {
		result["duration"] = strconv.FormatFloat(e.Duration, 'f', -1, 64)
	}
	return result
}
