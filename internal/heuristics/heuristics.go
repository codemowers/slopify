// Package heuristics guesses artist, album, title and numbering from library
// paths, so that untagged files are still browsable and searchable.
package heuristics

import (
	"github.com/codemowers/slopify/internal/media"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/codemowers/slopify/internal/sidecar"
)

var (
	discDir     = regexp.MustCompile(`(?i)^(?:cd|disc|disk)\s*(\d{1,2})$`)
	yearSuffix  = regexp.MustCompile(`^(.*?)\s*[(\[](\d{4})[)\]]$`)
	yearPrefix  = regexp.MustCompile(`^[(\[](\d{4})[)\]]\s*(.+)$`)
	catalogNum  = regexp.MustCompile(`(?i)^[0-9]*[a-z]+[0-9]+[0-9a-z]*[ -](\d{1,3})[ -](.+)$`)
	discTrack   = regexp.MustCompile(`^(\d)[-.](\d{2})(?:[ .-]+)(.+)$`)
	artistTrack = regexp.MustCompile(`^(.+?)\s+-\s*(\d{1,3})\s*-\s*(.+)$`)
	trackNumber = regexp.MustCompile(`^(\d{1,3})(?:\s*[.)-]\s*|\s+)(.+)$`)
)

// Guess derives metadata from a library-relative file path and returns it with
// a confidence between 0 and 1.
func Guess(rel string) (sidecar.Info, float64) {
	if kind, _, ok := media.Classify(rel); ok && kind == media.Video {
		return videoGuess(rel)
	}
	var info sidecar.Info
	dir, file := path.Split(rel)
	stem := strings.TrimSuffix(file, path.Ext(file))
	dirs := []string{}
	if d := strings.Trim(dir, "/"); d != "" {
		dirs = strings.Split(d, "/")
	}

	// A CD1/Disc 2 folder sits between the album folder and the files.
	if n := len(dirs); n > 0 {
		if m := discDir.FindStringSubmatch(spaced(dirs[n-1])); m != nil {
			info.Disc, _ = strconv.Atoi(m[1])
			dirs = dirs[:n-1]
		}
	}
	if n := len(dirs); n > 0 {
		albumArtist, album, year := parseAlbumDir(spaced(dirs[n-1]))
		info.Album, info.Year = album, year
		if albumArtist == "" && n > 1 {
			albumArtist = spaced(dirs[n-2])
		}
		info.AlbumArtist = albumArtist
	}

	underscored := !strings.Contains(stem, " ") && strings.Contains(stem, "_")
	name := spaced(stem)
	if m := catalogNum.FindStringSubmatch(name); m != nil {
		info.Track, _ = strconv.Atoi(m[1])
		name = m[2]
	} else if m := discTrack.FindStringSubmatch(name); m != nil {
		info.Disc, _ = strconv.Atoi(m[1])
		info.Track, _ = strconv.Atoi(m[2])
		name = m[3]
	} else if m := trackNumber.FindStringSubmatch(name); m != nil {
		info.Track, _ = strconv.Atoi(m[1])
		name = m[2]
	}

	artistNumber := artistTrack.FindStringSubmatch(name)
	switch parts := splitDash(name); {
	case artistNumber != nil:
		info.Artist, info.Title = strings.TrimSpace(artistNumber[1]), strings.TrimSpace(artistNumber[3])
		info.Track, _ = strconv.Atoi(artistNumber[2])
	case len(parts) >= 3:
		info.Artist, info.Title = parts[0], strings.Join(parts[2:], " - ")
		if info.Album == "" {
			info.Album = parts[1]
		}
	case len(parts) == 2:
		info.Artist, info.Title = parts[0], parts[1]
	case underscored && strings.Contains(name, "-"):
		// artist_name-track_title: spaces were underscores, so a bare hyphen
		// separates fields. Titles contain hyphens more often than artist
		// names do, but "chibi-tech" shows artists can too; split on the last.
		i := strings.LastIndex(name, "-")
		info.Artist, info.Title = strings.TrimSpace(name[:i]), strings.TrimSpace(name[i+1:])
	default:
		info.Title = name
	}
	if info.Artist == "" {
		info.Artist = info.AlbumArtist
	}
	if info.Title == "" {
		info.Title = stem
	}
	info.Artist, info.Title, info.Album = titleCase(info.Artist), titleCase(info.Title), titleCase(info.Album)
	info.AlbumArtist = titleCase(info.AlbumArtist)

	confidence := 0.2
	if info.Artist != "" {
		confidence += 0.3
	}
	if info.Album != "" {
		confidence += 0.2
	}
	if info.Track != 0 {
		confidence += 0.1
	}
	if underscored {
		confidence -= 0.1
	}
	return info, confidence
}

func parseAlbumDir(name string) (artist, album string, year int) {
	if parts := splitDash(name); len(parts) >= 2 {
		if y, err := strconv.Atoi(parts[0]); err == nil && len(parts[0]) == 4 {
			// "1997 - OK Computer"
			return "", strings.Join(parts[1:], " - "), y
		}
		artist, name = parts[0], strings.Join(parts[1:], " - ")
		if len(parts) >= 3 {
			// "Artist - 1997 - Album"
			if y, err := strconv.Atoi(parts[1]); err == nil && len(parts[1]) == 4 {
				return artist, strings.Join(parts[2:], " - "), y
			}
		}
	}
	if m := yearSuffix.FindStringSubmatch(name); m != nil {
		year, _ = strconv.Atoi(m[2])
		return artist, m[1], year
	}
	if m := yearPrefix.FindStringSubmatch(name); m != nil {
		year, _ = strconv.Atoi(m[1])
		return artist, m[2], year
	}
	return artist, name, 0
}

func splitDash(s string) []string {
	parts := strings.Split(s, " - ")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// spaced turns underscore-separated names into spaced ones.
func spaced(s string) string {
	if strings.Contains(s, " ") {
		return s
	}
	return strings.TrimSpace(strings.ReplaceAll(s, "_", " "))
}

// titleCase capitalizes words of all-lowercase names and leaves deliberately
// cased names such as "of Montreal" or "dEUS" alone.
func titleCase(s string) string {
	if s != strings.ToLower(s) {
		return s
	}
	r := []rune(s)
	start := true
	for i, c := range r {
		if start && unicode.IsLetter(c) {
			r[i] = unicode.ToUpper(c)
		}
		start = unicode.IsSpace(c) || c == '-' || c == '(' || c == '/'
	}
	return string(r)
}

var videoRelease = regexp.MustCompile(`(?i)(?:^|[ \[(])(?:2160p|1080[pi]|720p|576p|480p|4k|8k|web[- ]?dl|webrip|blu[- ]?ray|b[dr]rip|dvdrip|hdtv|remux|x26[45]|h[ -]?26[45]|hevc|avc|av1|dts(?:[- ]?hd)?|truehd|aac|ddp|eac3|ac3|hdr10?\+?|dolby[ -]?vision)(?:$|[ \])_-])`)
var videoYear = regexp.MustCompile(`(?:^|[ (\[])(19[0-9]{2}|20[0-9]{2})(?:$|[ )\]])`)

func videoGuess(rel string) (sidecar.Info, float64) {
	stem := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	name := strings.Join(strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(stem)), " ")
	if match := videoRelease.FindStringIndex(name); match != nil && match[0] > 0 {
		name = strings.TrimSpace(name[:match[0]])
	}
	year := 0
	for _, match := range videoYear.FindAllStringSubmatchIndex(name, -1) {
		prefix := strings.TrimSpace(strings.TrimRight(name[:match[0]], " (["))
		candidate, _ := strconv.Atoi(name[match[2]:match[3]])
		if prefix != "" && candidate <= time.Now().Year()+1 {
			year = candidate
			name = prefix
			break
		}
	}
	name = strings.Trim(name, " -_[]()")
	if name == "" {
		name = stem
	}
	return sidecar.Info{Title: name, Year: year}, .5
}
