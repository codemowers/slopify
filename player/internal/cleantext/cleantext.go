// Package cleantext makes untrusted network text safe to display: valid UTF-8,
// printable characters only, trimmed and bounded.
package cleantext

import (
	"strings"
	"unicode"
)

// Line returns s as one displayable line of at most limit runes.
func Line(s string, limit int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, "")))
	if runes := []rune(s); len(runes) > limit {
		s = strings.TrimSpace(string(runes[:limit]))
	}
	return s
}
