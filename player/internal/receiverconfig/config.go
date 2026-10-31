// Package receiverconfig shares deployment flags and discovery names.
package receiverconfig

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Pulse, AirPlay bool
	Name           string
}

func FromEnv() (Config, error) {
	c := Config{Pulse: true, Name: os.Getenv("SLOPIFY_NAME")}
	for key, dst := range map[string]*bool{"SLOPIFY_PULSE_ENABLED": &c.Pulse, "SLOPIFY_AIRPLAY_ENABLED": &c.AirPlay} {
		if value := os.Getenv(key); value != "" {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return c, fmt.Errorf("%s must be a boolean", key)
			}
			*dst = parsed
		}
	}
	if c.Name == "" {
		var err error
		c.Name, err = os.Hostname()
		if err != nil {
			return c, err
		}
	}
	return c, nil
}
