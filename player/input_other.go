//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
)

func followInput(context.Context, string, *inputDevice, chan<- knobEvent) {}

func inputPath(_ string, device string) string { return device }

func inputInfo([]string) int { fmt.Fprintln(os.Stderr, "input inventory requires Linux"); return 1 }
