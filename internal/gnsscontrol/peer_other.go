//go:build !linux

package gnsscontrol

import "net"

func PeerAllowed(c net.Conn, uid uint32) bool { return false }
