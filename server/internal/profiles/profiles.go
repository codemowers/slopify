// Package profiles defines ingress-visible, bitrate-specific stream profiles.
package profiles

import "slices"

type Profile struct {
	Name, Codec, MIME string
	Bitrate           int
	Video             bool
	Args              []string
}

var All = []Profile{
	{Name: "video-copy", Codec: "copy", MIME: "video/mp4", Video: true, Args: []string{"-c:v", "copy", "-c:a", "copy", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "-flush_packets", "1"}},
	{Name: "video-aac-192", Codec: "aac", MIME: "video/mp4", Bitrate: 192000, Video: true, Args: []string{"-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-ac", "2", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "-flush_packets", "1"}},
	{Name: "opus-128", Codec: "opus", MIME: "audio/ogg", Bitrate: 128000, Args: []string{"-c:a", "libopus", "-b:a", "128k", "-f", "ogg", "-flush_packets", "1"}},
	{Name: "mp3-192", Codec: "mp3", MIME: "audio/mpeg", Bitrate: 192000, Args: []string{"-c:a", "libmp3lame", "-b:a", "192k", "-f", "mp3", "-flush_packets", "1"}},
}

func Normalize(name string) string {
	switch name {
	case "", "original":
		return "original"
	case "opus":
		return "opus-128"
	case "mp3":
		return "mp3-192"
	}
	return name
}
func Lookup(name string) (Profile, bool) {
	name = Normalize(name)
	for _, p := range All {
		if p.Name == name {
			p.Args = slices.Clone(p.Args)
			return p, true
		}
	}
	return Profile{}, false
}
func Path(name string) string { return "/stream/" + Normalize(name) }
