package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codemowers/slopify/internal/volume"
)

func TestLocalVolumeRequiresSameOriginAndBounds(t *testing.T) {
	device := &fakeMixer{}
	c := &controller{mixer: &mixer{control: volume.Control{Name: "Digital", Min: 0, Max: 40, Level: 20}, switchAvailable: true, device: device}}
	for _, test := range []struct {
		origin, body string
		status       int
	}{
		{"http://attacker.invalid", `{"muted":true}`, 403},
		{"http://speaker.local", `{"level":41}`, 400},
		{"http://speaker.local", `{"muted":true,"unknown":1}`, 400},
		{"http://speaker.local", `{"muted":true}{}`, 400},
		{"http://speaker.local", `{"muted":true}`, 200},
	} {
		r := httptest.NewRequest("POST", "http://speaker.local/api/volume", strings.NewReader(test.body))
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slopify-Control", "1")
		w := httptest.NewRecorder()
		c.localVolumeCommand(w, r)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.body, w.Code, w.Body.String())
		}
	}
	if device.writes != 1 || !c.mixer.switchMuted || c.mixer.control.Level != 20 {
		t.Fatal("mute must switch hardware without changing gain")
	}
}
