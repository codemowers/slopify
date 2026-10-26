// Package auth independently protects APIs with OIDC access tokens.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codemowers/slopify/internal/httpx"
	"github.com/coreos/go-oidc/v3/oidc"
)

type userKey struct{}
type Authorizer struct{ verifier *oidc.IDTokenVerifier }

func New(ctx context.Context) (*Authorizer, error) {
	provider, err := oidc.NewProvider(ctx, httpx.Env("OIDC_ISSUER"))
	if err != nil {
		return nil, err
	}
	return &Authorizer{provider.Verifier(&oidc.Config{ClientID: httpx.Env("OIDC_AUDIENCE")})}, nil
}
func Subject(r *http.Request) string { s, _ := r.Context().Value(userKey{}).(string); return s }
func (a *Authorizer) Require(scope func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(raw, ".")
		var header struct {
			Type string `json:"typ"`
		}
		if !ok || len(parts) != 3 {
			invalid(w)
			return
		}
		b, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil || json.Unmarshal(b, &header) != nil || (header.Type != "at+jwt" && header.Type != "application/at+jwt") {
			invalid(w)
			return
		}
		token, err := a.verifier.Verify(r.Context(), raw)
		if err != nil || token.Subject == "" {
			invalid(w)
			return
		}
		var claims struct {
			Scope     string `json:"scope"`
			NotBefore int64  `json:"nbf"`
		}
		if token.Claims(&claims) != nil || claims.NotBefore > time.Now().Unix() {
			invalid(w)
			return
		}
		required := scope(r)
		if !contains(strings.Fields(claims.Scope), required) {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+required+`"`)
			http.Error(w, "required scope: "+required, http.StatusForbidden)
			return
		}
		// Services use verified context, never client-supplied identity headers.
		r.Header.Del("X-Slopify-User")
		ctx, cancel := context.WithDeadline(r.Context(), token.Expiry)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userKey{}, token.Subject)))
	})
}
func Scope(value string) func(*http.Request) string {
	return func(*http.Request) string { return value }
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func invalid(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
