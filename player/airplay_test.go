package main

import "testing"

func TestAirPlayVolumePackets(t *testing.T) {
	for _, p := range []string{"ssncpvol-15,0,0,0", "ssncpvol-144,0,0,0", "ssncpvol0,0,0,0"} {
		if _, ok := airplayVolume([]byte(p)); !ok {
			t.Fatal(p)
		}
	}
	for _, p := range []string{"ssncpvolNaN,0,0,0", "ssncpvol1,0,0,0", "ssncpvol-31,0,0,0", "ssncpvol-15,0,0", "ssncacre1234"} {
		if _, ok := airplayVolume([]byte(p)); ok {
			t.Fatal(p)
		}
	}
}
func TestAirPlayMetadataBoundary(t *testing.T) {
	var a airplayMetadata
	for _, p := range []string{"ssncconn10.0.0.2", "ssncdaidABC123", "ssncsnamLaptop", "ssncacreSECRET", "coreminmTrack", "ssncsnuaPipeWire"} {
		a.observe([]byte(p))
	}
	ip, m := a.snapshot()
	if ip != "10.0.0.2" || m["sender.id"] != "ABC123" || m["title"] != "Track" || len(m) != 4 {
		t.Fatalf("unexpected metadata: %v %v", ip, m)
	}
	a.observe([]byte("ssncconn10.0.0.3"))
	ip, m = a.snapshot()
	if ip != "10.0.0.3" || len(m) != 0 {
		t.Fatal("previous sender leaked")
	}
	a.observe([]byte("ssncclipinvalid"))
	ip, _ = a.snapshot()
	if ip != "10.0.0.3" {
		t.Fatal("accepted invalid IP")
	}
}
