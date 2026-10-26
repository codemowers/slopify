// Package sidecar defines the per-directory metadata file that the indexer
// writes beside the media it describes. Because the metadata lives in the
// directory, moving or renaming a directory keeps its metadata and nothing
// central has to be rebuilt.
package sidecar

import (
	"encoding/json"
	"os"
	"path"
	"time"

	"github.com/codemowers/slopify/internal/media"
)

const (
	Dir     = ".slopify"
	File    = "dir.json"
	Version = 1
)

// Info is descriptive metadata from embedded tags or from path heuristics.
type Info struct {
	Artist      string `json:"artist,omitempty"`
	AlbumArtist string `json:"albumArtist,omitempty"`
	Album       string `json:"album,omitempty"`
	Title       string `json:"title,omitempty"`
	Genre       string `json:"genre,omitempty"`
	Year        int    `json:"year,omitempty"`
	Track       int    `json:"track,omitempty"`
	Disc        int    `json:"disc,omitempty"`
}

func (i Info) Empty() bool { return i == Info{} }

func (i Info) HasCorruptText() bool {
	for _, text := range []string{i.Artist, i.AlbumArtist, i.Album, i.Title, i.Genre} {
		if media.CorruptText(text) {
			return true
		}
	}
	return false
}

type Entry struct {
	TagEncodingVersion int      `json:"tagEncodingVersion,omitempty"`
	AudioCodecs        []string `json:"audioCodecs,omitempty"`
	VideoCodecs        []string `json:"videoCodecs,omitempty"`
	TagSource          string   `json:"tagSource,omitempty"`
	ProbeVersion       int      `json:"probeVersion,omitempty"`
	ProbeError         string   `json:"probeError,omitempty"`
	Bitrate            int64    `json:"bitrate,omitempty"`
	SampleRate         int      `json:"sampleRate,omitempty"`
	Channels           int      `json:"channels,omitempty"`
	Bits               int      `json:"bits,omitempty"`

	Name    string     `json:"name"`
	Kind    media.Kind `json:"kind"`
	Size    int64      `json:"size"`
	ModTime time.Time  `json:"mtime"`
	// Duration in seconds and codec of the first audio stream, from ffprobe.
	Duration   float64 `json:"duration,omitempty"`
	Codec      string  `json:"codec,omitempty"`
	VideoCodec string  `json:"videoCodec,omitempty"`
	// Tags is what the file itself says; absent for untagged files.
	Tags *Info `json:"tags,omitempty"`
	// Guess is derived from directory and file names.
	Guess      Info    `json:"guess"`
	Confidence float64 `json:"confidence"`
}

// Display merges tags over guesses, field by field.
func (e Entry) Display() Info {
	d := e.Guess
	if t := e.Tags; t != nil && (e.Kind != media.Video || e.TagSource == "container") {
		for _, f := range []struct {
			dst *string
			src string
		}{
			{&d.Artist, t.Artist}, {&d.AlbumArtist, t.AlbumArtist}, {&d.Album, t.Album},
			{&d.Title, t.Title}, {&d.Genre, t.Genre},
		} {
			if f.src != "" && !media.CorruptText(f.src) {
				if e.Kind == media.Video && f.dst == &d.Title && media.TechnicalVideoTitle(f.src) {
					continue
				}
				*f.dst = f.src
			}
		}
		for _, f := range []struct {
			dst *int
			src int
		}{
			{&d.Year, t.Year}, {&d.Track, t.Track}, {&d.Disc, t.Disc},
		} {
			if f.src != 0 {
				*f.dst = f.src
			}
		}
	}
	return d
}

type Sidecar struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

func Path(dir string) string { return path.Join(dir, Dir, File) }

// Read loads the sidecar of a library directory. A directory without a
// sidecar returns fs.ErrNotExist.
func Read(root *os.Root, dir string) (*Sidecar, error) {
	b, err := root.ReadFile(Path(dir))
	if err != nil {
		return nil, err
	}
	var s Sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Write replaces the sidecar atomically, so readers never see a partial file.
func Write(root *os.Root, dir string, s *Sidecar) error {
	if err := root.MkdirAll(path.Join(dir, Dir), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(dir) + ".tmp"
	if err := root.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return root.Rename(tmp, Path(dir))
}
