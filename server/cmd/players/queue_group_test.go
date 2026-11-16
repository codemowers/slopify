package main

import (
	"encoding/json"
	"testing"
)

func TestQueueGroupOpaqueAndScoped(t *testing.T) {
	q := &Player{ID: "queue", Kind: User, Adopted: true, OutputIDs: []string{"one", "two"}}
	one := &Player{ID: "one", Kind: Device}
	two := &Player{ID: "two", Kind: Device}
	other := &Player{ID: "other", Kind: Device}
	s := &store{players: map[string]*Player{"queue": q, "one": one, "two": two, "other": other}, dirty: make(chan struct{}, 1)}
	decode := func(p *Player) Player {
		var v Player
		if err := json.Unmarshal(s.marshal(p), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	a, b := decode(one), decode(two)
	if a.QueueGroup == "" || a.QueueGroup != b.QueueGroup || a.QueueGroup == q.ID {
		t.Fatalf("bad group: %q %q", a.QueueGroup, b.QueueGroup)
	}
	if decode(q).QueueGroup != "" || decode(other).QueueGroup != "" {
		t.Fatal("group escaped selected devices")
	}
	if decode(one).QueueGroup != a.QueueGroup {
		t.Fatal("group changed")
	}
}

func TestNaturalAdvanceKeepsQueueTransportCommand(t *testing.T) {
	s := queueStore()
	q := &Player{ID: "queue", Kind: User, Adopted: true, Connected: true, QueueCommand: "continuous", State: Playing, Current: 1, Queue: []Entry{{ID: 1}, {ID: 2}}, OutputID: "device", OutputIDs: []string{"device"}}
	p := &Player{ID: "device", Kind: Device, Adopted: true, Connected: true}
	s.players[q.ID] = q
	s.players[p.ID] = p
	s.copyPlayback(p, q)
	if err := s.report(p.ID, report{Version: p.Version, Entry: 1, State: Playing, Ended: true}); err != nil {
		t.Fatal(err)
	}
	if q.Current != 2 || q.QueueCommand != "continuous" {
		t.Fatalf("natural advance broke continuity: %d %q", q.Current, q.QueueCommand)
	}
	s.sendQueue(q)
	if q.QueueCommand == "continuous" {
		t.Fatal("explicit command failed to invalidate transport")
	}
}
