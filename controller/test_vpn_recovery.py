import dataclasses
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from vpn_health import Ledger, Observation, Probe, Session, Supervisor, VPN
from vpn_recovery import (
    Backend,
    CommandTask,
    RecoveryOps,
    StartProof,
    backend_inventory,
)

GOOD = Probe("healthy", "peer-reachable", 12)
BAD = Probe("unhealthy", "peer-no-reply")
UNKNOWN = Probe("unknown", "probe-command-error")
VPN1 = VPN("office", "office", "192.0.2.10", "198.51.100.1", "tun0")
SESSION = Session(
    "/net/openvpn/v3/sessions/a",
    "office",
    "/net/openvpn/v3/configuration/a",
    "tun0",
    "192.0.2.10",
    True,
    ":1.20",
    backend_pid=123,
)
PROOF = StartProof(
    "--config-path",
    "/net/openvpn/v3/configuration/a",
    (":1.30", "/net/openvpn/v3/configuration/a"),
)
BACKEND = Backend(123, "1000", frozenset({"tun0"}), frozenset({VPN1.remote}), 15, 1024)


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "ledger.json"
        self.reader, self.network, self.ops = Mock(), Mock(), Mock()
        self.reader.snapshot.return_value = []
        self.network.unmanaged_interface.return_value = (
            "",
            "unmanaged-backend-ambiguous",
        )
        self.network.validate.return_value = True, ""
        self.ops.maintenance.return_value = False
        self.ops.missing.return_value = PROOF, ""
        self.ops.resource.return_value = None
        self.ops.interface_absent.return_value = True
        self.probe = Mock(return_value=GOOD)
        self.restart = Mock()
        self.task = Mock(kind="start")
        self.task.running.return_value = True
        self.ops.start.return_value = self.task
        self.retire = Mock(kind="retire")
        self.retire.running.return_value = True
        self.ops.disconnect.return_value = self.retire
        self.now = 100000
        self.new_supervisor()

    def new_supervisor(self):
        self.s = Supervisor(
            [VPN1],
            Ledger(self.path),
            self.reader,
            self.network,
            self.probe,
            self.restart,
            self.ops,
        )

    def step(self, n=1, uplink=GOOD):
        for _ in range(n):
            result = self.s.check(uplink, self.now)
            self.now += 20
        return result

    def test_confirmed_absence_starts_once_after_three_fresh_samples(self):
        self.step(2)
        self.ops.start.assert_not_called()
        _, d = self.step(100)
        self.ops.start.assert_called_once_with(VPN1, PROOF)
        self.restart.assert_not_called()
        self.ops.disconnect.assert_not_called()
        self.assertEqual(d["office"]["consecutive_failures"], 0)
        self.assertEqual(len(Ledger(self.path).data["vpns"]["office"]["attempts"]), 1)

    def test_failed_query_never_means_absent_or_starts(self):
        self.reader.snapshot.side_effect = OSError("query error")
        self.step(100)
        self.ops.start.assert_not_called()
        self.ops.missing.assert_not_called()
        self.assertFalse(self.path.exists())

    def test_absence_proof_failure_and_invisible_backend_block_creation(self):
        for why in [
            "backend-present-or-starting",
            "peer-tunnel-present",
            "service-start-in-progress",
        ]:
            self.ops.missing.return_value = None, why
            self.step(100)
        self.ops.missing.side_effect = OSError("process query failed")
        self.step(100)
        self.ops.start.assert_not_called()

    def test_unknown_or_down_upstream_never_creates(self):
        self.step(100, UNKNOWN)
        self.step(100, BAD)
        self.ops.start.assert_not_called()
        self.step(2)
        self.ops.start.assert_not_called()
        self.step()
        self.ops.start.assert_called_once()

    def test_revalidation_detects_new_session_config_or_backend(self):
        self.step(2)
        self.reader.snapshot.side_effect = [[], [SESSION]]
        self.step()
        self.ops.start.assert_not_called()
        self.reader.snapshot.side_effect = None
        self.new_supervisor()
        self.step(2)
        self.ops.missing.side_effect = [
            (PROOF, ""),
            (None, "backend-present-or-starting"),
        ]
        self.step()
        self.ops.start.assert_not_called()

    def test_changed_configuration_resets_absence_streak(self):
        self.step(2)
        self.ops.missing.return_value = (
            dataclasses.replace(PROOF, fingerprint=("new-generation",)),
            "",
        )
        self.step(2)
        self.ops.start.assert_not_called()
        self.step()
        self.ops.start.assert_called_once()

    def test_maintenance_and_per_vpn_opt_out_block_all_actions(self):
        self.ops.maintenance.return_value = True
        self.step(100)
        self.ops.maintenance.return_value = False
        self.s.vpns[0] = dataclasses.replace(VPN1, auto_recover=False)
        self.step(100)
        self.ops.start.assert_not_called()
        self.ops.disconnect.assert_not_called()

    def test_retry_budget_survives_restart_and_task_failure(self):
        self.ops.start.side_effect = OSError("spawn failed")
        self.step(3)
        self.new_supervisor()
        self.now -= 20
        self.step(3)
        self.assertEqual(self.ops.start.call_count, 1)
        self.step(20)
        self.assertEqual(self.ops.start.call_count, 3)
        self.new_supervisor()
        self.step(20)
        self.assertEqual(self.ops.start.call_count, 3)

    def session_retrying(self, fd=15):
        self.reader.snapshot.return_value = [
            dataclasses.replace(SESSION, connected=False, phase="CONN_RECONNECTING")
        ]
        self.ops.resource.return_value = dataclasses.replace(BACKEND, fd_count=fd)
        self.probe.return_value = BAD

    def test_fd_pressure_retires_only_exact_failed_backend(self):
        self.session_retrying(800)
        self.step(2)
        self.ops.disconnect.assert_not_called()
        _, d = self.step(100)
        self.ops.disconnect.assert_called_once_with(
            self.reader.snapshot.return_value[0]
        )
        self.ops.start.assert_not_called()
        self.restart.assert_not_called()
        self.assertEqual(d["office"]["backend_fd_count"], 800)
        self.assertEqual(
            Ledger(self.path).data["vpns"]["office"]["pending"], "awaiting-absence"
        )

    def test_healthy_or_detector_failed_session_never_recycled(self):
        self.session_retrying(1000)
        self.probe.return_value = GOOD
        self.step(100)
        self.probe.return_value = UNKNOWN
        self.step(100)
        self.ops.disconnect.assert_not_called()

    def test_unknown_upstream_prevents_resource_action(self):
        self.session_retrying(1000)
        self.step(100, UNKNOWN)
        self.ops.disconnect.assert_not_called()

    def test_long_retry_while_down_is_retired_but_not_started(self):
        self.session_retrying()
        self.step(5, BAD)  # threshold starts at 60 seconds; requires 3 valid faults.
        self.ops.disconnect.assert_not_called()
        self.step(1, BAD)
        self.ops.disconnect.assert_called_once()
        self.reader.snapshot.return_value = []
        self.retire.running.return_value = False
        self.step(100, BAD)
        self.ops.start.assert_not_called()
        self.step(3, GOOD)
        self.ops.start.assert_called_once()
        # Stop+start is ONE persistent recovery reservation.
        self.assertEqual(len(Ledger(self.path).data["vpns"]["office"]["attempts"]), 1)

    def test_long_retry_with_good_upstream_has_longer_grace(self):
        self.session_retrying()
        self.step(8)
        self.ops.disconnect.assert_not_called()
        self.step()
        self.ops.disconnect.assert_called_once()

    def test_rebuild_survives_controller_restart_and_claim_is_one_time(self):
        self.session_retrying(800)
        self.step(3)
        self.reader.snapshot.return_value = []
        self.new_supervisor()
        self.step(3)
        self.ops.start.assert_called_once()
        ledger = Ledger(self.path)
        self.assertEqual(len(ledger.data["vpns"]["office"]["attempts"]), 1)
        self.assertEqual(ledger.data["vpns"]["office"]["pending"], "start-claimed")
        self.new_supervisor()
        self.now -= 20
        self.step(3)
        # A future retry must spend a NEW reservation, never replay the old claim.
        self.assertEqual(len(Ledger(self.path).data["vpns"]["office"]["attempts"]), 2)

    def test_budget_exhaustion_still_contains_failing_backend_without_free_start(self):
        ledger = Ledger(self.path)
        for when in [99000, 99300, 99600]:
            self.assertEqual(ledger.reserve("office", when), "")
        self.new_supervisor()
        self.session_retrying(1000)
        _, details = self.step(3)
        self.ops.disconnect.assert_called_once()
        self.assertEqual(
            details["office"]["recovery"], "recovery-containment-requested"
        )
        self.assertEqual(len(Ledger(self.path).data["vpns"]["office"]["attempts"]), 3)
        self.retire.running.return_value = False
        self.reader.snapshot.return_value = []
        self.step(50)
        self.ops.start.assert_not_called()
        self.assertIsNone(Ledger(self.path).data["vpns"]["office"]["pending"])

    def test_resource_revalidation_healthy_or_changed_backend_cancels(self):
        self.session_retrying(800)
        self.step(2)
        self.probe.side_effect = [BAD, GOOD]
        self.step()
        self.ops.disconnect.assert_not_called()
        self.probe.side_effect = None
        self.new_supervisor()
        self.step(2)
        self.reader.snapshot.side_effect = [
            [dataclasses.replace(SESSION, connected=False, phase="CONN_RECONNECTING")],
            [
                dataclasses.replace(
                    SESSION, backend_pid=999, connected=False, phase="CONN_RECONNECTING"
                )
            ],
        ]
        self.step()
        self.ops.disconnect.assert_not_called()

    def test_missing_tun_can_be_proven_but_route_query_error_cannot(self):
        self.session_retrying(800)
        self.network.validate.return_value = False, "interface-route-query-failed"
        self.ops.interface_absent.return_value = False
        self.step(100)
        self.ops.disconnect.assert_not_called()
        self.ops.interface_absent.return_value = True
        self.step(3)
        self.ops.disconnect.assert_called_once()

    def test_no_create_if_old_backend_remains_after_disconnect(self):
        self.session_retrying(800)
        self.step(3)
        self.retire.running.return_value = False
        self.reader.snapshot.return_value = []
        self.ops.missing.return_value = None, "backend-present-or-starting"
        self.step(100)
        self.ops.start.assert_not_called()

    def test_multiple_missing_vpns_start_serially(self):
        other = dataclasses.replace(
            VPN1, id="home", profile="home", remote="192.0.2.11"
        )
        self.s.vpns.append(other)
        self.step(3)
        self.assertEqual(self.ops.start.call_count, 1)
        self.task.running.return_value = False
        # A healthy first tunnel plus confirmed absence of the second.
        self.reader.snapshot.return_value = [SESSION]
        self.step()
        self.assertEqual(self.ops.start.call_count, 2)

    def test_starting_other_profile_does_not_starve_confirmed_missing_vpn(self):
        other = dataclasses.replace(
            VPN1, id="home", profile="home", remote="192.0.2.11"
        )
        self.s.vpns.append(other)
        self.reader.snapshot.return_value = [
            dataclasses.replace(SESSION, connected=False, phase="CONN_CONNECTING")
        ]
        self.probe.return_value = BAD
        self.step(3)
        self.ops.start.assert_called_once_with(other, PROOF)


class ProofTests(unittest.TestCase):
    def setUp(self):
        self.ops = RecoveryOps([VPN1])
        self.ops.inventory = Mock(return_value=[])
        self.ops.peer_uses_tunnel = Mock(return_value=False)
        self.ops.service_busy = Mock(return_value=False)
        self.ops.start_target = Mock(return_value=PROOF)

    def test_absence_requires_all_independent_evidence(self):
        self.assertEqual(self.ops.missing(VPN1, []), (PROOF, ""))
        self.assertIsNone(self.ops.missing(VPN1, [SESSION])[0])
        self.ops.inventory.return_value = [BACKEND]
        self.assertIsNone(self.ops.missing(VPN1, [])[0])
        self.ops.inventory.return_value = [
            dataclasses.replace(BACKEND, interfaces=frozenset(), remotes=frozenset())
        ]
        self.assertIsNone(self.ops.missing(VPN1, [])[0])
        self.ops.inventory.return_value = []
        self.ops.peer_uses_tunnel.return_value = True
        self.assertIsNone(self.ops.missing(VPN1, [])[0])
        self.ops.peer_uses_tunnel.return_value = False
        self.ops.service_busy.return_value = True
        self.assertIsNone(self.ops.missing(VPN1, [])[0])

    def test_inventory_failure_is_not_absence(self):
        self.ops.inventory.side_effect = PermissionError("proc unavailable")
        with self.assertRaises(PermissionError):
            self.ops.missing(VPN1, [])

    def test_resource_requires_pinned_pid_and_server(self):
        self.ops.inventory.return_value = [BACKEND]
        self.assertEqual(self.ops.resource(SESSION, VPN1), BACKEND)
        self.assertIsNone(
            self.ops.resource(dataclasses.replace(SESSION, backend_pid=999), VPN1)
        )
        self.ops.inventory.return_value = [
            dataclasses.replace(BACKEND, remotes=frozenset({"192.0.2.99"}))
        ]
        self.assertIsNone(self.ops.resource(SESSION, VPN1))

    def test_commands_are_bounded_and_never_restart_shared_services(self):
        with patch("vpn_recovery.subprocess.Popen") as popen:
            task = self.ops.start(VPN1, PROOF)
            self.assertEqual(
                popen.call_args.args[0],
                [
                    "openvpn3",
                    "session-start",
                    "--config-path",
                    PROOF.target,
                    "--timeout",
                    "30",
                ],
            )
            self.ops.disconnect(SESSION)
            self.assertEqual(
                popen.call_args.args[0][-4:],
                [SESSION.owner, SESSION.path, "net.openvpn.v3.sessions", "Disconnect"],
            )
            popen.return_value.poll.return_value = None
            popen.return_value.pid = 123
            task.deadline = 0
            with patch("vpn_recovery.os.killpg") as kill:
                self.assertFalse(task.running())
                kill.assert_called_once()

    def test_peer_route_via_another_tunnel_cannot_prove_absence(self):
        ops = RecoveryOps([VPN1], uplink_interfaces=("eth0", "ppp0"))
        responses = [
            subprocess.CompletedProcess([], 0, "[]", ""),
            subprocess.CompletedProcess([], 0, '[{"dev":"wg0"}]', ""),
        ]
        with patch("vpn_recovery.subprocess.run", side_effect=responses):
            self.assertTrue(ops.peer_uses_tunnel(VPN1.peer))
        responses = [
            subprocess.CompletedProcess([], 0, "[]", ""),
            subprocess.CompletedProcess([], 0, '[{"dev":"ppp0"}]', ""),
        ]
        with patch("vpn_recovery.subprocess.run", side_effect=responses):
            self.assertFalse(ops.peer_uses_tunnel(VPN1.peer))

    def test_old_ledger_migrates_without_losing_attempts_and_clears_pending_on_success(
        self,
    ):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "ledger.json"
            path.write_text(
                json.dumps(
                    {
                        "last_time": 1000,
                        "vpns": {"office": {"attempts": [1000], "next_allowed": 1060}},
                    }
                )
            )
            ledger = Ledger(path)
            self.assertEqual(ledger.reserve("office", 2000, rebuild=True), "")
            self.assertEqual(ledger.data["vpns"]["office"]["attempts"], [1000, 2000])
            self.assertEqual(
                ledger.claim_rebuild_start("office", 1999),
                (False, "clock-moved-backward"),
            )
            self.assertEqual(ledger.claim_rebuild_start("office", 2100), (True, ""))
            self.assertEqual(
                Ledger(path).claim_rebuild_start("office", 2200), (False, "")
            )
            Ledger(path).healthy("office")
            self.assertIsNone(Ledger(path).data["vpns"]["office"]["pending"])


class ConfigAndInventoryTests(unittest.TestCase):
    def bus(self, paths):
        import types

        bus, manager = Mock(), Mock()
        bus.get_name_owner.return_value = ":1.30"
        bus.get_object.return_value = manager
        manager.get_dbus_method.return_value.return_value = paths
        return bus, {"dbus": types.SimpleNamespace(SystemBus=lambda: bus)}

    def test_imported_configuration_requires_unique_exact_generation(self):
        ops = RecoveryOps([VPN1])
        path = "/net/openvpn/v3/configuration/a"
        bus, modules = self.bus([path])
        with patch.dict("sys.modules", modules):
            self.assertEqual(ops.start_target(VPN1), PROOF)
            bus.get_object.return_value.get_dbus_method.return_value.return_value = [
                path,
                path + "2",
            ]
            with self.assertRaises(RuntimeError):
                ops.start_target(VPN1)
            bus.get_object.return_value.get_dbus_method.return_value.return_value = [
                path
            ]
            with self.assertRaises(RuntimeError):
                ops.start_target(dataclasses.replace(VPN1, config_path=path + "wrong"))
            bus.get_name_owner.side_effect = [":1.30", ":1.31"]
            with self.assertRaises(RuntimeError):
                ops.start_target(VPN1)

    def test_missing_named_configuration_cannot_import_arbitrary_paths(self):
        ops = RecoveryOps([VPN1])
        bus, modules = self.bus([])
        with patch.dict("sys.modules", modules):
            for profile in [
                "missing-name",
                "/tmp/profile.ovpn",
                "/etc/openvpn3/autoload/../other.ovpn",
            ]:
                with self.assertRaises(RuntimeError):
                    ops.start_target(dataclasses.replace(VPN1, profile=profile))

    def test_autoload_requires_root_regular_file_and_nonwritable_directories(self):
        import os, stat, types

        ops = RecoveryOps([VPN1])
        bus, modules = self.bus([])
        vpn = dataclasses.replace(VPN1, profile="/etc/openvpn3/autoload/test.ovpn")
        directory = types.SimpleNamespace(st_uid=0, st_mode=stat.S_IFDIR | 0o755)
        with tempfile.TemporaryFile() as config:
            config.write(b"client\n")
            config.flush()
            real = os.fstat(config.fileno())
            info = types.SimpleNamespace(
                st_uid=0,
                st_mode=stat.S_IFREG | 0o600,
                st_size=7,
                st_dev=real.st_dev,
                st_ino=real.st_ino,
                st_mtime_ns=real.st_mtime_ns,
            )
            with (
                patch.dict("sys.modules", modules),
                patch("vpn_recovery.Path.lstat", return_value=directory),
            ):
                for uid, mode in [
                    (1000, stat.S_IFREG | 0o600),
                    (0, stat.S_IFREG | 0o666),
                    (0, stat.S_IFDIR | 0o700),
                ]:
                    info.st_uid, info.st_mode = uid, mode
                    fd = os.dup(config.fileno())
                    with (
                        patch("vpn_recovery.os.open", return_value=fd),
                        patch("vpn_recovery.os.fstat", return_value=info),
                    ):
                        with self.assertRaises(RuntimeError):
                            ops.start_target(vpn)
                info.st_uid, info.st_mode = 0, stat.S_IFREG | 0o600
                config.seek(0)
                fd = os.dup(config.fileno())
                with (
                    patch("vpn_recovery.os.open", return_value=fd),
                    patch("vpn_recovery.os.fstat", return_value=info),
                ):
                    proof = ops.start_target(vpn)
                    self.assertEqual(proof.option, "--config")
                    self.assertEqual(len(proof.fingerprint[-1]), 64)
                directory.st_mode = stat.S_IFDIR | 0o777
                with self.assertRaises(RuntimeError):
                    ops.start_target(vpn)

    def test_idle_manager_activation_never_starts_a_backend(self):
        from vpn_recovery import bus_owner

        class NoOwner(Exception):
            def get_dbus_name(self):
                return "org.freedesktop.DBus.Error.NameHasNoOwner"

        bus = Mock()
        bus.get_name_owner.side_effect = [NoOwner(), ":1.30"]
        self.assertEqual(bus_owner(bus, "net.openvpn.v3.configuration"), ":1.30")
        bus.start_service_by_name.assert_called_once_with(
            "net.openvpn.v3.configuration"
        )
        bus = Mock()
        bus.get_name_owner.side_effect = PermissionError("denied")
        with self.assertRaises(PermissionError):
            bus_owner(bus, "net.openvpn.v3.sessions")
        bus.start_service_by_name.assert_not_called()

    def test_process_without_tun_is_still_present_and_prevents_creation(self):
        with tempfile.TemporaryDirectory() as d:
            proc = Path(d)
            (proc / "net").mkdir()
            (proc / "net" / "udp").write_text(
                "header\n0: 00000000:AAAA 0A0200C0:04AA 01 0 0 0 0 0 999\n"
            )
            (proc / "net" / "tcp").write_text("header\n")
            p = proc / "123"
            (p / "fd").mkdir(parents=True)
            (p / "exe").symlink_to(
                "/usr/libexec/openvpn3-linux/openvpn3-service-client"
            )
            (p / "stat").write_text(
                "123 (openvpn3 client) "
                + " ".join(["S"] + ["0"] * 18 + ["12345"] + ["0"] * 5)
            )
            (p / "limits").write_text(
                "Max open files            1024                 524288               files\n"
            )
            (p / "fd" / "8").symlink_to("socket:[999]")
            found = backend_inventory({VPN1.remote}, proc)
            self.assertEqual(len(found), 1)
            self.assertEqual(found[0].start, "12345")
            self.assertEqual(found[0].interfaces, frozenset())
            self.assertEqual(found[0].remotes, {VPN1.remote})
            self.assertEqual(found[0].fd_limit, 1024)
            ops = RecoveryOps([VPN1])
            ops.inventory = Mock(return_value=found)
            self.assertEqual(
                ops.missing(VPN1, []), (None, "backend-present-or-starting")
            )


class ManagerReadinessTests(unittest.TestCase):
    def test_only_startup_object_export_races_are_retried(self):
        from vpn_recovery import call_ready

        class Startup(Exception):
            def get_dbus_name(self):
                return "org.freedesktop.DBus.Error.UnknownMethod"

        method = Mock(side_effect=[Startup(), Startup(), ["config-path"]])
        with patch("vpn_recovery.time.sleep"):
            self.assertEqual(call_ready(method), ["config-path"])
        self.assertEqual(method.call_count, 3)
        method = Mock(side_effect=PermissionError("denied"))
        with self.assertRaises(PermissionError):
            call_ready(method)
        self.assertEqual(method.call_count, 1)


class RealProcessRecoveryCycle(unittest.TestCase):
    def test_missing_start_healthy_exit_controller_restart_and_recreation(self):
        import sys, time

        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "ledger.json"
            marker = Path(directory) / "starts.txt"
            tasks = []
            reader, net, ops = Mock(), Mock(), Mock()
            reader.snapshot.side_effect = lambda: (
                [SESSION] if tasks and tasks[-1].process.poll() is None else []
            )
            net.validate.return_value = True, ""
            net.unmanaged_interface.return_value = "", "unmanaged-backend-ambiguous"
            ops.maintenance.return_value = False
            ops.resource.return_value = None
            ops.missing.return_value = PROOF, ""

            def start(vpn, proof):
                task = CommandTask(
                    [
                        sys.executable,
                        "-c",
                        "from pathlib import Path;import sys,time;"
                        'f=Path(sys.argv[1]).open("a");f.write("start\\n");f.close();time.sleep(30)',
                        str(marker),
                    ],
                    "start",
                    35,
                )
                tasks.append(task)
                return task

            ops.start.side_effect = start
            restart = Mock()
            probe = Mock(return_value=GOOD)

            def supervisor():
                return Supervisor(
                    [VPN1], Ledger(ledger), reader, net, probe, restart, ops
                )

            def wait_marker(n):
                end = time.monotonic() + 2
                while time.monotonic() < end:
                    if marker.exists() and len(marker.read_text().splitlines()) >= n:
                        return
                    time.sleep(0.01)
                self.fail("backend process did not start")

            try:
                s = supervisor()
                for now in [100000, 100020, 100040]:
                    s.check(GOOD, now)
                wait_marker(1)
                for now in [100060, 100080]:
                    _, detail = s.check(GOOD, now)
                    self.assertEqual(detail["office"]["state"], "healthy")
                self.assertEqual(len(tasks), 1)
                tasks[-1].process.terminate()
                tasks[-1].process.wait()
                s = supervisor()  # All transient task state is lost, ledger remains.
                for now in [100100, 100120, 100140]:
                    s.check(GOOD, now)
                wait_marker(2)
                self.assertEqual(marker.read_text().splitlines(), ["start", "start"])
                self.assertEqual(
                    len(Ledger(ledger).data["vpns"]["office"]["attempts"]), 2
                )
                restart.assert_not_called()
                ops.disconnect.assert_not_called()
            finally:
                for task in tasks:
                    if task.process.poll() is None:
                        task.process.kill()
                    task.process.wait()


if __name__ == "__main__":
    unittest.main()
