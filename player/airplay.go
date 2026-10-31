package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Only explicitly enabled loopback Shairport metadata is accepted. Sender IDs
// are descriptive, not authentication; Active-Remote credentials are ignored.
type airplayMetadata struct {
	mu     sync.Mutex
	ip     string
	fields map[string]string
}

func (a *airplayMetadata) snapshot() (string, map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := map[string]string{}
	for k, v := range a.fields {
		m[k] = v
	}
	return a.ip, m
}
func airplayVolume(packet []byte) (float64, bool) {
	if len(packet) < 9 || len(packet) > 136 || string(packet[:8]) != "ssncpvol" {
		return 0, false
	}
	parts := strings.Split(string(packet[8:]), ",")
	if len(parts) != 4 {
		return 0, false
	}
	var value float64
	for i, p := range parts {
		v, e := strconv.ParseFloat(p, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		if i == 0 {
			value = v
		}
	}
	return value, value == -144 || value >= -30 && value <= 0
}
func (a *airplayMetadata) observe(packet []byte) {
	if len(packet) < 8 || len(packet) > 2056 {
		return
	}
	tag, value := string(packet[:8]), metadataText(string(packet[8:]))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fields == nil {
		a.fields = map[string]string{}
	}
	if tag == "ssncconn" {
		a.fields = map[string]string{}
		a.ip = ""
	}
	if tag == "ssncconn" || tag == "ssncclip" {
		if ip := net.ParseIP(value); ip != nil {
			a.ip = ip.String()
		}
		return
	}
	key := ""
	switch tag {
	case "ssncdaid":
		key = "sender.id"
	case "ssncsnam":
		key = "sender.name"
	case "ssncsnua":
		key = "sender.software"
	case "coreminm":
		key = "title"
	case "coreasar":
		key = "artist"
	case "coreasal":
		key = "album"
	case "coreasgn":
		key = "genre"
	}
	if key != "" {
		if value == "" {
			delete(a.fields, key)
		} else {
			a.fields[key] = value
		}
	}
}
func (c *controller) startAirPlayMetadata(ctx context.Context, address string) error {
	addr, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return err
	}
	if addr.IP == nil || !addr.IP.IsLoopback() || addr.Port == 0 {
		return errors.New("AirPlay metadata must bind an explicit loopback address and port")
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	latest := make(chan float64, 1)
	go func() { <-ctx.Done(); conn.Close() }()
	go func() {
		defer close(latest)
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			c.airplay.observe(buf[:n])
			if v, ok := airplayVolume(buf[:n]); ok && c.mixer != nil {
				select {
				case <-latest:
				default:
				}
				select {
				case latest <- v:
				default:
				}
			}
		}
	}()
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			select {
			case v, ok := <-latest:
				if !ok {
					return
				}
				control := c.mixer.snapshot()
				call, cancel := context.WithTimeout(ctx, 2*time.Second)
				var err error
				c.mixer.mu.Lock()
				hasSwitch := c.mixer.switchAvailable
				c.mixer.mu.Unlock()
				if v == -144 && hasSwitch {
					err = c.mixer.setMute(call, true)
				} else {
					level := control.Min
					if v != -144 {
						level += int(math.Round((v + 30) / 30 * float64(control.Max-control.Min)))
					}
					err = c.mixer.setLevel(call, level)
					if err == nil && hasSwitch {
						err = c.mixer.setMute(call, false)
					}
				}
				cancel()
				if err != nil {
					slog.Warn("AirPlay hardware volume", "err", err)
				} else {
					c.reportVolume(ctx)
				}
			default:
			}
		}
	}()
	return nil
}
