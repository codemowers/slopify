package discovery

import (
	"context"
	"errors"
	"net/netip"

	"github.com/godbus/dbus/v5"
)

// Resolve looks up one service instance through Avahi in the address family of
// family. The caller decides whether the answer is acceptable: a name on the LAN
// is not authentication, so it should only dial a host it already trusts.
func Resolve(ctx context.Context, serviceType, name string, family netip.Addr) (netip.AddrPort, error) {
	if name == "" || len(name) > 63 {
		return netip.AddrPort{}, errors.New("unacceptable service name")
	}
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return netip.AddrPort{}, err
	}
	defer conn.Close()
	protocol := int32(0) // IPv4
	if family.Unmap().Is6() {
		protocol = 1
	}
	var iface, resolvedProtocol, addressProtocol int32
	var resolvedName, resolvedType, domain, host, address string
	var port uint16
	var txt [][]byte
	var flags uint32
	err = call(ctx, conn.Object(avahi, dbus.ObjectPath("/")), avahi+".Server.ResolveService", int32(-1), int32(-1), name, serviceType, "local", protocol, uint32(0)).
		Store(&iface, &resolvedProtocol, &resolvedName, &resolvedType, &domain, &host, &addressProtocol, &address, &port, &txt, &flags)
	if err != nil {
		return netip.AddrPort{}, err
	}
	addr, err := netip.ParseAddr(address)
	if err != nil || port == 0 {
		return netip.AddrPort{}, errors.New("unusable resolved address")
	}
	return netip.AddrPortFrom(addr.WithZone("").Unmap(), port), nil
}
