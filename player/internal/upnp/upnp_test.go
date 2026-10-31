package upnp

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePlayer struct {
	played  []string
	stopped int
	playing bool
	level   int
	noMixer bool
}

func (p *fakePlayer) CheckURI(uri string) error {
	if strings.Contains(uri, "bad") {
		return errors.New("rejected")
	}
	return nil
}
func (p *fakePlayer) Play(uri string) error {
	p.played = append(p.played, uri)
	p.playing = true
	return nil
}
func (p *fakePlayer) Stop()                     { p.stopped++; p.playing = false }
func (p *fakePlayer) Playing() bool             { return p.playing }
func (p *fakePlayer) Volume() (int, bool, bool) { return p.level, false, !p.noMixer }
func (p *fakePlayer) SetVolume(v int) error     { p.level = v; return nil }

// The envelope swyh-rs sends, including the escaped DIDL-Lite metadata.
func envelope(service, action, body string) string {
	return `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="urn:schemas-upnp-org:service:` + service + `:1">` + body + `</u:` + action + `></s:Body></s:Envelope>`
}

func soap(t *testing.T, url, service, action, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/upnp/"+service+"/control", strings.NewReader(envelope(service, action, body)))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:`+service+`:1#`+action+`"`)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestSwyhPlaySequence(t *testing.T) {
	p := &fakePlayer{level: 40}
	srv := httptest.NewServer(NewServer(Config{Name: "Kitchen & Co", Player: p}).Handler())
	defer srv.Close()
	uri := "http://192.0.2.5:5901/stream/swyh.raw?bd=16"
	metadata := "&lt;DIDL-Lite&gt;&lt;item&gt;&lt;res protocolInfo=&quot;http-get:*:audio/L16;rate=44100;channels=2:DLNA.ORG_PN=LPCM&quot;&gt;x&lt;/res&gt;&lt;/item&gt;&lt;/DIDL-Lite&gt;"
	if code, body := soap(t, srv.URL, "AVTransport", "Play", `<InstanceID>0</InstanceID><Speed>1</Speed>`); code != 500 || !strings.Contains(body, "<errorCode>701<") {
		t.Fatal("play before a URI:", code, body)
	}
	soap(t, srv.URL, "AVTransport", "Stop", `<InstanceID>0</InstanceID>`)
	if code, body := soap(t, srv.URL, "AVTransport", "SetAVTransportURI", `<InstanceID>0</InstanceID><CurrentURI>`+strings.ReplaceAll(uri, "&", "&amp;")+`</CurrentURI><CurrentURIMetaData>`+metadata+`</CurrentURIMetaData>`); code != 200 {
		t.Fatal(code, body)
	}
	if code, body := soap(t, srv.URL, "AVTransport", "Play", `<InstanceID>0</InstanceID><Speed>1</Speed>`); code != 200 || !strings.Contains(body, "PlayResponse") {
		t.Fatal(code, body)
	}
	if len(p.played) != 1 || p.played[0] != uri || p.stopped != 1 {
		t.Fatalf("%+v", p)
	}
	if _, body := soap(t, srv.URL, "AVTransport", "GetTransportInfo", `<InstanceID>0</InstanceID>`); !strings.Contains(body, "<CurrentTransportState>PLAYING<") {
		t.Fatal(body)
	}
	if _, body := soap(t, srv.URL, "RenderingControl", "GetVolume", `<InstanceID>0</InstanceID><Channel>Master</Channel>`); !strings.Contains(body, "<CurrentVolume>40<") || !strings.Contains(body, "GetVolumeResponse") {
		t.Fatal(body)
	}
	soap(t, srv.URL, "RenderingControl", "SetVolume", `<InstanceID>0</InstanceID><Channel>Master</Channel><DesiredVolume>55</DesiredVolume>`)
	if p.level != 55 {
		t.Fatal(p.level)
	}
	if code, _ := soap(t, srv.URL, "RenderingControl", "SetVolume", `<DesiredVolume>101</DesiredVolume>`); code != 500 {
		t.Fatal(code)
	}
	if code, body := soap(t, srv.URL, "AVTransport", "SetAVTransportURI", `<CurrentURI>http://bad/</CurrentURI>`); code != 500 || !strings.Contains(body, "<errorCode>716<") {
		t.Fatal(code, body)
	}
	if code, body := soap(t, srv.URL, "AVTransport", "Reboot", ``); code != 500 || !strings.Contains(body, "<errorCode>401<") {
		t.Fatal(code, body)
	}
	if _, body := soap(t, srv.URL, "ConnectionManager", "GetProtocolInfo", ``); !strings.Contains(body, "audio/L16") {
		t.Fatal(body)
	}
}

func TestDescriptionsAreWellFormedAndFindable(t *testing.T) {
	srv := httptest.NewServer(NewServer(Config{Name: "Kitchen & Co", Player: &fakePlayer{}}).Handler())
	defer srv.Close()
	paths := []string{"/upnp/device.xml", "/upnp/AVTransport.xml", "/upnp/RenderingControl.xml", "/upnp/ConnectionManager.xml"}
	for _, path := range paths {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(path, res.StatusCode)
		}
		decoder := xml.NewDecoder(strings.NewReader(string(b)))
		for {
			if _, err := decoder.Token(); err != nil {
				if err != io.EOF {
					t.Fatal(path, err)
				}
				break
			}
		}
		if path == "/upnp/device.xml" {
			for _, want := range []string{"Kitchen &amp; Co", "urn:upnp-org:serviceId:AVTransport", "/upnp/RenderingControl/control", deviceType} {
				if !strings.Contains(string(b), want) {
					t.Fatal(want, string(b))
				}
			}
		}
	}
	req, _ := http.NewRequest("SUBSCRIBE", srv.URL+"/upnp/AVTransport/event", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotImplemented {
		t.Fatal(res.StatusCode)
	}
}

func TestSearchMatching(t *testing.T) {
	s := NewServer(Config{Name: "x", Player: &fakePlayer{}})
	search := func(target string) []string {
		return s.searchTargets([]byte("M-SEARCH * HTTP/1.1\r\nHost: 239.255.255.250:1900\r\nMan: \"ssdp:discover\"\r\nST: " + target + "\r\nMX: 3\r\n\r\n"))
	}
	if got := search("urn:schemas-upnp-org:service:RenderingControl:1"); len(got) != 1 {
		t.Fatal("swyh-rs search not answered", got)
	}
	if got := search("ssdp:all"); len(got) != 6 {
		t.Fatal(got)
	}
	if got := search("urn:av-openhome-org:service:Product:1"); len(got) != 0 {
		t.Fatal(got)
	}
	if got := s.searchTargets([]byte("NOTIFY * HTTP/1.1\r\nST: ssdp:all\r\n\r\n")); len(got) != 0 {
		t.Fatal(got)
	}
	reply := string(s.searchReply("urn:schemas-upnp-org:service:RenderingControl:1", "http://192.0.2.1:1/upnp/device.xml"))
	for _, want := range []string{"HTTP/1.1 200 OK\r\n", "LOCATION: http://192.0.2.1:1/upnp/device.xml\r\n", "ST: urn:schemas-upnp-org:service:RenderingControl:1\r\n"} {
		if !strings.Contains(reply, want) {
			t.Fatal(want, reply)
		}
	}
	if s.uuid != NewServer(Config{Name: "x"}).uuid {
		t.Fatal("identity not stable")
	}
}
