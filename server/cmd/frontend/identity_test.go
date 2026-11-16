package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQueueIdentityComesFromVerifiedSession(t *testing.T) {
	g := &frontend{key: []byte("test-session-key")}
	called := false
	handler := g.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		u, _ := g.session(r)
		if got := u.Subject; got != "alice" {
			t.Errorf("untrusted subject forwarded: %q", got)
		}
	}))
	request := httptest.NewRequest("GET", "/api/players/events", nil)
	request.Header.Set("X-Slopify-User", "bob")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if called || w.Code != http.StatusUnauthorized {
		t.Fatal("header alone authenticated a user")
	}
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: g.seal(user{Subject: "alice", Expires: time.Now().Add(time.Hour)})})
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if !called {
		t.Fatal("verified session rejected")
	}
}
