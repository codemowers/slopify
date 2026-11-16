package main

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/codemowers/slopify/internal/media"
	"github.com/codemowers/slopify/internal/sidecar"
)

// Credit text is shown as-is; long values such as liner notes in a copyright
// field are cut so that sidecars stay small on large libraries.
const maxCreditText = 512

// At most this many artist identifiers are kept per field.
const maxCreditIDs = 8

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	isrcPattern = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{3}[0-9]{7}$`)
	keyNoise    = strings.NewReplacer(" ", "", "_", "", "-", "")
)

// creditKey folds the spellings formats use for one field: Vorbis
// MUSICBRAINZ_TRACKID, ID3 TXXX/MP4 "MusicBrainz Track Id" and so on.
func creditKey(k string) string { return keyNoise.Replace(strings.ToLower(k)) }

// credits picks well-known fields from tags already lowercased by probe.
// Identifiers that fail validation are dropped rather than stored.
func credits(tags map[string]string) *sidecar.Credits {
	folded := map[string]string{}
	for k, v := range tags {
		if v = strings.TrimSpace(strings.Trim(v, "\x00")); v != "" {
			folded[creditKey(k)] = v
		}
	}
	text := func(keys ...string) string {
		for _, k := range keys {
			if v := folded[k]; v != "" && !media.CorruptText(v) {
				return truncateText(v, maxCreditText)
			}
		}
		return ""
	}
	ids := func(k string) []string {
		var out []string
		for _, v := range strings.FieldsFunc(folded[k], func(r rune) bool { return r == ';' || r == '/' || r == ',' || r == ' ' }) {
			if v = strings.ToLower(v); uuidPattern.MatchString(v) && len(out) < maxCreditIDs && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out
	}
	id := func(k string) string {
		if v := ids(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	c := sidecar.Credits{
		Composer:     text("composer"),
		Copyright:    text("copyright"),
		License:      text("license", "licence"),
		Label:        text("label", "publisher", "organization"),
		ISRC:         isrc(folded["isrc"]),
		AcoustID:     id("acoustidid"),
		Recording:    id("musicbrainztrackid"),
		ReleaseTrack: id("musicbrainzreleasetrackid"),
		Release:      id("musicbrainzalbumid"),
		ReleaseGroup: id("musicbrainzreleasegroupid"),
		Work:         id("musicbrainzworkid"),
		Artists:      ids("musicbrainzartistid"),
		AlbumArtists: ids("musicbrainzalbumartistid"),
	}
	if c.Composer == "" && c.Copyright == "" && c.License == "" && c.Label == "" && c.ISRC == "" && c.AcoustID == "" &&
		c.Recording == "" && c.ReleaseTrack == "" && c.Release == "" && c.ReleaseGroup == "" && c.Work == "" &&
		c.Artists == nil && c.AlbumArtists == nil {
		return nil
	}
	return &c
}

// isrc accepts the first valid code of a possibly multi-valued tag.
func isrc(v string) string {
	for _, code := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == '/' || r == ',' || r == ' ' }) {
		if code = strings.ToUpper(strings.ReplaceAll(code, "-", "")); isrcPattern.MatchString(code) {
			return code
		}
	}
	return ""
}

// truncateText cuts at a rune boundary and marks the cut.
func truncateText(v string, limit int) string {
	if len(v) <= limit {
		return v
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + "…"
}
