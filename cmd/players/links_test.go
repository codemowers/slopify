package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codemowers/slopify/internal/signedurl"
)

func newLinkFixture(t *testing.T) (*store, string, string, *http.ServeMux) {
	t.Helper()
	s := queueStore()
	s.signer = signedurl.New("test-key")
	w := httptest.NewRecorder()
	s.createLink(w, httptest.NewRequest("POST", "/api/players/links", strings.NewReader(`{"name":"Kitchen"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var result struct{ ID, URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	_, fragment, _ := strings.Cut(result.URL, "#")
	id, token, _ := strings.Cut(fragment, ".")
	if id != result.ID || len(token) != 43 {
		t.Fatalf("invalid capability link: %s", result.URL)
	}
	mux := http.NewServeMux()
	s.registerLinkRoutes(mux)
	return s, id, token, mux
}

func linkRequest(mux *http.ServeMux, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Slopify-Receiver", strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestOutputLinkCapabilityAndStreamIsolation(t *testing.T) {
	s, id, token, mux := newLinkFixture(t)
	p := s.players[id]
	p.Queue = []Entry{{ID: 1, Path: "one.mp3", Duration: 120}, {ID: 2, Path: "two.mp3"}}
	p.Current, p.State, p.UpdatedAt = 1, Playing, time.Now()
	for _, tc := range []struct {
		path, token string
		code        int
	}{
		{"/output-api/" + id + "/stream?entry=1", "wrong", 401},
		{"/output-api/link-other/stream?entry=1", token, 401},
		{"/output-api/" + id + "/stream?entry=2", token, 403},
		{"/output-api/" + id + "/stream?entry=1&format=video-aac-192", token, 400},
		{"/output-api/" + id + "/stream?entry=1&start=NaN", token, 400},
	} {
		w := linkRequest(mux, "GET", tc.path, tc.token, "")
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	w := linkRequest(mux, "GET", "/output-api/"+id+"/stream?entry=1&path=private.mp3", token, "")
	if w.Code != 200 {
		t.Fatalf("stream: %d %s", w.Code, w.Body)
	}
	var stream struct{ URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &stream); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(stream.URL)
	if err != nil || u.Query().Get("path") != "one.mp3" || s.signer.Verify(u.Query()) != nil {
		t.Fatalf("unexpected signed stream: %s", stream.URL)
	}
	for _, path := range []string{"/output-api/" + id + "/command", "/output-api/" + id + "/library"} {
		if got := linkRequest(mux, "POST", path, token, `{"op":"clear"}`).Code; got != 404 {
			t.Fatalf("command exposed: %s %d", path, got)
		}
	}
	for _, public := range []any{json.RawMessage(s.marshal(p)), s.linkSnapshot(id, p.LinkHash)} {
		b, _ := json.Marshal(public)
		if strings.Contains(string(b), p.LinkHash) || strings.Contains(string(b), token) {
			t.Fatal("public response leaked capability")
		}
	}
}

func TestOutputLinkReportsCannotPauseSeekOrSkip(t *testing.T) {
	s, id, token, mux := newLinkFixture(t)
	p := s.players[id]
	p.reportReceiver = strings.Repeat("a", 32)
	p.receivers = map[string]*linkReceiver{p.reportReceiver: {cancel: func() {}}}
	p.Queue = []Entry{{ID: 1, Path: "one.mp3", Duration: 120}, {ID: 2, Path: "two.mp3"}}
	p.Current, p.State, p.UpdatedAt = 1, Playing, time.Now()
	w := linkRequest(mux, "POST", "/output-api/"+id+"/report", token, `{"entry":1,"version":0,"position":0,"state":"paused","ended":true}`)
	if w.Code != 204 || p.State != Playing || p.Current != 1 {
		t.Fatalf("report changed playback: %d %+v", w.Code, p)
	}
	w = linkRequest(mux, "POST", "/output-api/"+id+"/report", token, `{"entry":1,"version":0,"position":10}`)
	if w.Code != 204 || s.pos(p) > 1 {
		t.Fatalf("telemetry advanced clock: %d %f", w.Code, s.pos(p))
	}
	w = linkRequest(mux, "POST", "/output-api/"+id+"/report", token, `{"entry":1,"version":0,"position":80}`)
	if w.Code != 400 || s.pos(p) > 1 {
		t.Fatalf("report sought: %d %f", w.Code, s.pos(p))
	}
}

func TestOutputLinkRotationOwnershipAndPersistence(t *testing.T) {
	s, id, oldToken, mux := newLinkFixture(t)
	p := s.players[id]
	s.file = filepath.Join(t.TempDir(), "players.json")
	b, _ := json.Marshal([]*Player{p})
	if err := os.WriteFile(s.file, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded := queueStore()
	loaded.file = s.file
	if err := loaded.load(); err != nil || loaded.players[id] == nil || loaded.players[id].LinkHash != p.LinkHash {
		t.Fatalf("empty shared output lost on restart: %v", err)
	}
	p.Owner = queueID("someone-else")
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.rotateLink(w, r)
	if w.Code != 404 {
		t.Fatalf("another owner rotated link: %d", w.Code)
	}
	p.Owner = queueID("")
	w = httptest.NewRecorder()
	s.rotateLink(w, r)
	if w.Code != 200 {
		t.Fatalf("rotation: %d %s", w.Code, w.Body)
	}
	if got := linkRequest(mux, "GET", "/output-api/"+id+"/stream?entry=1", oldToken, "").Code; got != 401 {
		t.Fatalf("rotated token still authorized: %d", got)
	}
	if err := s.command(id, command{Op: "forget"}); err != nil {
		t.Fatal(err)
	}
	if s.players[id] != nil {
		t.Fatal("revoked output remains")
	}
}

func TestOutputLinkMultipleReceiversAndRevocation(t *testing.T) {
	s, id, token, mux := newLinkFixture(t)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	connect := func(receiver string) (*http.Response, *bufio.Scanner) {
		t.Helper()
		r, _ := http.NewRequest("GET", server.URL+"/output-api/"+id+"/events", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Slopify-Receiver", strings.Repeat(receiver, 32))
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			t.Fatalf("connect: %d", response.StatusCode)
		}
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: player" {
				return response, scanner
			}
		}
		response.Body.Close()
		t.Fatalf("no initial player state: %v", scanner.Err())
		return nil, nil
	}
	first, _ := connect("a")
	defer first.Body.Close()
	second, stream := connect("b")
	defer second.Body.Close()
	s.mu.Lock()
	p := s.players[id]
	count, primary := len(p.receivers), p.reportReceiver
	s.mu.Unlock()
	if count != 2 || primary != strings.Repeat("a", 32) {
		t.Fatalf("receivers=%d primary=%s", count, primary)
	}
	first.Body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		count, primary, online := len(p.receivers), p.reportReceiver, p.Connected
		s.mu.Unlock()
		if count == 1 && primary == strings.Repeat("b", 32) && online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver failover: count=%d primary=%s online=%v", count, primary, online)
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.command(id, command{Op: "forget"}); err != nil {
		t.Fatal(err)
	}
	closed := false
	for stream.Scan() {
		if stream.Text() == "event: closed" {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatalf("revocation did not close receiver: %v", stream.Err())
	}
	if got := linkRequest(mux, "GET", "/output-api/"+id+"/stream?entry=1", token, "").Code; got != 401 {
		t.Fatalf("revoked capability status=%d", got)
	}
}

func TestWebSinkAvailableWithoutSessionsAndAdvancesQueue(t *testing.T) {
	s, id, _, _ := newLinkFixture(t)
	if err := s.userCommand("", command{Op: "outputs", Outputs: []string{id}}); err != nil {
		t.Fatalf("cannot select sink without receivers: %v", err)
	}
	if err := s.userCommand("", command{Op: "append", Tracks: []Entry{
		{Path: "one.mp3", Duration: 10}, {Path: "two.mp3", Duration: 20},
	}}); err != nil {
		t.Fatal(err)
	}
	q, output := s.players[queueID("")], s.players[id]
	if q.State != Playing || !q.Connected || output.State != Playing {
		t.Fatal("zero-session sink was treated as offline")
	}
	var public Player
	if err := json.Unmarshal(s.marshal(output), &public); err != nil || !public.Available || public.Connected || public.Receivers != 0 {
		t.Fatalf("incorrect public sink availability: %+v %v", public, err)
	}
	first := q.Current
	q.UpdatedAt = time.Now().Add(-11 * time.Second)
	s.advanceIdleSinks()
	if q.Current == first || q.entry(q.Current).Path != "two.mp3" || q.State != Playing {
		t.Fatal("queue did not advance with zero listeners")
	}
	detach, err := s.attach(id, func() *Player { return &Player{} }, func(*Player) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	detach()
	if q.State != Playing || output.State != Playing || output.disconnectTimer != nil {
		t.Fatal("last receiver disconnect paused the sink")
	}
	// With a real receiver, natural playback completion owns advancement.
	output.receivers = map[string]*linkReceiver{"test": {cancel: func() {}}}
	q.UpdatedAt = time.Now().Add(-21 * time.Second)
	s.advanceIdleSinks()
	if q.State != Playing {
		t.Fatal("server clock interrupted a connected receiver")
	}
	output.receivers = nil
	s.advanceIdleSinks()
	if q.State != Stopped || output.State != Stopped {
		t.Fatal("empty sink did not finish its queue")
	}
}

func TestLegacyWebSinkStateMigratesWithoutLosingLinkOrOwnership(t *testing.T) {
	s, id, token, _ := newLinkFixture(t)
	p := s.players[id]
	p.Owner = queueID("alice")
	p.Queue = []Entry{{ID: 1, Path: "one.mp3", Duration: 120}}
	p.Current = 1
	p.State, p.Position = Playing, 42
	p.Kind = Browser
	legacy := struct {
		*Player
		Shared bool `json:"shared"`
	}{p, true}
	file := filepath.Join(t.TempDir(), "players.json")
	b, err := json.Marshal([]any{legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded := queueStore()
	loaded.file = file
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	sink := loaded.linked(id, linkHash(r))
	if sink == nil || sink.Kind != WebSink || sink.State != Playing || sink.Position != 42 {
		t.Fatalf("sink or capability lost in migration: %+v", sink)
	}
	if !loaded.visiblePlayer("alice", sink) || loaded.visiblePlayer("bob", sink) {
		t.Fatal("web sink ownership lost during migration")
	}
	if err := loaded.webCommand("bob", id, command{Op: "forget"}); !errors.Is(err, errForbidden) {
		t.Fatalf("another account could revoke the sink: %v", err)
	}
	public := loaded.marshal(sink)
	var view Player
	if err := json.Unmarshal(public, &view); err != nil {
		t.Fatal(err)
	}
	if !view.Available || view.Connected || strings.Contains(string(public), sink.LinkHash) || strings.Contains(string(public), "shared") {
		t.Fatalf("incorrect explicit sink state: %s", public)
	}
}
