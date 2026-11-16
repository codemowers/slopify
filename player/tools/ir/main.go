// slopify-ir is an OpenWrt receiver setup helper, separate from audio playback.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprintln(os.Stderr, "Usage: slopify-ir {ir-info|ir-setup|ir-learn} [flags]\nConfigure kernel IR receivers/keymaps and learn decoded scancodes. No transmission or audio playback.")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(irCommand(ctx, os.Args[1:]))
}
