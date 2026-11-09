package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func readCounters(name string) (counterReading, error) {
	if !interfaceName.MatchString(name) {
		return counterReading{}, errors.New("invalid interface name")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return counterReading{}, err
	}
	raw, err := syscall.RouteRIB(syscall.NET_RT_IFLIST2, iface.Index)
	if err != nil {
		return counterReading{}, err
	}
	return parseDarwinCounters(raw, iface.Index)
}
func parseDarwinCounters(raw []byte, index int) (counterReading, error) {
	for len(raw) >= 4 {
		n := int(binary.LittleEndian.Uint16(raw))
		if n < 4 || n > len(raw) {
			return counterReading{}, errors.New("invalid route message length")
		}
		if raw[3] == unix.RTM_IFINFO2 && n >= unix.SizeofIfMsghdr2 {
			var h struct {
				Msglen                             uint16
				Version, Type                      uint8
				Addrs, Flags                       int32
				Index, Pad                         uint16
				SndLen, SndMaxlen, SndDrops, Timer int32
				Data                               unix.IfData64
			}
			if err := binary.Read(bytes.NewReader(raw[:n]), binary.LittleEndian, &h); err != nil {
				return counterReading{}, err
			}
			if int(h.Index) == index {
				return counterReading{Index: uint64(h.Index), RX: h.Data.Ibytes, TX: h.Data.Obytes, Up: h.Flags&unix.IFF_UP != 0}, nil
			}
		}
		raw = raw[n:]
	}
	return counterReading{}, errors.New("interface counters unavailable")
}
