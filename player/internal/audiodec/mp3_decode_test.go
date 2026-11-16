//go:build !no_mp3

package audiodec

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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
