package main

import (
	"golang.org/x/sys/unix"
	"net"
	"syscall"
)

func bindProbeInterface(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		i, err := net.InterfaceByName(name)
		if err != nil {
			return err
		}
		var setErr error
		err = raw.Control(func(fd uintptr) { setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, i.Index) })
		if err != nil {
			return err
		}
		return setErr
	}
}
