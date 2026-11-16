package receiverconfig

import "testing"

func TestReceiverConfiguration(t *testing.T) {
	for _, tc := range []struct {
		pulse string
		want  bool
	}{{"true", true}, {"false", false}, {"", true}} {
		t.Setenv("SLOPIFY_NAME", "HiFiBerry Amp2")
		t.Setenv("SLOPIFY_PULSE_ENABLED", tc.pulse)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.Pulse != tc.want || c.Name != "HiFiBerry Amp2" {
			t.Fatalf("%+v", c)
		}
	}
	t.Setenv("SLOPIFY_PULSE_ENABLED", "typo")
	if _, err := FromEnv(); err == nil {
		t.Fatal("accepted invalid deployment flag")
	}
}
