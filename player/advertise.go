package main

import (
	"context"
	"fmt"
	"net"

	"github.com/codemowers/slopify/player/internal/discovery"
)

// advertise publishes a receiver over mDNS through Avahi, unless it listens on
// loopback, and only for IPv4 when it accepts nothing else. Discovery is not
// authentication.
func advertise(ctx context.Context, serviceType string, listener net.Listener, txt []string, name func(collision int) string) {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.IP.IsLoopback() {
		return
	}
	discovery.Publish(ctx, serviceType, addr.Port, txt, addr.IP.To4() != nil, name)
}

// collisionName numbers an instance whose name another device already uses.
func collisionName(name string, collision int) string {
	if collision > 1 {
		return fmt.Sprintf("%s (%d)", name, collision)
	}
	return name
}
