package audiodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// fixture encodes noise with ffmpeg, the reference decoder for these tests.
func fixture(t *testing.T, name string, rate int, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), name)
	cmd := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
		"anoisesrc=d=3:c=pink:a=0.5:r=" + strconv.Itoa(rate), "-ac", "2"}
	if out, err := exec.Command("ffmpeg", append(append(cmd, args...), path)...).CombinedOutput(); err != nil {
		t.Fatalf("encoding: %v: %s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func reference(t *testing.T, data []byte, name string) []int32 {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path, "-f", "s32le", "-c:a", "pcm_s32le", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]int32, len(out)/4)
	binary.Read(bytes.NewReader(out), binary.LittleEndian, samples)
	return samples
}

func decodeAll(t *testing.T, d Decoder) []int32 {
	t.Helper()
	var all []int32
	buf := make([]int32, 3000)
	for {
		n, err := d.Read(buf)
		all = append(all, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLosslessMatchesReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate int
		bits int
		args []string
	}{
		{"16.flac", 44100, 16, []string{"-sample_fmt", "s16"}},
		{"24.flac", 96000, 24, []string{"-sample_fmt", "s32", "-bits_per_raw_sample", "24"}},
		{"16.wav", 44100, 16, []string{"-c:a", "pcm_s16le"}},
		{"24.wav", 48000, 24, []string{"-c:a", "pcm_s24le"}},
		{"32.wav", 192000, 32, []string{"-c:a", "pcm_s32le"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixture(t, tc.name, tc.rate, tc.args...)
			d, err := Open(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if f := d.Format(); f.Rate != tc.rate || f.Channels != 2 || f.Bits != tc.bits {
				t.Fatalf("format %+v", f)
			}
			got, want := decodeAll(t, d), reference(t, data, tc.name)
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("sample %d: %#x, want %#x", i, got[i], want[i])
				}
			}
		})
	}
}

func TestSeekMatchesReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"seek.flac", []string{"-sample_fmt", "s16"}},
		{"seek.wav", []string{"-c:a", "pcm_s24le"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixture(t, tc.name, 44100, tc.args...)
			d, err := Open(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			want := reference(t, data, tc.name)[44100*2*3/2:]
			landed, err := d.Seek(1.5)
			if err != nil || landed != 1.5 {
				t.Fatalf("landed %v: %v", landed, err)
			}
			got := decodeAll(t, d)
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("sample %d differs", i)
				}
			}
		})
	}
}

func TestMP3MatchesReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate int
		args []string
	}{
		{"44-cbr.mp3", 44100, []string{"-b:a", "192k"}},
		{"44-vbr.mp3", 44100, []string{"-q:a", "2"}},
		{"48-320.mp3", 48000, []string{"-b:a", "320k"}},
		{"32-128.mp3", 32000, []string{"-b:a", "128k"}},
		{"44-mono.mp3", 44100, []string{"-b:a", "128k", "-ac", "1"}},
		{"22-mpeg2.mp3", 22050, []string{"-b:a", "64k"}},
		{"24-mpeg2.mp3", 24000, []string{"-b:a", "64k"}},
		{"16-mpeg2.mp3", 16000, []string{"-b:a", "48k"}},
		{"12-mpeg25.mp3", 12000, []string{"-b:a", "32k"}},
		{"8-mpeg25.mp3", 8000, []string{"-b:a", "24k"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixture(t, tc.name, tc.rate, append([]string{"-c:a", "libmp3lame"}, tc.args...)...)
			d, err := Open(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if f := d.Format(); f.Rate != tc.rate || f.Bits != mp3Precision {
				t.Fatalf("format %+v", f)
			}
			got, want := decodeAll(t, d), reference(t, data, tc.name)
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d (gapless trim)", len(got), len(want))
			}
			worst, sum := mp3Compare(got, want)
			rms := math.Sqrt(sum / float64(len(got)))
			t.Logf("worst %.1f, RMS %.3f 16-bit LSB against the float reference", worst, rms)
			// ISO 11172-4 full accuracy: RMS below 2^-15/sqrt(12), 0.58 16-bit LSB.
			if rms > 0.58 {
				t.Fatalf("RMS %.3f 16-bit LSB", rms)
			}
		})
	}
}

// Clicks force the encoder into short and mixed-transition blocks.
// Every variant holds the same audio, however it is wrapped, and tags are ignored.
func TestMP3FindsAudioPastTags(t *testing.T) {
	data := fixture(t, "x.mp3", 44100, "-c:a", "libmp3lame", "-b:a", "128k", "-id3v2_version", "0")
	base, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := decodeAll(t, base)
	tag := func(size int, fill byte) []byte {
		return append([]byte{'I', 'D', '3', 3, 0, 0, byte(size >> 21 & 0x7f), byte(size >> 14 & 0x7f), byte(size >> 7 & 0x7f), byte(size & 0x7f)}, bytes.Repeat([]byte{fill}, size)...)
	}
	junk := bytes.Repeat([]byte{0xff, 0x00, 0x12, 0xe0}, 300) // Includes false syncs.
	for name, prefix := range map[string][]byte{
		"plain":          nil,
		"tag":            tag(4096, 0),
		"tag with sync":  tag(4096, 0xff),
		"two tags":       append(tag(100, 0), tag(200, 'x')...),
		"padding":        append(tag(100, 0), make([]byte, 5000)...),
		"junk after tag": append(tag(100, 0), junk...),
		"junk, no tag":   junk,
		"large tag":      tag(300000, 'x'),
		"bad tag size":   append([]byte{'I', 'D', '3', 3, 0, 0, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)...),
	} {
		t.Run(name, func(t *testing.T) {
			d, err := Open(bytes.NewReader(append(bytes.Clone(prefix), data...)))
			if err != nil {
				t.Fatal(err)
			}
			if f := d.Format(); f.Codec != "MP3" {
				t.Fatalf("%+v", f)
			}
			got := decodeAll(t, d)
			if len(got) != len(want) {
				t.Fatalf("%d samples, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("sample %d differs", i)
				}
			}
			if _, err := d.Seek(1); err != nil {
				t.Fatal(err)
			}
			if n, err := d.Read(make([]int32, 1024)); n == 0 || err != nil {
				t.Fatalf("after seek: %d, %v", n, err)
			}
		})
	}
}

func TestMP3Transients(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, rate := range []string{"44100", "22050"} {
		path := filepath.Join(t.TempDir(), "t.mp3")
		src := "aevalsrc='if(lt(mod(t,0.15),0.002),0.9*random(0)-0.45,0.001*sin(2*PI*440*t))|if(lt(mod(t+0.05,0.2),0.002),0.9*random(1)-0.45,0.001*sin(2*PI*660*t))':d=3:s=" + rate
		if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", src, "-c:a", "libmp3lame", "-b:a", "128k", path).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Open(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		m := d.(*mp3Decoder)
		var short int
		var got []int32
		buf := make([]int32, 2304)
		for {
			n, err := d.Read(buf)
			got = append(got, buf[:n]...)
			if m.gr[0][0].short || m.gr[1][0].short {
				short++
			}
			if err != nil {
				break
			}
		}
		want := reference(t, data, "t.mp3")
		if len(got) != len(want) {
			t.Fatalf("%d samples, want %d", len(got), len(want))
		}
		worst, sum := mp3Compare(got, want)
		rms := math.Sqrt(sum / float64(len(got)))
		t.Logf("%s Hz: %d reads saw short blocks, worst %.2f, RMS %.3f 16-bit LSB", rate, short, worst, rms)
		if short == 0 {
			t.Fatal("fixture produced no short blocks")
		}
		if rms > 0.58 {
			t.Fatalf("RMS %.3f", rms)
		}
	}
}

func mp3Compare(got, want []int32) (worst, sum float64) {
	for i := range got {
		e := math.Abs(float64(got[i])-float64(want[i])) / 65536
		worst = math.Max(worst, e)
		sum += e * e
	}
	return worst, sum
}

// countingReader measures how much of a file a seek needs.
type countingReader struct {
	*bytes.Reader
	read int64
}

func (r *countingReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.read += int64(n)
	return n, err
}

// Noise is unique everywhere, so the true landing point can be measured.
func TestMP3SeekLandsWhereItSays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		tolerance float64
	}{
		{"cbr", []string{"-b:a", "192k"}, 0.1},
		{"vbr", []string{"-q:a", "2"}, 1.0},
		{"no-tag", []string{"-b:a", "192k", "-write_xing", "0"}, 0.1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath("ffmpeg"); err != nil {
				t.Skip("ffmpeg unavailable")
			}
			path := filepath.Join(t.TempDir(), "s.mp3")
			args := append([]string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "anoisesrc=d=60:c=pink:a=0.5:r=44100", "-ac", "2", "-c:a", "libmp3lame"}, tc.args...)
			if out, err := exec.Command("ffmpeg", append(args, path)...).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			base, err := Open(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			all := decodeAll(t, base)
			for _, target := range []float64{1, 7.5, 30, 45.25, 58} {
				r := &countingReader{Reader: bytes.NewReader(data)}
				d, err := Open(r)
				if err != nil {
					t.Fatal(err)
				}
				r.read = 0
				landed, err := d.Seek(target)
				if err != nil {
					t.Fatal(err)
				}
				buf := make([]int32, 4096)
				for filled := 0; filled < len(buf); { // Read may return less than asked.
					n, err := d.Read(buf[filled:])
					if err != nil {
						t.Fatal(err)
					}
					filled += n
				}
				truth := -1
				// Search for this audio near the target. Late samples have settled.
				probe := buf[2048:2096]
				from := max(int((target-5)*44100)*2, 0)
				for i := from; i+len(probe) < len(all) && i < from+20*44100; i += 2 {
					match := true
					for j, v := range probe {
						if d := int64(all[i+j]) - int64(v); d > 1<<20 || d < -(1<<20) {
							match = false
							break
						}
					}
					if match {
						truth = i - 2048
						break
					}
				}
				if truth < 0 {
					t.Fatalf("seek to %g: audio not found near the target", target)
				}
				actual := float64(truth/2) / 44100
				t.Logf("target %.2f: reported %.3f, actual %.3f, error %.3f s, read %d KB of %d", target, landed, actual, landed-actual, r.read/1024, len(data)/1024)
				if math.Abs(landed-actual) > tc.tolerance {
					t.Fatalf("reported %.3f, actual %.3f", landed, actual)
				}
				if math.Abs(actual-target) > 2 {
					t.Fatalf("landed %.3f for target %.2f", actual, target)
				}
				if r.read > int64(len(data))/4+256<<10 {
					t.Fatalf("seek read %d of %d bytes", r.read, len(data))
				}
			}
		})
	}
}

func TestMP3SurvivesCorruption(t *testing.T) {
	data := fixture(t, "bad.mp3", 44100, "-c:a", "libmp3lame", "-b:a", "128k")
	rng := rand.New(rand.NewSource(1))
	buf := make([]int32, 4096)
	for round := 0; round < 40; round++ {
		bad := bytes.Clone(data)
		for i := 0; i < 1+round*20; i++ {
			bad[rng.Intn(len(bad))] = byte(rng.Intn(256))
		}
		d, err := Open(bytes.NewReader(bad))
		if err != nil {
			continue
		}
		for {
			if _, err := d.Read(buf); err != nil {
				break
			}
		}
		d.Seek(1)
		d.Read(buf)
	}
}

func FuzzMP3(f *testing.F) {
	f.Add([]byte{0xff, 0xfb, 0x90, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := Open(bytes.NewReader(data))
		if err != nil {
			return
		}
		buf := make([]int32, 2048)
		for i := 0; i < 64; i++ {
			if _, err := d.Read(buf); err != nil {
				return
			}
		}
	})
}

func BenchmarkMP3(b *testing.B) {
	path := filepath.Join(b.TempDir(), "b.mp3")
	if file := os.Getenv("AUDIODEC_BENCH_MP3"); file != "" {
		path = file
	} else if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "anoisesrc=d=10:c=pink:r=44100", "-ac", "2", "-c:a", "libmp3lame", "-b:a", "320k", path).CombinedOutput(); err != nil {
		b.Skipf("%v: %s", err, out)
	}
	data, _ := os.ReadFile(path)
	buf := make([]int32, 8192)
	for b.Loop() {
		d, err := Open(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := d.Read(buf); err != nil {
				break
			}
		}
	}
}

func TestRejectsOtherContent(t *testing.T) {
	for _, in := range [][]byte{[]byte("OggS, nothing useful here"), []byte("x"), append([]byte("ID3\x04\x00\x00\xff\xff\xff\xff"), make([]byte, 32)...)} {
		if _, err := Open(bytes.NewReader(in)); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

func BenchmarkFLAC24(b *testing.B) {
	path := filepath.Join(b.TempDir(), "b.flac")
	if file := os.Getenv("AUDIODEC_BENCH_FLAC"); file != "" {
		path = file
	} else if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "anoisesrc=d=10:c=pink:r=96000", "-ac", "2", "-sample_fmt", "s32", "-bits_per_raw_sample", "24", path).CombinedOutput(); err != nil {
		b.Skipf("%v: %s", err, out)
	}
	data, _ := os.ReadFile(path)
	buf := make([]int32, 8192)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		d, err := Open(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := d.Read(buf); err != nil {
				break
			}
		}
	}
}
