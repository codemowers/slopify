package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"

	"github.com/codemowers/slopify/player/internal/discovery"
	"github.com/codemowers/slopify/player/internal/raop"
)

// raopLive is an AirPlay 1 session. The UI calls the protocol AirPlay; flags and
// code call it RAOP, the version actually implemented.
type raopLive struct{ *raop.Stream }

func (s raopLive) Protocol() string      { return "airplay" }
func (s raopLive) Peer() string          { return s.Stream.Peer }
func (s raopLive) Volume() uint32        { return 0 }
func (s raopLive) SetVolume(uint32)      {} // RAOP has no way to tell the sender.
func (s raopLive) SetDeviceDelay(uint64) {}
func (s raopLive) Props() map[string]string {
	meta := s.Metadata()
	return map[string]string{"application.name": s.Name, "media.title": meta.Title, "media.artist": meta.Artist,
		"media.album": meta.Album, "media.genre": meta.Genre}
}

// PCM describes the decoded samples, which are held as left-justified int32.
func (s raopLive) PCM() livePCM {
	return livePCM{name: fmt.Sprintf("S%d_LE", s.Bits), rate: s.Rate, channels: s.Channels, bytes: 4, bits: s.Bits}
}

// startRAOP runs the AirPlay 1 receiver and advertises it. Without a usable key it
// still serves plain RAOP, so a key that has not been fetched yet only limits senders.
func (c *controller) startRAOP(ctx context.Context, listen string, bufferMillis int, keyPath string) error {
	lan := c.nativeLAN
	if lan == nil {
		return fmt.Errorf("raop-listen requires the ALSA playback backend")
	}
	config := raop.Config{BufferMillis: bufferMillis, OnStream: func(stream *raop.Stream) bool { return lan.addStream(raopLive{stream}) },
		Resolve: func(ctx context.Context, name string, peer netip.Addr) (netip.AddrPort, error) {
			return discovery.Resolve(ctx, "_dacp._tcp", name, peer)
		}}
	if keyPath != "" {
		key, err := raop.LoadKey(keyPath)
		if err != nil {
			slog.Warn("raop encryption unavailable; Apple devices cannot connect", "err", err)
		} else {
			config.Key = key
			c.raopEncrypted.Store(true)
		}
	}
	host, _ := os.Hostname()
	config.HardwareID = raop.HardwareID(host + "\x00" + c.name)
	config.OnVolume = func(stream *raop.Stream, db float64) {
		key, _, _ := liveSenderKey(raopLive{stream})
		c.requestSenderVolume(key, dbSenderLevel(db))
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := raop.NewServer(config)
	go func() {
		if err := server.Serve(ctx, listener); err != nil {
			slog.Error("raop receiver", "err", err)
		}
	}()
	_, txt := raop.Advertisement(c.name, config.HardwareID, config.Key != nil)
	advertise(ctx, raop.ServiceType, listener, txt, func(collision int) string {
		instance, _ := raop.Advertisement(collisionName(c.name, collision)+" via AirPlay", config.HardwareID, config.Key != nil)
		return instance
	})
	slog.Info("raop receiver listening", "address", listener.Addr().String(), "encryption", config.Key != nil)
	return nil
}
