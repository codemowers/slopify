//go:build !linux

package main

import "syscall"

// TCP_USER_TIMEOUT and TCP_INFO are only used on Linux.
func (t tcpTuning) control(_, _ string, _ syscall.RawConn) error { return nil }

func readTCPSample(syscall.RawConn) (tcpSample, bool) { return tcpSample{}, false }
