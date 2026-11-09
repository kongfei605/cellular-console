package main

import (
	"encoding/binary"
	"golang.org/x/sys/unix"
	"net"
	"testing"
)

func TestDarwinCountersWireLayoutAndBounds(t *testing.T) {
	raw := make([]byte, unix.SizeofIfMsghdr2)
	binary.LittleEndian.PutUint16(raw, uint16(len(raw)))
	raw[3] = unix.RTM_IFINFO2
	binary.LittleEndian.PutUint32(raw[8:], unix.IFF_UP)
	binary.LittleEndian.PutUint16(raw[12:], 42)
	binary.LittleEndian.PutUint64(raw[96:], 12345678901)
	binary.LittleEndian.PutUint64(raw[104:], 9876543210)
	c, err := parseDarwinCounters(raw, 42)
	if err != nil || c.Index != 42 || !c.Up || c.RX != 12345678901 || c.TX != 9876543210 {
		t.Fatal(c, err)
	}
	for _, bad := range [][]byte{raw[:10], {0, 0, 0, 0}, {255, 255, 0, 0}} {
		if _, err := parseDarwinCounters(bad, 42); err == nil {
			t.Fatal("truncated message accepted")
		}
	}
}
func TestDarwinLoopbackCountersReadRealHostWithoutModem(t *testing.T) {
	iface, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	c, err := readCounters("lo0")
	if err != nil || c.Index != uint64(iface.Index) || !c.Up {
		t.Fatal(c, err)
	}
}
