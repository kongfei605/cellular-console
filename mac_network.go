package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kongfei605/cellular-console/internal/atmodem"
)

// localNetworkObserver observes routes and bound HTTPS health. It never acts as
// a failover controller and never writes host routes, DNS or network services.
type localNetworkObserver struct {
	mu                                        sync.Mutex
	helper, location, primary, cell, probeURL string
	session                                   string
	state                                     Failover
	observed                                  time.Time
	config                                    NetworkConfig
	readRoute                                 func(context.Context) (string, string, error)
	probe                                     func(context.Context, string, string) bool
	discover                                  func(context.Context) ([]atmodem.Device, error)
}

func primaryRoute(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return "", "", err
	}
	var iface, gateway string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if fields[0] == "interface:" {
				iface = fields[1]
			}
			if fields[0] == "gateway:" {
				gateway = fields[1]
			}
		}
	}
	if !interfaceName.MatchString(iface) {
		return "", "", errors.New("default route unavailable")
	}
	return iface, gateway, nil
}
func interfaceIPv4(name string) (string, error) {
	i, err := net.InterfaceByName(name)
	if err != nil {
		return "", err
	}
	if i.Flags&net.FlagUp == 0 {
		return "", errors.New("interface down")
	}
	addresses, err := i.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addresses {
		if ip, _, e := net.ParseCIDR(a.String()); e == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String(), nil
		}
	}
	return "", errors.New("interface IPv4 unavailable")
}
func probeInterface(ctx context.Context, name, target string) bool {
	ip, err := interfaceIPv4(name)
	if err != nil {
		return false
	}
	// Bind the socket to the interface as well as its address. DNS may use the
	// host resolver; a successful response still requires this interface's path.
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}, Timeout: 3 * time.Second, Control: bindProbeInterface(name)}
	transport := &http.Transport{Proxy: nil, DialContext: dialer.DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, "HEAD", target, nil)
	if err != nil {
		return false
	}
	response, err := client.Do(r)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}
func newLocalNetwork(helper, location, primary, target string) (*localNetworkObserver, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("mac-probe-url requires a credential-free HTTPS endpoint returning 200")
	}
	if primary != "" && !interfaceName.MatchString(primary) {
		return nil, errors.New("invalid primary interface")
	}
	return &localNetworkObserver{helper: helper, location: location, primary: primary, probeURL: target, state: Failover{Source: "macos-local", Active: "unknown", VPNs: map[string]bool{}}, readRoute: primaryRoute, probe: probeInterface, discover: func(ctx context.Context) ([]atmodem.Device, error) { return atmodem.Devices(ctx, helper) }}, nil
}
func (n *localNetworkObserver) sample(ctx context.Context) {
	devices, err := n.discover(ctx)
	if err != nil {
		n.invalidate()
		return
	}
	var device *atmodem.Device
	for _, d := range devices {
		if n.location != "" && n.location != d.Location {
			continue
		}
		if device != nil {
			n.invalidate()
			return
		}
		v := d
		device = &v
	}
	if device == nil || !interfaceName.MatchString(device.Network) {
		n.invalidate()
		return
	}
	route, gateway, err := n.readRoute(ctx)
	if err != nil {
		n.invalidate()
		return
	}
	primary := n.primary
	if primary == "" {
		primary = route
		if primary == device.Network {
			n.mu.Lock()
			primary = n.config.WiredInterface
			n.mu.Unlock()
			if primary == "" {
				primary = "unavailable"
			}
		}
	}
	f := Failover{Source: "macos-local", VPNs: map[string]bool{}, WiredGateway: gateway, CellularPresent: true, Active: "unknown"}
	cellIP, cellErr := interfaceIPv4(device.Network)
	primaryIP, primaryErr := interfaceIPv4(primary)
	f.CellularIP = cellIP
	f.WiredIP = primaryIP
	f.WiredLink = primaryErr == nil
	if route == device.Network {
		f.Active = "cellular"
	} else if route == primary && primary != device.Network {
		f.Active = "wired"
	}
	var wg sync.WaitGroup
	if cellErr == nil {
		wg.Go(func() { f.CellularHealthy = n.probe(ctx, device.Network, n.probeURL) })
	}
	if primaryErr == nil && primary != device.Network && f.Active == "wired" {
		wg.Go(func() { f.WiredHealthy = n.probe(ctx, primary, n.probeURL) })
	}
	wg.Wait()
	// A probe spanning a route change cannot authorize an operation.
	after, _, routeErr := n.readRoute(ctx)
	if routeErr != nil || after != route {
		n.invalidate()
		return
	}
	f.ConnectionState = "disconnected"
	if f.CellularHealthy {
		f.ConnectionState = "connected"
	}
	f.Updated = time.Now().Format("2006-01-02T15:04:05-0700")
	n.mu.Lock()
	defer n.mu.Unlock()
	n.location = device.Location
	n.session = device.Session
	n.cell = device.Network
	n.state = f
	n.observed = time.Now()
	n.config = defaultNetworkConfig()
	n.config.WiredInterface = primary
	n.config.CellInterface = device.Network
	n.config.IntervalSeconds = 8
	n.config.ProbeTargets = []string{n.probeURL}
}
func (n *localNetworkObserver) invalidate() { n.mu.Lock(); n.observed = time.Time{}; n.mu.Unlock() }
func (n *localNetworkObserver) run(ctx context.Context) {
	n.sample(ctx)
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.sample(ctx)
		}
	}
}
func (n *localNetworkObserver) status() (Failover, error) {
	n.mu.Lock()
	f, stamp, location, session, cell, primary := n.state, n.observed, n.location, n.session, n.cell, n.config.WiredInterface
	n.mu.Unlock()
	if stamp.IsZero() || time.Since(stamp) > 15*time.Second {
		return f, errors.New("Mac 本地网络探测未就绪或已过期")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	route, _, err := n.readRoute(ctx)
	if err != nil || f.Active == "wired" && route != primary || f.Active == "cellular" && route != cell {
		return f, errors.New("Mac 默认路由已变化，等待重新探测")
	}
	devices, err := n.discover(ctx)
	if err != nil {
		return f, err
	}
	for _, d := range devices {
		if d.Location == location && d.Session == session && d.Network == cell {
			return f, nil
		}
	}
	return f, errors.New("EC20 USB 代际已变化，等待重新探测")
}
func (n *localNetworkObserver) configuration() NetworkConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.config.CellInterface == "" {
		c := defaultNetworkConfig()
		c.WiredInterface = "unavailable"
		c.CellInterface = "unavailable-cell"
		return c
	}
	return n.config
}
