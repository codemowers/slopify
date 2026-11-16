package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnonymousIdentityAndMutationBoundary(t *testing.T) {
	t.Setenv("SLOPIFY_ANONYMOUS", "true")
	a, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := a.Require(Scope("slopify:players:write"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Subject(r) != "anonymous" || User(r).Issuer != "slopify:anonymous" || User(r).Name != "Anonymous" {
			t.Fatal("wrong shared identity")
		}
		if r.Header.Get("X-Slopify-User") != "" {
			t.Fatal("untrusted identity header retained")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, method, origin, site, header string
		want                               int
	}{
		{"read", "GET", "", "", "", 204},
		{"nonbrowser mutation", "POST", "", "", "1", 204},
		{"same origin", "POST", "http://example.com", "same-origin", "1", 204},
		{"form mutation", "POST", "http://example.com", "same-origin", "", 403},
		{"cross origin", "POST", "http://evil.example", "", "1", 403},
		{"scheme mismatch", "POST", "https://example.com", "", "1", 403},
		{"null origin", "POST", "null", "", "1", 403},
		{"cross site read", "GET", "", "cross-site", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://example.com/api/test", nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-Slopify-Request", tc.header)
			r.Header.Set("X-Slopify-User", "attacker")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestAnonymousRequiresExplicitOptIn(t *testing.T) {
	for _, v := range []string{"", "false", "1", "TRUE"} {
		t.Setenv("SLOPIFY_ANONYMOUS", v)
		if AnonymousEnabled() {
			t.Fatalf("enabled by %q", v)
		}
	}
}
