package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/godbus/dbus/v5"
)

// Publish advertises a service of another type through Avahi until ctx ends,
// republishing after daemon or bus restarts; ipv4 limits it to IPv4 addresses. name
// is called with the collision count, starting at one, so a renamed instance keeps
// its own prefix scheme.
func Publish(ctx context.Context, serviceType string, port int, txt []string, ipv4 bool, name func(collision int) string) {
	records := make([][]byte, len(txt))
	for i, record := range txt {
		records[i] = []byte(record)
	}
	go func() {
		collision := 1
		failed := false
		for ctx.Err() == nil {
			started := time.Now()
			err := publishSession(ctx, serviceType, label(name(collision), 63), uint16(port), records, ipv4)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errCollision) {
				collision++
			}
			if !failed || time.Since(started) > 30*time.Second {
				slog.Warn("service advertisement reconnecting", "type", serviceType, "err", err)
				failed = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

func publishSession(parent context.Context, serviceType, name string, port uint16, txt [][]byte, ipv4 bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	sink := &signals{events: make(chan *dbus.Signal, 16), overflow: make(chan struct{}, 1)}
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx), dbus.WithSignalHandler(sink))
	if err != nil {
		return err
	}
	defer conn.Close()
	server := conn.Object(avahi, dbus.ObjectPath("/"))
	var owner string
	if err = call(ctx, conn.BusObject(), "org.freedesktop.DBus.GetNameOwner", avahi).Store(&owner); err != nil {
		return err
	}
	if err = conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(owner), dbus.WithMatchInterface(avahi+".EntryGroup")); err != nil {
		return err
	}
	if err = conn.AddMatchSignalContext(ctx, dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, avahi)); err != nil {
		return err
	}
	var groupPath dbus.ObjectPath
	if err = call(ctx, server, avahi+".Server.EntryGroupNew").Store(&groupPath); err != nil {
		return err
	}
	group := conn.Object(avahi, groupPath)
	protocol := int32(-1) // Any.
	if ipv4 {
		protocol = 0
	}
	if err = call(ctx, group, avahi+".EntryGroup.AddService", int32(-1), protocol, uint32(0), name, serviceType, "local", "", port, txt).Err; err != nil {
		if isCollision(err) {
			return errCollision
		}
		return err
	}
	if err = call(ctx, group, avahi+".EntryGroup.Commit").Err; err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return errors.New("D-Bus disconnected")
		case <-sink.overflow:
			return errors.New("advertisement signal queue overflow")
		case signal := <-sink.events:
			if signal.Sender == "org.freedesktop.DBus" {
				// The bus also greets each connection with NameAcquired; only
				// Avahi's own name changing hands means the daemon restarted.
				if signal.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(signal.Body) == 3 && signal.Body[0] == avahi && signal.Body[2] != owner {
					return errors.New("Avahi owner changed")
				}
				continue
			}
			if signal.Sender != owner || signal.Path != groupPath || signal.Name != avahi+".EntryGroup.StateChanged" {
				continue
			}
			var state int32
			var message string
			if dbus.Store(signal.Body, &state, &message) == nil {
				switch state {
				case 3:
					return errCollision
				case 4:
					return fmt.Errorf("Avahi publication failed: %s", message)
				}
			}
		}
	}
}
