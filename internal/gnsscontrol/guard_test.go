package gnsscontrol

import "testing"

func TestNetworkGuardHasNoImplicitHostSpecificFallback(t *testing.T) {
	for _, g := range []NetworkGuard{{}, {CellInterface: "ppp7", USBPath: "2-3.4"}, {VPNInterfaces: []string{"wg0"}}} {
		if g.Check() == nil {
			t.Fatal("incomplete network guard accepted")
		}
		if _, e := g.Signature(); e == nil {
			t.Fatal("incomplete guard signature accepted")
		}
	}
}
