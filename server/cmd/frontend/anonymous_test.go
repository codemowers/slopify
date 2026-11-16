package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnonymousSessionAndToken(t *testing.T) {
	g := &frontend{anonymous: true}
	r := httptest.NewRequest("GET", "http://example.com/", nil)
	r.Header.Set("X-Slopify-User", "attacker")
	u, ok := g.session(r)
	if !ok || u.Subject != "anonymous" {
		t.Fatal("missing anonymous session")
	}
	for _, tc := range []struct {
		header, origin string
		want           int
	}{{"1", "http://example.com", 200}, {"", "http://example.com", 403}, {"1", "http://evil.example", 403}} {
		r := httptest.NewRequest("POST", "http://example.com/auth/token", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Slopify-Request", tc.header)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		g.token(w, r)
		if w.Code != tc.want {
			t.Fatalf("got %d want %d", w.Code, tc.want)
		}
		if w.Code == 200 {
			var got struct {
				Anonymous   bool
				AccessToken string
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !got.Anonymous || got.AccessToken != "" {
				t.Fatal("unexpected anonymous token response")
			}
		}
	}
}
