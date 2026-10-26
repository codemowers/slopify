package heuristics

import (
	"testing"

	"github.com/codemowers/slopify/internal/sidecar"
)

func TestGuess(t *testing.T) {
	for _, tc := range []struct {
		path string
		want sidecar.Info
	}{
		{
			"8BP050 Blip Festival/cd3/8bp050iii-01-chibi-tech-oniichan_dakara_iiyo.mp3",
			sidecar.Info{Artist: "Chibi-Tech", Album: "8BP050 Blip Festival", Title: "Oniichan Dakara Iiyo", Track: 1, Disc: 3},
		},
		{
			"Radiohead/1997 - OK Computer/02 - Paranoid Android.flac",
			sidecar.Info{Artist: "Radiohead", AlbumArtist: "Radiohead", Album: "OK Computer", Year: 1997, Title: "Paranoid Android", Track: 2},
		},
		{
			"Daft Punk - Discovery (2001)/03. Digital Love.flac",
			sidecar.Info{Artist: "Daft Punk", AlbumArtist: "Daft Punk", Album: "Discovery", Year: 2001, Title: "Digital Love", Track: 3},
		},
		{
			"Compilations/Best Of/Disc 2/1-05 Massive Attack - Teardrop.mp3",
			sidecar.Info{Artist: "Massive Attack", AlbumArtist: "Compilations", Album: "Best Of", Title: "Teardrop", Track: 5, Disc: 1},
		},
		{
			"loose/Some Song.mp3",
			sidecar.Info{Artist: "", Album: "Loose", Title: "Some Song"},
		},
		{
			"untitled.mp3",
			sidecar.Info{Title: "Untitled"},
		},
	} {
		got, _ := Guess(tc.path)
		if got != tc.want {
			t.Errorf("Guess(%q)\n got %+v\nwant %+v", tc.path, got, tc.want)
		}
	}
}
