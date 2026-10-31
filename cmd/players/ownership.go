package main

import (
	"strings"
	"unicode"

	"github.com/codemowers/slopify/internal/auth"
)

func ownerDisplayName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	runes := []rune(name)
	if len(runes) > 64 {
		name = string(runes[:64])
	}
	if name == "" {
		return "User"
	}
	return name
}
func refreshDeviceName(p *Player) {
	if p.Kind != Device || p.NameSet {
		return
	}
	name := p.ReportedName
	if name == "" {
		name = "Speaker"
	}
	if p.Owner != "" {
		name = ownerDisplayName(p.OwnerName) + "'s " + name
	}
	p.Name = name
}

// Refresh verified display claims without changing ownership or local identity.
func (s *store) rememberOwner(user auth.Principal) {
	if user.Subject == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.players {
		if p.Kind != Device || p.Owner != queueID(user.Subject) {
			continue
		}
		if p.OwnerIssuer != "" && p.OwnerIssuer != user.Issuer {
			continue
		}
		name := ownerDisplayName(user.Name)
		if p.OwnerName == name && p.OwnerSubject == user.Subject && p.OwnerIssuer == user.Issuer {
			continue
		}
		p.OwnerName = name
		p.OwnerSubject = user.Subject
		p.OwnerIssuer = user.Issuer
		refreshDeviceName(p)
		p.Version++
		s.changed(p)
	}
}
