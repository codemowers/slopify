// Package signedurl signs and verifies expiring stream URLs, so that players
// without a browser session (devices, <audio> elements) can fetch media.
package signedurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"time"
)

type Signer struct{ key []byte }

func New(key string) *Signer { return &Signer{key: []byte(key)} }

func (s *Signer) mac(p, format string, exp int64) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(p + "\x00" + format + "\x00" + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Query returns the query string granting access to path in format until ttl elapses.
func (s *Signer) Query(p, format string, ttl time.Duration) url.Values {
	exp := time.Now().Add(ttl).Unix()
	return url.Values{
		"path":   {p},
		"format": {format},
		"exp":    {strconv.FormatInt(exp, 10)},
		"sig":    {s.mac(p, format, exp)},
	}
}

var (
	ErrExpired = errors.New("signed URL expired")
	ErrInvalid = errors.New("invalid signature")
)

func (s *Signer) Verify(q url.Values) error {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		return ErrInvalid
	}
	if !hmac.Equal([]byte(q.Get("sig")), []byte(s.mac(q.Get("path"), q.Get("format"), exp))) {
		return ErrInvalid
	}
	if time.Now().Unix() > exp {
		return ErrExpired
	}
	return nil
}
