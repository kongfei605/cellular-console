import json
import subprocess
import unittest
from unittest.mock import Mock, patch

from ec20_failover import Controller
from vpn_health import Probe

UP = Probe("healthy", "uplink-reachable")
DOWN = Probe("unhealthy", "uplink-no-reply")
UNKNOWN = Probe("unknown", "probe-command-error")


class KernelRoutes:
    """Small independent route-table model: assert which interface wins."""

    def __init__(self):
        self.routes = [
            {"dev": "eth0", "gateway": "192.0.2.1", "metric": 100, "protocol": "dhcp"},
            {
                "dev": "usb0",
                "gateway": "198.51.100.1",
                "metric": 101,
                "protocol": "dhcp",
            },
            {"dev": "ppp0", "metric": 700, "protocol": "196"},
        ]
        self.history = []

    def __call__(self, *args):
        a = list(args)
        if a == ["-j", "route", "show", "default"]:
            return subprocess.CompletedProcess(a, 0, json.dumps(self.routes), "")
        if a == ["-j", "rule", "show"]:
            return subprocess.CompletedProcess(a, 0, "[]", "")
        if a[:3] == ["route", "replace", "default"]:
            d = {}
            for i in range(3, len(a), 2):
                d[a[i]] = a[i + 1]
            entry = {
                "dev": d["dev"],
                "metric": int(d["metric"]),
                "protocol": d["proto"],
            }
            if "via" in d:
                entry["gateway"] = d["via"]
            self.routes = [
                r
                for r in self.routes
                if not (r["dev"] == entry["dev"] and r["metric"] == entry["metric"])
            ] + [entry]
        elif a[:3] == ["route", "del", "default"]:
            d = dict(zip(a[3::2], a[4::2]))
            self.routes = [
                r
                for r in self.routes
                if not (
                    r["dev"] == d["dev"]
                    and r["metric"] == int(d["metric"])
                    and r["protocol"] == d["proto"]
                )
            ]
        else:
            raise AssertionError("unexpected route command " + repr(a))
        self.history.append([dict(r) for r in self.routes])
        return subprocess.CompletedProcess(a, 0, "", "")

    def selected(self):
        return min(self.routes, key=lambda r: r["metric"])["dev"]


class RouteRegressionTests(unittest.TestCase):
    def controller(self):
        c = Controller.__new__(Controller)
        c.wired, c.cell = "eth0", "ppp0"
        c.gw = "192.0.2.1"
        c.gateway_address = "192.0.2.2"
        c.active = "cellular"
        c.config = {
            "vpns": [],
            "failure_count": 3,
            "recovery_seconds": 30,
            "probe_targets": ["223.5.5.5"],
        }
        c.failures = 0
        c.good_since = None
        c.last_switch = None
        return c

    def test_cellular_preference_beats_usb_default_without_deleting_wired(self):
        c = self.controller()
        kernel = KernelRoutes()
        with patch("ec20_failover.ip", side_effect=kernel):
            c.enforce_routes("192.0.2.2", "203.0.113.2")
        self.assertEqual(kernel.selected(), "ppp0")
        self.assertTrue(
            any(r["dev"] == "eth0" and r["protocol"] == "dhcp" for r in kernel.routes)
        )
        self.assertTrue(
            any(r["dev"] == "usb0" and r["metric"] == 101 for r in kernel.routes)
        )
        self.assertFalse(
            any(r["dev"] == "ppp0" and r["metric"] == 700 for r in kernel.routes)
        )

    def test_lost_ppp_never_leaves_system_without_wired_default(self):
        c = self.controller()
        kernel = KernelRoutes()
        with patch("ec20_failover.ip", side_effect=kernel):
            c.enforce_routes("192.0.2.2", "203.0.113.2")
            c.enforce_routes("192.0.2.2", None)
        self.assertEqual(kernel.selected(), "eth0")
        self.assertFalse(any(r["dev"] == "ppp0" for r in kernel.routes))
        self.assertTrue(
            all(
                any(r["dev"] == "eth0" for r in snapshot) for snapshot in kernel.history
            )
        )

    def test_failback_restores_wired_before_removing_preferred_ppp(self):
        c = self.controller()
        kernel = KernelRoutes()
        with patch("ec20_failover.ip", side_effect=kernel):
            c.enforce_routes("192.0.2.2", "203.0.113.2")
            c.active = "wired"
            c.enforce_routes("192.0.2.2", "203.0.113.2")
        self.assertEqual(kernel.selected(), "eth0")
        self.assertEqual(
            [r["metric"] for r in kernel.routes if r["dev"] == "ppp0"], [700]
        )
        self.assertTrue(all(snapshot for snapshot in kernel.history))

    def test_failed_or_unknown_cellular_is_never_selected(self):
        for probe in [DOWN, UNKNOWN, None]:
            c = self.controller()
            c.active = "wired"
            for t in range(20):
                c.select_uplink(DOWN, "203.0.113.2", True, t * 5, probe)
            self.assertEqual(c.active, "wired")
        c = self.controller()
        c.active = None
        c.select_uplink(UNKNOWN, "203.0.113.2", True, 0, DOWN)
        self.assertEqual(c.active, "wired")

    def test_usable_wired_returns_immediately_after_ppp_loss(self):
        c = self.controller()
        c.select_uplink(UP, None, False, 100, DOWN)
        self.assertEqual(c.active, "wired")
        c = self.controller()
        c.select_uplink(UP, "203.0.113.2", True, 100, DOWN)
        self.assertEqual(c.active, "wired")

    def test_gateway_is_recovered_from_dhcp_even_if_live_default_is_missing(self):
        c = self.controller()

        def nm(args, timeout=8):
            return subprocess.CompletedProcess(
                args,
                0,
                (
                    ""
                    if args[2] == "IP4.GATEWAY"
                    else "requested_routers = 1 | routers = 192.0.2.1 | subnet_mask = 255.255.255.0"
                ),
                "",
            )

        fakeip = Mock(return_value=subprocess.CompletedProcess([], 0, "[]", ""))
        with (
            patch("ec20_failover.run", side_effect=nm),
            patch("ec20_failover.ip", fakeip),
        ):
            c.wired_probe_route("192.0.2.2")
        self.assertEqual(c.gw, "192.0.2.1")
        self.assertIn(
            unittest.mock.call(
                "route",
                "replace",
                "table",
                "2021",
                "default",
                "via",
                "192.0.2.1",
                "dev",
                "eth0",
                "onlink",
                "src",
                "192.0.2.2",
                "proto",
                "196",
            ),
            fakeip.call_args_list,
        )

    def test_changed_lease_never_reuses_previous_network_gateway(self):
        c = self.controller()
        empty = subprocess.CompletedProcess([], 0, "", "")
        with (
            patch("ec20_failover.run", return_value=empty),
            patch("ec20_failover.ip") as ip,
        ):
            c.wired_probe_route("198.51.100.2")
        self.assertIsNone(c.gw)
        ip.assert_not_called()

    def test_failed_gateway_query_does_not_mutate_routes(self):
        c = self.controller()
        with (
            patch(
                "ec20_failover.run",
                return_value=subprocess.CompletedProcess([], 1, "", "failed"),
            ),
            patch("ec20_failover.ip") as ip,
        ):
            with self.assertRaises(RuntimeError):
                c.wired_probe_route("192.0.2.2")
        ip.assert_not_called()


if __name__ == "__main__":
    unittest.main()
