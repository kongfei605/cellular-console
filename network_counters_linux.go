//go:build !darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readCounters(name string) (counterReading, error) {
	var c counterReading
	if !interfaceName.MatchString(name) {
		return c, fmt.Errorf("invalid interface name")
	}
	dir := filepath.Join("/sys/class/net", name)
	read := func(p string, base int) (uint64, error) {
		b, e := os.ReadFile(filepath.Join(dir, p))
		if e != nil {
			return 0, e
		}
		return strconv.ParseUint(strings.TrimSpace(string(b)), base, 64)
	}
	var err error
	if c.Index, err = read("ifindex", 10); err != nil {
		return c, err
	}
	if c.RX, err = read("statistics/rx_bytes", 10); err != nil {
		return c, err
	}
	if c.TX, err = read("statistics/tx_bytes", 10); err != nil {
		return c, err
	}
	flags, err := read("flags", 0)
	c.Up = flags&1 != 0
	return c, err
}
