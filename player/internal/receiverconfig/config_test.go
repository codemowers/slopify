package receiverconfig

import "testing"

func TestReceiverConfiguration(t *testing.T) {
	for _, tc := range []struct {
		pulse, airplay string
		wantP, wantA   bool
	}{
		{"true", "true", true, true},
		{"false", "true", false, true},
		{"true", "false", true, false},
		{"false", "false", false, false},
	} {
		t.Setenv("SLOPIFY_NAME", "HiFiBerry Amp2")
		t.Setenv("SLOPIFY_PULSE_ENABLED", tc.pulse)
		t.Setenv("SLOPIFY_AIRPLAY_ENABLED", tc.airplay)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.Pulse != tc.wantP || c.AirPlay != tc.wantA {
			t.Fatalf("%+v", c)
		}
	}
	t.Setenv("SLOPIFY_PULSE_ENABLED", "typo")
	if _, err := FromEnv(); err == nil {
		t.Fatal("accepted invalid deployment flag")
	}
}
