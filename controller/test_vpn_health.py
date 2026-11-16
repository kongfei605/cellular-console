import dataclasses
import errno
from enum import Enum
import types
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from vpn_health import (
    Ledger,
    LinuxNetwork,
    OpenVPN,
    Probe,
    RestartTask,
    Session,
    Supervisor,
    VPN,
    backend_endpoints,
    combine,
    load_vpns,
    ping,
)
from ec20_failover import Controller

GOOD = Probe("healthy", "peer-reachable", 42)
BAD = Probe("unhealthy", "peer-no-reply")
UNKNOWN = Probe("unknown", "probe-command-error")
UP = Probe("healthy", "uplink-reachable")
VPN1 = VPN("office", "office-profile", "192.0.2.10", "198.51.100.1", "tun0")
SESSION = Session(
    "/net/openvpn/v3/sessions/abc",
    "office-profile",
    "/net/openvpn/v3/configuration/abc",
    "tun14",
    "192.0.2.10",
    True,
    ":1.50",
)


class HealthTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "ledger.json"
        self.reader, self.net, self.probe, self.launch = (
            Mock(),
            Mock(),
            Mock(return_value=BAD),
            Mock(),
        )
        self.reader.snapshot.return_value = [SESSION]
        self.net.validate.return_value = (True, "")
        self.task = Mock()
        self.task.running.return_value = True
        self.launch.return_value = self.task
        self.s = self.new_supervisor()
        self.now = 100000

    def new_supervisor(self):
        return Supervisor(
            [VPN1], Ledger(self.path), self.reader, self.net, self.probe, self.launch
        )

    def step(self, count=1, uplink=UP):
        for _ in range(count):
            result = self.s.check(uplink, self.now)
            self.now += 20
        return result

    def test_dynamic_interface_profile_identity_and_conservative_guard(self):
        self.probe.return_value = GOOD
        legacy, details = self.step()
        self.assertEqual(details["office"]["interface"], "tun14")
        self.assertEqual(details["office"]["state"], "healthy")
        self.assertFalse(legacy["tun0"])  # Never silently relax hardware guards.
        self.probe.assert_called_once_with("tun14", "198.51.100.1")
        self.launch.assert_not_called()

    def test_unmanaged_reachable_backend_is_display_only(self):
        self.reader.snapshot.return_value = []
        self.net.unmanaged_interface.return_value = ("tun14", "")
        self.probe.return_value = GOOD
        _, details = self.step(100)
        self.assertEqual(details["office"]["state"], "healthy")
        self.assertEqual(details["office"]["recovery"], "manual-session-ownership")
        self.probe.return_value = BAD
        _, details = self.step(100)
        self.assertEqual(details["office"]["state"], "unknown")
        self.assertEqual(details["office"]["consecutive_failures"], 0)
        self.launch.assert_not_called()
        self.assertFalse(self.path.exists())

    def test_unmanaged_fallback_never_uses_another_managed_interface(self):
        self.reader.snapshot.return_value = [
            dataclasses.replace(SESSION, profile="other")
        ]
        self.net.unmanaged_interface.return_value = ("tun14", "")
        self.probe.return_value = GOOD
        _, details = self.step(10)
        self.assertEqual(details["office"]["state"], "unknown")
        self.probe.assert_not_called()

    def test_v23_missing_remote_requires_backend_endpoint_proof(self):
        self.reader.snapshot.return_value = [
            dataclasses.replace(SESSION, remote="", backend_pid=123)
        ]
        self.net.remote_matches.return_value = False
        _, details = self.step(5)
        self.assertEqual(details["office"]["state"], "unknown")
        self.net.remote_matches.return_value = True
        self.probe.return_value = GOOD
        _, details = self.step()
        self.assertEqual(details["office"]["state"], "healthy")
        self.launch.assert_not_called()

    def test_legacy_interface_can_belong_to_another_vpn(self):
        other = dataclasses.replace(
            SESSION, path=SESSION.path + "2", profile="home", interface="tun0"
        )
        self.reader.snapshot.return_value = [SESSION, other]
        self.probe.return_value = GOOD
        self.step()
        self.probe.assert_called_once_with("tun14", VPN1.peer)

    def test_detector_failure_never_restarts_even_after_many_samples(self):
        self.probe.return_value = UNKNOWN
        _, details = self.step(200)
        self.assertEqual(details["office"]["state"], "unknown")
        self.assertEqual(details["office"]["consecutive_failures"], 0)
        self.launch.assert_not_called()
        self.assertFalse(self.path.exists())

    def test_query_failure_and_empty_inventory_never_create_sessions(self):
        for inventory in [[], [SESSION, SESSION]]:
            self.reader.snapshot.return_value = inventory
            self.step(50)
        self.reader.snapshot.side_effect = RuntimeError("bus unavailable")
        self.step(50)
        self.launch.assert_not_called()

    def test_remote_and_route_mismatch_inhibit_recovery(self):
        self.reader.snapshot.return_value = [
            dataclasses.replace(SESSION, remote="192.0.2.99")
        ]
        self.step(20)
        self.probe.assert_not_called()
        self.reader.snapshot.return_value = [SESSION]
        self.net.validate.return_value = False, "peer-route-mismatch"
        _, detail = self.step(20)
        self.assertEqual(detail["office"]["state"], "unknown")
        self.launch.assert_not_called()

    def test_unknown_breaks_consecutive_failure_streak(self):
        self.step(2)
        self.probe.return_value = UNKNOWN
        self.step()
        self.probe.return_value = BAD
        self.step(2)
        self.launch.assert_not_called()
        self.step()
        self.launch.assert_called_once_with(SESSION)

    def test_unhealthy_or_unknown_uplink_blocks_all_vpn_restarts(self):
        self.step(100, UNKNOWN)
        self.step(100, BAD)
        self.launch.assert_not_called()
        self.step(2)
        self.launch.assert_not_called()
        self.step()
        self.launch.assert_called_once()

    def test_single_flight_and_cooldown(self):
        self.step(50)
        self.launch.assert_called_once()
        self.task.running.return_value = False
        self.step(3)
        self.assertEqual(self.launch.call_count, 2)

    def test_revalidation_session_change_or_probe_error_blocks_action(self):
        for changed in [
            dataclasses.replace(SESSION, owner=":1.51"),
            dataclasses.replace(SESSION, interface="tun21"),
        ]:
            self.s = self.new_supervisor()
            self.reader.snapshot.return_value = [SESSION]
            self.step(2)
            self.reader.snapshot.side_effect = [[SESSION], [changed]]
            self.step()
            self.reader.snapshot.side_effect = None
        self.s = self.new_supervisor()
        self.reader.snapshot.return_value = [SESSION]
        self.step(2)
        self.probe.side_effect = [BAD, UNKNOWN]
        self.step()
        self.launch.assert_not_called()

    def test_session_change_and_stale_observation_reset_streak(self):
        self.step(2)
        self.reader.snapshot.return_value = [
            dataclasses.replace(SESSION, path=SESSION.path + "2")
        ]
        self.step(2)
        self.launch.assert_not_called()
        self.now += 60
        self.step(2)
        self.launch.assert_not_called()
        self.step()
        self.launch.assert_called_once()

    def test_transition_and_authentication_never_restart(self):
        for phase in [
            "CONN_CONNECTING",
            "CONN_RECONNECTING",
            "CONN_PAUSED",
            "CONN_AUTH_FAILED",
        ]:
            self.reader.snapshot.return_value = [
                dataclasses.replace(SESSION, connected=False, phase=phase)
            ]
            _, details = self.step(10)
            self.assertEqual(details["office"]["state"], "unknown")
        self.launch.assert_not_called()

    def test_slow_revalidation_cannot_use_stale_evidence(self):
        self.step(2)
        with patch("vpn_health.time.monotonic", side_effect=[0, 26, 26]):
            _, details = self.step()
        self.assertEqual(details["office"]["state"], "unknown")
        self.assertEqual(details["office"]["consecutive_failures"], 0)
        self.launch.assert_not_called()

    def test_failed_launch_still_consumes_persisted_budget(self):
        self.launch.side_effect = OSError(errno.EAGAIN, "resource")
        self.step(3)
        self.assertEqual(len(Ledger(self.path).data["vpns"]["office"]["attempts"]), 1)
        self.s = self.new_supervisor()
        self.now -= 20
        self.step(3)
        self.assertEqual(self.launch.call_count, 1)

    def test_ledger_write_failure_prevents_action(self):
        with patch("vpn_health.atomic_json", side_effect=OSError("disk full")):
            self.step(30)
        self.launch.assert_not_called()

    def test_ledger_persists_budget_across_restarts_and_brief_health(self):
        self.task.running.return_value = False
        self.step(3)
        self.s = self.new_supervisor()
        self.step(3)  # First retry eligible after 60 seconds.
        self.s = self.new_supervisor()
        self.probe.return_value = GOOD
        self.step()
        self.probe.return_value = BAD
        self.step(6)
        self.assertEqual(self.launch.call_count, 3)
        for _ in range(5):
            self.s = self.new_supervisor()
            self.step(10)
        self.assertEqual(self.launch.call_count, 3)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)

    def test_24_hour_cap_and_clock_rollback(self):
        ledger = Ledger(self.path)
        for i in range(6):
            self.assertEqual(ledger.reserve("office", 100000 + i * 4000), "")
        self.assertEqual(
            Ledger(self.path).reserve("office", 125000), "recovery-budget-exhausted"
        )
        self.assertEqual(
            Ledger(self.path).reserve("office", 99000), "clock-moved-backward"
        )

    def test_corrupt_ledger_fails_closed(self):
        for raw in [
            "broken",
            '{"last_time": 0, "vpns": {"office": {}}}',
            '{"last_time": NaN, "vpns": {}}',
        ]:
            self.path.write_text(raw)
            with self.assertRaises(ValueError):
                Ledger(self.path)

    def test_one_unknown_vpn_inhibits_other_vpn_recovery(self):
        self.s.vpns.append(
            dataclasses.replace(VPN1, id="other", profile="missing-profile")
        )
        self.step(20)
        self.launch.assert_not_called()

    def test_shared_interface_is_ambiguous(self):
        self.reader.snapshot.return_value = [
            SESSION,
            dataclasses.replace(SESSION, profile="other"),
        ]
        self.step(20)
        self.launch.assert_not_called()
        self.probe.assert_not_called()


class ProbeTests(unittest.TestCase):
    def result(self, code, stderr="", stdout=""):
        return Mock(return_value=subprocess.CompletedProcess([], code, stdout, stderr))

    def test_exit_codes_and_local_errors(self):
        for code, error in [
            (2, ""),
            (1, "connect: Resource temporarily unavailable"),
            (127, "not found"),
        ]:
            self.assertEqual(
                ping("tun14", "198.51.100.1", self.result(code, error)).state, "unknown"
            )
        self.assertEqual(
            ping("tun14", "198.51.100.1", self.result(1)).state, "unhealthy"
        )
        healthy = ping(
            "tun14", "198.51.100.1", self.result(0, stdout="64 bytes: time=42.1 ms")
        )
        self.assertEqual(healthy.latency_ms, 42.1)

    def test_spawn_timeout_resource_exhaustion_and_missing_binary(self):
        for exc in [
            subprocess.TimeoutExpired("ping", 4),
            OSError(errno.EAGAIN, "resource"),
            FileNotFoundError(),
        ]:
            self.assertEqual(
                ping("tun14", "198.51.100.1", Mock(side_effect=exc)).state, "unknown"
            )

    def test_unknown_uplink_does_not_become_unhealthy(self):
        self.assertEqual(combine([BAD, UNKNOWN]).state, "unknown")
        self.assertEqual(combine([BAD, BAD]).state, "unhealthy")
        self.assertEqual(combine([GOOD, UNKNOWN]).state, "healthy")

    def test_network_query_errors_and_wrong_route_are_unknown(self):
        addresses = subprocess.CompletedProcess(
            [], 0, json.dumps([{"addr_info": [{"scope": "global"}]}]), ""
        )
        wrong = subprocess.CompletedProcess([], 0, '[{"dev":"tun0"}]', "")
        with patch("vpn_health.subprocess.run", side_effect=[addresses, wrong]):
            self.assertEqual(
                LinuxNetwork().validate("tun14", VPN1.peer),
                (False, "peer-route-mismatch"),
            )
        with patch(
            "vpn_health.subprocess.run", side_effect=OSError(errno.EAGAIN, "resource")
        ):
            self.assertFalse(LinuxNetwork().validate("tun14", VPN1.peer)[0])

    def test_restart_is_existing_unique_owner_and_path_only(self):
        with patch("vpn_recovery.subprocess.Popen") as popen:
            RestartTask(SESSION)
        command = popen.call_args.args[0]
        self.assertEqual(
            command[-4:],
            [SESSION.owner, SESSION.path, "net.openvpn.v3.sessions", "Restart"],
        )
        self.assertNotIn("session-start", command)
        self.assertNotIn("systemctl", command)

    def test_backend_identity_requires_executable_tun_and_endpoint(self):
        with tempfile.TemporaryDirectory() as directory:
            proc = Path(directory)
            (proc / "net").mkdir()
            (proc / "net" / "udp").write_text(
                "header\n0: 00000000:AAA 0A0200C0:04AA 01 0 0 0 0 0 999\n"
            )
            (proc / "net" / "tcp").write_text("header\n")
            process = proc / "123"
            (process / "fd").mkdir(parents=True)
            (process / "fdinfo").mkdir()
            (process / "exe").symlink_to(
                "/usr/libexec/openvpn3-linux/openvpn3-service-client"
            )
            (process / "fd" / "7").symlink_to("/dev/net/tun")
            (process / "fdinfo" / "7").write_text("iff:\ttun14\n")
            (process / "fd" / "8").symlink_to("socket:[999]")
            self.assertEqual(
                backend_endpoints("tun14", {VPN1.remote}, proc), {123: {VPN1.remote}}
            )
            self.assertEqual(backend_endpoints("tun21", {VPN1.remote}, proc), {})
            self.assertEqual(
                backend_endpoints("tun14", {"192.0.2.99"}, proc), {123: set()}
            )
            (process / "exe").unlink()
            (process / "exe").symlink_to("/usr/bin/other-process")
            self.assertEqual(backend_endpoints("tun14", {VPN1.remote}, proc), {})

    def test_duplicate_config_identity_rejected(self):
        config = {
            "vpns": [
                dict(
                    id="office",
                    profile="office",
                    remote="192.0.2.10",
                    peer="198.51.100.1",
                    interface="tun0",
                )
            ]
            * 2
        }
        with self.assertRaises(ValueError):
            load_vpns(config)


class DBusReaderTests(unittest.TestCase):
    def test_typed_properties_and_manager_generation(self):
        status_major = Enum("StatusMajor", {"CONNECTION": 2})
        status_minor = Enum("StatusMinor", {"CONN_CONNECTED": 7})
        bus = Mock()
        bus.get_name_owner.return_value = ":1.50"
        manager, session = Mock(), Mock()
        manager.get_dbus_method.return_value.return_value = [SESSION.path]
        session.get_dbus_method.return_value.return_value = dict(
            config_name=SESSION.profile,
            config_path=SESSION.config_path,
            device_name=SESSION.interface,
            connected_to=("udp", SESSION.remote, 1194),
            status=(2, 7, "connected"),
        )
        bus.get_object.side_effect = lambda owner, path, introspect: (
            manager if path == "/net/openvpn/v3/sessions" else session
        )
        modules = {
            "dbus": types.SimpleNamespace(SystemBus=lambda: bus),
            "openvpn3": types.SimpleNamespace(
                StatusMajor=status_major, StatusMinor=status_minor
            ),
        }
        with patch.dict("sys.modules", modules):
            self.assertEqual(OpenVPN().snapshot(), [SESSION])
            p = session.get_dbus_method.return_value.return_value
            del p["connected_to"]
            p["backend_pid"] = 123
            found = OpenVPN().snapshot()[0]
            self.assertEqual(found.remote, "")
            self.assertEqual(found.backend_pid, 123)
            bus.get_name_owner.side_effect = [":1.50", ":1.51"]
            with self.assertRaises(RuntimeError):
                OpenVPN().snapshot()


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.c = Controller.__new__(Controller)
        self.c.config = {"failure_count": 3, "recovery_seconds": 30}
        self.c.active, self.c.failures, self.c.good_since, self.c.last_switch = (
            "wired",
            0,
            None,
            None,
        )

    def test_complete_tick_never_forces_vpn_restart_on_switch_or_renewal(self):
        config = json.loads(
            (
                Path(__file__).resolve().parents[1]
                / "configs/failover-controller.example.json"
            ).read_text()
        )
        with tempfile.TemporaryDirectory() as directory:
            c = Controller(
                config, Path(directory) / "status.json", Path(directory) / "ledger.json"
            )
            reader, network, launch = Mock(), Mock(), Mock()
            reader.snapshot.side_effect = RuntimeError("session detection unavailable")
            c.supervisor = Supervisor(
                [VPN1],
                Ledger(Path(directory) / "ledger.json"),
                reader,
                network,
                Mock(return_value=UNKNOWN),
                launch,
            )
            c.active = "wired"
            c.cellular_devices = Mock(return_value=[Path("/synthetic-usb")])
            c.wired_probe_route = Mock()
            c.gw = "192.0.2.1"
            c.enforce_routes = Mock()
            c.uplink = Mock(side_effect=lambda iface: BAD if iface == c.wired else UP)
            with (
                patch("ec20_failover.addresses", side_effect=lambda iface: "192.0.2.2"),
                patch("ec20_failover.Path.read_text", return_value="1"),
                patch("ec20_failover.time.monotonic") as clock,
                patch("ec20_failover.subprocess.Popen") as spawn,
            ):
                for i in range(100):
                    clock.return_value = 100 + i * 20
                    c.tick()
                self.assertEqual(c.active, "cellular")
                # A replacement PPP address is not permission to restart a VPN.
                with patch("ec20_failover.addresses", return_value="192.0.2.3"):
                    clock.return_value += 20
                    c.tick()
                launch.assert_not_called()
                spawn.assert_not_called()
            status = json.loads((Path(directory) / "status.json").read_text())
            self.assertEqual(status["vpn_status"]["office"]["state"], "unknown")
            self.assertEqual(status["vpn_recovery_policy"], "identity-bounded-v2")
            self.assertFalse((Path(directory) / "ledger.json").exists())

    def test_unknown_uplink_never_switches_or_accumulates_failures(self):
        for i in range(100):
            self.c.select_uplink(UNKNOWN, "192.0.2.2", True, i * 5, cellular=UP)
        self.assertEqual(self.c.active, "wired")
        self.assertEqual(self.c.failures, 0)

    def test_unknown_breaks_failure_and_recovery_continuity(self):
        for i in range(2):
            self.c.select_uplink(BAD, "192.0.2.2", True, i * 5, cellular=UP)
        self.c.select_uplink(UNKNOWN, "192.0.2.2", True, 10, cellular=UP)
        for i in range(2):
            self.c.select_uplink(BAD, "192.0.2.2", True, 15 + i * 5, cellular=UP)
        self.assertEqual(self.c.active, "wired")
        self.c.select_uplink(BAD, "192.0.2.2", True, 25, cellular=UP)
        self.assertEqual(self.c.active, "cellular")
        self.c.select_uplink(UP, "192.0.2.2", True, 30, cellular=UP)
        self.c.select_uplink(UNKNOWN, "192.0.2.2", True, 59, cellular=UP)
        self.c.select_uplink(UP, "192.0.2.2", True, 60, cellular=UP)
        self.assertEqual(self.c.active, "cellular")
        self.c.select_uplink(UP, "192.0.2.2", True, 90, cellular=UP)
        self.assertEqual(self.c.active, "wired")


if __name__ == "__main__":
    unittest.main()
