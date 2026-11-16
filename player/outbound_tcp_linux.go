//go:build linux

package main

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func (t tcpTuning) control(_, _ string, c syscall.RawConn) error {
	if t.UserTimeout == 0 {
		return nil
	}
	var optErr error
	if err := c.Control(func(fd uintptr) {
		optErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(t.UserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return optErr
}

func readTCPSample(raw syscall.RawConn) (tcpSample, bool) {
	var info *unix.TCPInfo
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		info, optErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	}); err != nil || optErr != nil {
		return tcpSample{}, false
	}
	return tcpSample{
		rtt:         time.Duration(info.Rtt) * time.Microsecond,
		receiveRTT:  time.Duration(info.Rcv_rtt) * time.Microsecond,
		retransmits: info.Total_retrans,
		outOfOrder:  info.Rcv_ooopack,
		segments:    info.Segs_in,
	}, true
}
