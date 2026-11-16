package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/codemowers/slopify/player/internal/rtp"
)

func TestLocalMulticastSDPUnavailable(t *testing.T) {
	c := &controller{}
	for _, status := range []int{404, 503} {
		w := httptest.NewRecorder()
		c.localMulticastSDP(w, httptest.NewRequest("GET", "/multicast.sdp", nil))
		if w.Code != status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d: %d %s", status, w.Code, w.Body.String())
		}
		c.rtpSender = rtp.NewSender(rtp.SenderConfig{Group: netip.MustParseAddrPort("239.192.1.2:5004")})
	}
}

func TestRTPCollidingGroupsRemainIndividuallySelectable(t *testing.T) {
	sources := newSpeakerSources("")
	group := netip.MustParseAddr("239.192.12.34")
	keys := map[string]bool{}
	for i, ip := range []string{"192.168.1.5", "192.168.1.6"} {
		stream := rtpLive{&rtp.Stream{Description: rtp.Description{Name: "Same name", Group: group, Port: 5004}, Peer: ip}}
		key, name, _ := liveSenderKey(stream)
		if keys[key] {
			t.Fatal("different senders merged")
		}
		keys[key] = true
		stream.Name = "Renamed sender"
		stream.Port = 5006
		if changed, _, _ := liveSenderKey(stream); changed != key {
			t.Fatal("name or port changed remembered identity")
		}
		sources.Sources = append(sources.Sources, &speakerSource{ID: fmt.Sprintf("lan_%d", i+1), Key: key, Name: name, IP: ip, Protocol: "rtp", Connected: true})
	}
	c := &controller{sources: sources}
	// Exercise the same revision-checked selection API used by the web cards.
	for _, id := range []string{"lan_1", "lan_2", "lan_1"} {
		body := fmt.Sprintf(`{"revision":%d,"selected":%q}`, sources.Revision, id)
		req := httptest.NewRequest("POST", "http://speaker.local/api/sources", strings.NewReader(body))
		req.Header.Set("Origin", "http://speaker.local")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Slopify-Control", "1")
		response := httptest.NewRecorder()
		c.localSourceCommand(response, req)
		if response.Code != 204 || sources.Selected != id || sources.Mode != "off" {
			t.Fatalf("select %s: %d %s", id, response.Code, response.Body.String())
		}
	}
	data, err := json.Marshal(c.sourceSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`"id":"lan_1"`, `"id":"lan_2"`, `"ip":"192.168.1.5"`, `"ip":"192.168.1.6"`} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("UI status missing %s", text)
		}
	}
}

func TestSourceReorderEveryMode(t *testing.T) {
	for _, mode := range []string{"off", "recent", "priority"} {
		t.Run(mode, func(t *testing.T) {
			sources := newSpeakerSources("")
			sources.Mode = mode
			sources.Selected = "slopify"
			sources.Sources = append(sources.Sources, &speakerSource{ID: "lan_1", Name: "Laptop"})
			c := &controller{sources: sources}
			body := fmt.Sprintf(`{"revision":%d,"order":["lan_1","slopify"]}`, sources.Revision)
			req := httptest.NewRequest("POST", "http://speaker.local/api/sources", strings.NewReader(body))
			req.Header.Set("Origin", "http://speaker.local")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Slopify-Control", "1")
			w := httptest.NewRecorder()
			c.localSourceCommand(w, req)
			if w.Code != 204 || sources.Sources[0].ID != "lan_1" || sources.Mode != mode || sources.Selected != "slopify" {
				t.Fatalf("reorder changed mode/selection: %d %+v", w.Code, sources)
			}
		})
	}
}
func TestLocalServerLinkStripsSecrets(t *testing.T) {
	c := &controller{local: &localStatus{}, server: "https://user:secret@slopify.codemowers.io:8443/private?token=secret#fragment"}
	data, err := json.Marshal(c.localSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var status struct{ ServerOrigin, ServerHost string }
	if err = json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if status.ServerOrigin != "https://slopify.codemowers.io:8443" || status.ServerHost != "slopify.codemowers.io" || strings.Contains(string(data), "secret") {
		t.Fatalf("unsafe link: %s", data)
	}
}

func TestReorderReceiverOnlyPage(t *testing.T) {
	s := newSpeakerSources("")
	s.Sources = []*speakerSource{{ID: "lan_1"}, {ID: "slopify"}, {ID: "lan_2"}}
	c := &controller{sources: s}
	req := httptest.NewRequest("POST", "http://speaker.local/api/sources", strings.NewReader(fmt.Sprintf(`{"revision":%d,"order":["lan_2","lan_1"]}`, s.Revision)))
	req.Header.Set("Origin", "http://speaker.local")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slopify-Control", "1")
	w := httptest.NewRecorder()
	c.localSourceCommand(w, req)
	if w.Code != 204 || s.Sources[0].ID != "lan_2" || s.Sources[1].ID != "slopify" || s.Sources[2].ID != "lan_1" {
		t.Fatalf("visible reorder failed: %d %s", w.Code, w.Body.String())
	}
}
