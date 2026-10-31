package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codemowers/slopify/internal/auth"
)

func TestDeviceOwnershipAndNames(t *testing.T) {
	s := queueStore()
	p := &Player{ID: "amp", Kind: Device, Connected: true, ReportedName: "HiFiBerry Amp2"}
	s.players[p.ID] = p
	user := auth.Principal{Issuer: "https://idp.example", Subject: "alice", Name: "Lauri"}
	if err := s.webCommandFor(user, p.ID, command{Op: "adopt"}); err != nil {
		t.Fatal(err)
	}
	if p.Owner != queueID(user.Subject) || p.OwnerIssuer != user.Issuer || p.OwnerSubject != user.Subject || p.Name != "Lauri's HiFiBerry Amp2" {
		t.Fatalf("wrong ownership/name: %+v", p)
	}
	if s.visiblePlayer("bob", p) {
		t.Fatal("claimed device visible to another user")
	}
	for _, other := range []auth.Principal{
		{Issuer: user.Issuer, Subject: "bob"},
		{Issuer: "https://different-idp.example", Subject: user.Subject},
	} {
		for _, op := range []string{"adopt", "rename", "forget", "volume"} {
			if err := s.webCommandFor(other, p.ID, command{Op: op, Name: "stolen", Level: level(20)}); !errors.Is(err, errForbidden) {
				t.Fatalf("%s allowed for %+v: %v", op, other, err)
			}
		}
	}
	if err := s.userCommand("bob", command{Op: "outputs", Outputs: []string{p.ID}}); !errors.Is(err, errForbidden) {
		t.Fatalf("foreign output selected: %v", err)
	}
	if err := s.webCommandFor(user, p.ID, command{Op: "rename", Name: "Living room"}); err != nil {
		t.Fatal(err)
	}
	p.ReportedName = "New local name"
	refreshDeviceName(p)
	if p.Name != "Living room" {
		t.Fatal("local name overwrote web override")
	}
	if err := s.webCommandFor(user, p.ID, command{Op: "rename"}); err != nil {
		t.Fatal(err)
	}
	if p.Name != "Lauri's New local name" || p.NameSet {
		t.Fatalf("default reset: %+v", p)
	}
	p.TokenHash = "private-device-token"
	b := s.marshal(p)
	for _, secret := range []string{"ownerSubject", "ownerIssuer", "private-device-token"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("public JSON contains %q", secret)
		}
	}
}

func TestDeviceClaimIsAtomic(t *testing.T) {
	s := queueStore()
	s.players["amp"] = &Player{ID: "amp", Kind: Device, Connected: true}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, subject := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(subject string) { defer wg.Done(); results <- s.webCommand(subject, "amp", command{Op: "adopt"}) }(subject)
	}
	wg.Wait()
	close(results)
	won, denied := 0, 0
	for err := range results {
		if err == nil {
			won++
		} else if errors.Is(err, errForbidden) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if won != 1 || denied != 1 {
		t.Fatalf("claims: won=%d denied=%d", won, denied)
	}
}

func TestLegacyDeviceOwnershipMigration(t *testing.T) {
	for _, owners := range [][]string{nil, {"alice"}, {"alice", "bob"}} {
		t.Run(strings.Join(owners, "+"), func(t *testing.T) {
			s := queueStore()
			s.file = filepath.Join(t.TempDir(), "players.json")
			records := []*Player{{ID: "amp", Kind: Device, Name: "Amp", Adopted: true}}
			for _, subject := range owners {
				records = append(records, &Player{ID: queueID(subject), Kind: User, Adopted: true, OutputIDs: []string{"amp"}})
			}
			b, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.file, b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.load(); err != nil {
				t.Fatal(err)
			}
			p := s.players["amp"]
			if len(owners) == 1 {
				if !p.Adopted || p.Owner != queueID("alice") {
					t.Fatalf("unique selection not migrated: %+v", p)
				}
				s.rememberOwner(auth.Principal{Issuer: "https://idp.example", Subject: "alice", Name: "Lauri"})
				if p.Name != "Lauri's Amp" {
					t.Fatalf("name: %q", p.Name)
				}
			} else {
				if p.Adopted || p.Owner != "" {
					t.Fatal("ambiguous/unselected device claimed")
				}
				for _, subject := range owners {
					if len(queueOutputs(s.players[queueID(subject)])) != 0 {
						t.Fatal("stale selection retained")
					}
				}
			}
		})
	}
}
