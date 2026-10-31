//go:build !linux

package main

import "context"

func followInput(context.Context, string, *inputDevice, chan<- knobEvent) {}
