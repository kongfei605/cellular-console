package main

import (
	"flag"
	"github.com/godbus/dbus/v5"
	"github.com/kongfei605/cellular-console/internal/gnsscontrol"
	"github.com/kongfei605/cellular-console/internal/hostconfig"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"time"
)

type peerListener struct {
	net.Listener
	uid uint32
}

func (l peerListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		if gnsscontrol.PeerAllowed(c, l.uid) {
			return c, nil
		}
		_ = c.Close()
	}
}
func main() {
	device := flag.String("device-id", "", "locally reviewed EC20 identity")
	hostPath := flag.String("host-config", hostconfig.DefaultPath, "root-owned reviewed hardware and network profile")
	failover := flag.String("failover-state", "/run/ec20-failover/status.json", "independent failover status")
	flag.Parse()
	host, e := hostconfig.Load(*hostPath)
	if e != nil {
		log.Fatal(e)
	}
	if *device != "" && *device != host.DeviceID {
		log.Fatal("device-id conflicts with host profile")
	}
	*device = host.DeviceID
	if os.Geteuid() != 0 || *device == "" || os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) || os.Getenv("LISTEN_FDS") != "1" {
		log.Fatal("root and one systemd-activated Unix socket required")
	}
	u, e := user.Lookup("ec20-console")
	if e != nil {
		log.Fatal(e)
	}
	uid, e := strconv.ParseUint(u.Uid, 10, 32)
	if e != nil {
		log.Fatal(e)
	}
	inherited := os.NewFile(3, "gnss-control")
	listener, e := net.FileListener(inherited)
	_ = inherited.Close()
	if e != nil {
		log.Fatal(e)
	}
	if listener.Addr().Network() != "unix" {
		log.Fatal("only Unix listener accepted")
	}
	conn, e := dbus.ConnectSystemBus()
	if e != nil {
		log.Fatal(e)
	}
	defer conn.Close()
	handler := &gnsscontrol.Server{Driver: &gnsscontrol.Modem{Conn: conn, DeviceID: *device}, Guard: gnsscontrol.NetworkGuard{Failover: *failover, CellInterface: host.CellInterface, USBPath: host.USBPath, VPNInterfaces: host.VPNInterfaces}, DeviceID: *device}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 2048}
	log.Print("GNSS-only broker ready on local Unix socket; no automatic modem operation")
	log.Fatal(server.Serve(peerListener{Listener: listener, uid: uint32(uid)}))
}
