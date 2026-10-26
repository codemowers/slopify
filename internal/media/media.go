// Package media classifies library files and validates library-relative paths.
package media

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Kind string

const (
	Audio Kind = "audio"
	Video Kind = "video"
)

var types = map[string]struct {
	kind Kind
	mime string
}{
	".mp3":  {Audio, "audio/mpeg"},
	".flac": {Audio, "audio/flac"},
	".ogg":  {Audio, "audio/ogg"},
	".oga":  {Audio, "audio/ogg"},
	".opus": {Audio, "audio/ogg"},
	".m4a":  {Audio, "audio/mp4"},
	".aac":  {Audio, "audio/aac"},
	".wav":  {Audio, "audio/wav"},
	".mp4":  {Video, "video/mp4"},
	".m4v":  {Video, "video/mp4"},
	".webm": {Video, "video/webm"},
	".mkv":  {Video, "video/x-matroska"},
	".mov":  {Video, "video/quicktime"},
}

// Classify returns the media kind and MIME type of a file name, or ok=false
// for files that are not part of the library.
func Classify(name string) (kind Kind, mime string, ok bool) {
	t, ok := types[strings.ToLower(path.Ext(name))]
	return t.kind, t.mime, ok
}

var ErrBadPath = errors.New("invalid library path")

// CleanPath normalizes a slash-separated, library-relative path. The empty
// string and "." denote the library root. Absolute paths, ".." segments and
// hidden segments (such as the .slopify sidecar directory) are rejected.
func CleanPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ".", nil
	}
	if path.Clean(p) != p {
		return "", ErrBadPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || strings.HasPrefix(seg, ".") {
			return "", ErrBadPath
		}
	}
	return p, nil
}

// Track labels describe a stream, not the title of the containing movie.
var technicalTitle = regexp.MustCompile(`(?i)^(?:(?:dts(?:[ -]?hd)?(?:[ -]?ma)?|truehd|aac|ac[ -]?3|e[ -]?ac[ -]?3|ddp|dolby(?:[ -]?(?:digital|atmos))?|flac|pcm|opus|vorbis|audio|english|eng|russian|rus|german|deu|french|fra|japanese|jpn|estonian|est|stereo|surround|mono|[125678]\.[01]|\d+\s*(?:kbps|kbit/s|hz|khz|bit|channels?))(?:[ ._/-]+|$))+$`)

func TechnicalVideoTitle(title string) bool {
	return technicalTitle.MatchString(strings.TrimSpace(title))
}

// CorruptText detects irrecoverably replaced or invalid metadata text.
func CorruptText(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsRune(s, utf8.RuneError)
}
