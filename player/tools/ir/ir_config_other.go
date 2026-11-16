//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
)

func irCommand(context.Context, []string) int {
	fmt.Fprintln(os.Stderr, "IR configuration requires Linux")
	return 1
}
