// Package upnp is a small UPnP AV media renderer: SSDP search replies, device and
// service descriptions, and the SOAP actions that controllers such as swyh-rs use
// to start a stream. There is no authentication, eventing or playlist support, so
// it belongs on a trusted LAN only.
package upnp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	deviceType   = "urn:schemas-upnp-org:device:MediaRenderer:1"
	ssdpAddress  = "239.255.255.250:1900"
	maxSOAPBytes = 64 << 10
	maxSSDPBytes = 2048

	faultInvalidAction = 401
	faultInvalidArgs   = 402
	faultActionFailed  = 501
	faultNoContents    = 701
	faultBadURI        = 716
)

// SinkProtocols is what ConnectionManager advertises.
const SinkProtocols = "http-get:*:audio/flac:*,http-get:*:audio/x-flac:*,http-get:*:audio/wav:*," +
	"http-get:*:audio/x-wav:*,http-get:*:audio/vnd.wave:*,http-get:*:audio/L16:*,http-get:*:audio/L24:*"

// Player does the work behind the actions.
type Player interface {
	// CheckURI reports whether a stream address is acceptable.
	CheckURI(uri string) error
	Play(uri string) error
	Stop()
	Playing() bool
	// Volume returns 0..100 and mute, and false when there is no hardware control.
	Volume() (int, bool, bool)
	SetVolume(int) error
}

// Config describes the renderer.
type Config struct {
	Name   string
	Player Player
}

// Server is one renderer.
type Server struct {
	config Config
	uuid   string
	port   int

	mu  sync.Mutex
	uri string
}

// NewServer returns a renderer whose identity is stable for its name.
func NewServer(config Config) *Server {
	sum := sha256.Sum256([]byte("slopify-upnp:" + config.Name))
	h := hex.EncodeToString(sum[:16])
	return &Server{config: config, uuid: h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]}
}

// Serve answers on the HTTP listener and on the SSDP multicast group until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	s.port = l.Addr().(*net.TCPAddr).Port
	group, err := net.ResolveUDPAddr("udp4", ssdpAddress)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		return fmt.Errorf("joining SSDP group: %w", err)
	}
	go s.serveSSDP(ctx, conn)
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		conn.Close()
		server.Close()
	}()
	if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) serveSSDP(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, maxSSDPBytes)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("upnp discovery ended", "err", err)
			}
			return
		}
		targets := s.searchTargets(buf[:n])
		if len(targets) == 0 {
			continue
		}
		ip, err := localIP(peer)
		if err != nil {
			continue
		}
		location := "http://" + net.JoinHostPort(ip, strconv.Itoa(s.port)) + "/upnp/device.xml"
		for _, target := range targets {
			conn.WriteToUDP(s.searchReply(target, location), peer)
		}
	}
}

// localIP finds the address this host uses to reach a peer. Nothing is sent.
func localIP(peer *net.UDPAddr) (string, error) {
	conn, err := net.DialUDP("udp4", nil, peer)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// searchTargets returns the search targets a request should be answered with.
func (s *Server) searchTargets(request []byte) []string {
	lines := strings.Split(string(request), "\n")
	if !strings.HasPrefix(strings.ToUpper(lines[0]), "M-SEARCH ") {
		return nil
	}
	target, discover := "", false
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToUpper(strings.TrimSpace(name)) {
		case "ST":
			target = value
		case "MAN":
			discover = strings.Trim(value, `"`) == "ssdp:discover"
		}
	}
	if !discover {
		return nil
	}
	all := []string{"upnp:rootdevice", "uuid:" + s.uuid, deviceType}
	for _, service := range services {
		all = append(all, service.kind())
	}
	if target == "ssdp:all" {
		return all
	}
	for _, known := range all {
		if target == known {
			return []string{target}
		}
	}
	return nil
}

func (s *Server) searchReply(target, location string) []byte {
	usn := "uuid:" + s.uuid
	if target != usn {
		usn += "::" + target
	}
	return []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\nLOCATION: " + location +
		"\r\nSERVER: Linux UPnP/1.0 Slopify/1.0\r\nST: " + target + "\r\nUSN: " + usn + "\r\n\r\n")
}

type action struct {
	name    string
	in, out []string
}

type service struct {
	name    string
	actions []action
}

func (v service) kind() string { return "urn:schemas-upnp-org:service:" + v.name + ":1" }

var services = []service{
	{"AVTransport", []action{
		{"SetAVTransportURI", []string{"InstanceID", "CurrentURI", "CurrentURIMetaData"}, nil},
		{"Play", []string{"InstanceID", "Speed"}, nil},
		{"Stop", []string{"InstanceID"}, nil},
		{"GetTransportInfo", []string{"InstanceID"}, []string{"CurrentTransportState", "CurrentTransportStatus", "CurrentSpeed"}},
		{"GetMediaInfo", []string{"InstanceID"}, []string{"NrTracks", "MediaDuration", "CurrentURI", "CurrentURIMetaData", "NextURI", "NextURIMetaData", "PlayMedium", "RecordMedium", "WriteStatus"}},
		{"GetPositionInfo", []string{"InstanceID"}, []string{"Track", "TrackDuration", "TrackMetaData", "TrackURI", "RelTime", "AbsTime", "RelCount", "AbsCount"}},
		{"GetCurrentTransportActions", []string{"InstanceID"}, []string{"Actions"}},
	}},
	{"RenderingControl", []action{
		{"GetVolume", []string{"InstanceID", "Channel"}, []string{"CurrentVolume"}},
		{"SetVolume", []string{"InstanceID", "Channel", "DesiredVolume"}, nil},
		{"GetMute", []string{"InstanceID", "Channel"}, []string{"CurrentMute"}},
	}},
	{"ConnectionManager", []action{
		{"GetProtocolInfo", nil, []string{"Source", "Sink"}},
		{"GetCurrentConnectionIDs", nil, []string{"ConnectionIDs"}},
		{"GetCurrentConnectionInfo", []string{"ConnectionID"}, []string{"RcsID", "AVTransportID", "ProtocolInfo", "PeerConnectionManager", "PeerConnectionID", "Direction", "Status"}},
	}},
}

// Handler serves descriptions and control.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /upnp/device.xml", s.serveDevice)
	for _, svc := range services {
		mux.HandleFunc("GET /upnp/"+svc.name+".xml", func(w http.ResponseWriter, r *http.Request) { serveDescription(w, svc) })
		mux.HandleFunc("POST /upnp/"+svc.name+"/control", func(w http.ResponseWriter, r *http.Request) { s.serveControl(w, r, svc) })
		// There is no eventing, which controllers must cope with.
		mux.HandleFunc("/upnp/"+svc.name+"/event", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "events are not implemented", http.StatusNotImplemented)
		})
	}
	return mux
}

func escape(text string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(text))
	return b.String()
}

func writeXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(status)
	io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+body)
}

func (s *Server) serveDevice(w http.ResponseWriter, _ *http.Request) {
	var list strings.Builder
	for _, svc := range services {
		fmt.Fprintf(&list, "<service><serviceType>%s</serviceType><serviceId>urn:upnp-org:serviceId:%s</serviceId>"+
			"<SCPDURL>/upnp/%s.xml</SCPDURL><controlURL>/upnp/%s/control</controlURL><eventSubURL>/upnp/%s/event</eventSubURL></service>",
			svc.kind(), svc.name, svc.name, svc.name, svc.name)
	}
	writeXML(w, http.StatusOK, fmt.Sprintf(`<root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion>`+
		`<device><deviceType>%s</deviceType><friendlyName>%s</friendlyName><manufacturer>Slopify</manufacturer>`+
		`<manufacturerURL>https://github.com/codemowers/slopify</manufacturerURL><modelName>Slopify speaker</modelName>`+
		`<UDN>uuid:%s</UDN><serviceList>%s</serviceList></device></root>`, deviceType, escape(s.config.Name), s.uuid, list.String()))
}

func serveDescription(w http.ResponseWriter, svc service) {
	var actions, variables strings.Builder
	seen := map[string]bool{}
	argument := func(name, direction string) {
		fmt.Fprintf(&actions, "<argument><name>%s</name><direction>%s</direction><relatedStateVariable>A_ARG_TYPE_%s</relatedStateVariable></argument>", name, direction, name)
		if !seen[name] {
			seen[name] = true
			fmt.Fprintf(&variables, `<stateVariable sendEvents="no"><name>A_ARG_TYPE_%s</name><dataType>string</dataType></stateVariable>`, name)
		}
	}
	for _, a := range svc.actions {
		fmt.Fprintf(&actions, "<action><name>%s</name><argumentList>", a.name)
		for _, name := range a.in {
			argument(name, "in")
		}
		for _, name := range a.out {
			argument(name, "out")
		}
		actions.WriteString("</argumentList></action>")
	}
	writeXML(w, http.StatusOK, `<scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><actionList>`+
		actions.String()+`</actionList><serviceStateTable>`+variables.String()+`</serviceStateTable></scpd>`)
}

// parseAction reads a SOAP envelope: the first element inside Body and its fields.
func parseAction(r io.Reader) (string, map[string]string, error) {
	decoder := xml.NewDecoder(r)
	inBody, name := false, ""
	args := map[string]string{}
	field, value := "", ""
	for {
		token, err := decoder.Token()
		if err != nil {
			if name != "" && errors.Is(err, io.EOF) {
				return name, args, nil
			}
			return "", nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch {
			case !inBody:
				inBody = t.Name.Local == "Body"
			case name == "":
				name = t.Name.Local
			case field == "":
				field, value = t.Name.Local, ""
			}
		case xml.CharData:
			if field != "" {
				value += string(t)
			}
		case xml.EndElement:
			if field == t.Name.Local {
				args[field] = value
				field = ""
			} else if name == t.Name.Local {
				return name, args, nil
			}
		}
	}
}

func (s *Server) serveControl(w http.ResponseWriter, r *http.Request, svc service) {
	name, args, err := parseAction(http.MaxBytesReader(w, r.Body, maxSOAPBytes))
	if err != nil {
		http.Error(w, "invalid SOAP request", http.StatusBadRequest)
		return
	}
	var known *action
	for i := range svc.actions {
		if svc.actions[i].name == name {
			known = &svc.actions[i]
		}
	}
	if known == nil {
		writeFault(w, faultInvalidAction, "Invalid Action")
		return
	}
	result, code, err := s.perform(name, args)
	if err != nil {
		writeFault(w, code, err.Error())
		return
	}
	var fields strings.Builder
	for _, out := range known.out {
		fmt.Fprintf(&fields, "<%s>%s</%s>", out, escape(result[out]), out)
	}
	writeXML(w, http.StatusOK, fmt.Sprintf(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`+
		`<u:%sResponse xmlns:u="%s">%s</u:%sResponse></s:Body></s:Envelope>`, name, svc.kind(), fields.String(), name))
}

func writeFault(w http.ResponseWriter, code int, description string) {
	writeXML(w, http.StatusInternalServerError, fmt.Sprintf(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault>`+
		`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`+
		`<errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, escape(description)))
}

func (s *Server) perform(name string, args map[string]string) (map[string]string, int, error) {
	player := s.config.Player
	s.mu.Lock()
	uri := s.uri
	s.mu.Unlock()
	switch name {
	case "SetAVTransportURI":
		next := args["CurrentURI"]
		if err := player.CheckURI(next); err != nil {
			return nil, faultBadURI, err
		}
		s.mu.Lock()
		s.uri = next
		s.mu.Unlock()
	case "Play":
		if uri == "" {
			return nil, faultNoContents, errors.New("No contents")
		}
		if err := player.Play(uri); err != nil {
			return nil, faultActionFailed, err
		}
	case "Stop":
		player.Stop()
	case "GetTransportInfo":
		state := "NO_MEDIA_PRESENT"
		if player.Playing() {
			state = "PLAYING"
		} else if uri != "" {
			state = "STOPPED"
		}
		return map[string]string{"CurrentTransportState": state, "CurrentTransportStatus": "OK", "CurrentSpeed": "1"}, 0, nil
	case "GetMediaInfo":
		return map[string]string{"NrTracks": "1", "MediaDuration": "NOT_IMPLEMENTED", "CurrentURI": uri, "NextURI": "NOT_IMPLEMENTED",
			"NextURIMetaData": "NOT_IMPLEMENTED", "PlayMedium": "NETWORK", "RecordMedium": "NOT_IMPLEMENTED", "WriteStatus": "NOT_IMPLEMENTED"}, 0, nil
	case "GetPositionInfo":
		return map[string]string{"Track": "1", "TrackDuration": "NOT_IMPLEMENTED", "TrackMetaData": "NOT_IMPLEMENTED", "TrackURI": uri,
			"RelTime": "NOT_IMPLEMENTED", "AbsTime": "NOT_IMPLEMENTED", "RelCount": "2147483647", "AbsCount": "2147483647"}, 0, nil
	case "GetCurrentTransportActions":
		return map[string]string{"Actions": "Play,Stop"}, 0, nil
	case "GetVolume", "GetMute":
		level, muted, ok := player.Volume()
		if !ok {
			return nil, faultActionFailed, errors.New("No hardware volume")
		}
		mute := "0"
		if muted {
			mute = "1"
		}
		return map[string]string{"CurrentVolume": strconv.Itoa(level), "CurrentMute": mute}, 0, nil
	case "SetVolume":
		level, err := strconv.Atoi(args["DesiredVolume"])
		if err != nil || level < 0 || level > 100 {
			return nil, faultInvalidArgs, errors.New("Invalid Args")
		}
		if err := player.SetVolume(level); err != nil {
			return nil, faultActionFailed, err
		}
	case "GetProtocolInfo":
		return map[string]string{"Source": "", "Sink": SinkProtocols}, 0, nil
	case "GetCurrentConnectionIDs":
		return map[string]string{"ConnectionIDs": "0"}, 0, nil
	case "GetCurrentConnectionInfo":
		return map[string]string{"RcsID": "0", "AVTransportID": "0", "ProtocolInfo": "", "PeerConnectionManager": "", "PeerConnectionID": "-1", "Direction": "Input", "Status": "OK"}, 0, nil
	}
	return nil, 0, nil
}
