// Command frontend serves static assets and OIDC login. APIs are independently
// authenticated and reached directly through ingress; no API proxy lives here.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/codemowers/slopify/internal/httpx"
	"github.com/codemowers/slopify/web"
)

const (
	sessionCookie = "slopify_session"
	loginCookie   = "slopify_login"
	sessionTTL    = 7 * 24 * time.Hour
)

type user struct {
	Subject string        `json:"sub"`
	Name    string        `json:"name,omitempty"`
	Email   string        `json:"email,omitempty"`
	Expires time.Time     `json:"exp"`
	Token   *oauth2.Token `json:"token,omitempty"`
}

type login struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Return   string    `json:"return"`
	Expires  time.Time `json:"exp"`
}

type frontend struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	key      []byte
	secure   bool
	audience string
}

func main() {
	ctx, stop := httpx.SignalContext()
	defer stop()

	provider, err := oidc.NewProvider(ctx, httpx.Env("OIDC_ISSUER"))
	if err != nil {
		slog.Error("discovering OIDC provider", "err", err)
		os.Exit(1)
	}
	// The OIDCClient secret lists redirect URIs comma-separated; the first is ours.
	redirect, _, _ := strings.Cut(httpx.Env("OIDC_REDIRECT_URI"), ",")
	clientID := httpx.Env("OIDC_CLIENT_ID")
	g := &frontend{
		oauth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: httpx.Env("OIDC_CLIENT_SECRET"),
			Endpoint:     provider.Endpoint(),
			RedirectURL:  redirect,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "offline_access", "slopify:library:read", "slopify:players:read", "slopify:players:write", "slopify:stream"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		key:      []byte(httpx.Env("SESSION_KEY")),
		secure:   strings.HasPrefix(redirect, "https://"),
		audience: httpx.Env("OIDC_AUDIENCE"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /auth/login", g.login)
	mux.HandleFunc("GET /auth/callback", g.callback)
	mux.HandleFunc("POST /auth/logout", g.logout)
	mux.Handle("GET /api/me", g.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := g.session(r)
		u.Token = nil
		httpx.JSON(w, u)
	})))
	mux.HandleFunc("POST /auth/token", g.token)
	static := http.FileServerFS(web.FS)
	mux.HandleFunc("GET /output/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		r.URL.Path = "/output.html"
		static.ServeHTTP(w, r)
	})
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if _, ok := g.session(r); !ok {
				http.Redirect(w, r, "/auth/login", http.StatusFound)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(w, r)
	}))
	if err := httpx.Serve(ctx, securityHeaders(mux)); err != nil {
		slog.Error("serving", "err", err)
		os.Exit(1)
	}
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self' blob:; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

func (g *frontend) aead() cipher.AEAD {
	key := sha256.Sum256(g.key)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}
func (g *frontend) seal(v any) string {
	b, _ := json.Marshal(v)
	aead := g.aead()
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, b, nil))
}
func (g *frontend) open(value string, v any) bool {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	aead := g.aead()
	if len(b) < aead.NonceSize() {
		return false
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], nil)
	return err == nil && json.Unmarshal(plain, v) == nil
}

func (g *frontend) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: g.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (g *frontend) session(r *http.Request) (user, bool) {
	var u user
	c, err := r.Cookie(sessionCookie)
	if err != nil || !g.open(c.Value, &u) || time.Now().After(u.Expires) {
		return user{}, false
	}
	return u, true
}

func (g *frontend) require(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := g.session(r)
		if !ok {
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (g *frontend) login(w http.ResponseWriter, r *http.Request) {
	ret := r.URL.Query().Get("return")
	if !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") || strings.HasPrefix(ret, "/\\") {
		ret = "/"
	}
	l := login{State: rand.Text(), Nonce: rand.Text(), Verifier: oauth2.GenerateVerifier(), Return: ret, Expires: time.Now().Add(10 * time.Minute)}
	g.setCookie(w, loginCookie, g.seal(l), 10*time.Minute)
	http.Redirect(w, r, g.oauth.AuthCodeURL(l.State, oidc.Nonce(l.Nonce), oauth2.S256ChallengeOption(l.Verifier), oauth2.SetAuthURLParam("resource", g.audience)), http.StatusFound)
}

func (g *frontend) callback(w http.ResponseWriter, r *http.Request) {
	var l login
	c, err := r.Cookie(loginCookie)
	if err != nil || !g.open(c.Value, &l) || time.Now().After(l.Expires) || r.URL.Query().Get("state") != l.State {
		http.Error(w, "login expired or invalid, please try again", http.StatusBadRequest)
		return
	}
	g.setCookie(w, loginCookie, "", -time.Second)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "sign-in failed: "+e, http.StatusForbidden)
		return
	}
	tok, err := g.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(l.Verifier), oauth2.SetAuthURLParam("resource", g.audience))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			http.Error(w, "sign-in failed: "+re.ErrorCode, http.StatusForbidden)
			return
		}
		slog.Error("exchanging authorization code", "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "identity provider returned no ID token", http.StatusBadGateway)
		return
	}
	idt, err := g.verifier.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != l.Nonce {
		http.Error(w, "invalid ID token", http.StatusForbidden)
		return
	}
	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := idt.Claims(&claims); err != nil {
		http.Error(w, "invalid ID token claims", http.StatusForbidden)
		return
	}
	u := user{Subject: idt.Subject, Name: claims.Name, Email: claims.Email, Expires: time.Now().Add(sessionTTL), Token: tok}
	g.setCookie(w, sessionCookie, g.seal(u), sessionTTL)
	slog.Info("signed in", "sub", u.Subject, "email", u.Email)
	http.Redirect(w, r, l.Return, http.StatusFound)
}

func (g *frontend) logout(w http.ResponseWriter, r *http.Request) {
	g.setCookie(w, sessionCookie, "", -time.Second)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Only the access token is exposed to JS. Refresh and ID tokens remain in
// the encrypted HttpOnly cookie; API services validate the token themselves.
func (g *frontend) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	u, ok := g.session(r)
	if !ok || u.Token == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	token, err := g.oauth.TokenSource(r.Context(), u.Token).Token()
	if err != nil {
		http.Error(w, "session renewal failed", http.StatusUnauthorized)
		return
	}
	if token.AccessToken != u.Token.AccessToken {
		u.Token = token
		g.setCookie(w, sessionCookie, g.seal(u), sessionTTL)
	}
	httpx.JSON(w, map[string]any{"accessToken": token.AccessToken, "expiresAt": token.Expiry})
}
