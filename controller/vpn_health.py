"""Identity-based OpenVPN observations and bounded, independently proven recovery.

D-Bus contract: OpenVPN/openvpn3-linux/docs/dbus/
               dbus-service-net.openvpn.v3.sessions.md
All dependencies that touch the host are injected into Supervisor for tests.
"""

from dataclasses import asdict, dataclass, field, replace
import datetime
import ipaddress
import json
import logging
import math
import os
from pathlib import Path
import re
import socket
import subprocess
import time
from vpn_recovery import CommandTask, RecoveryOps, bus_owner, call_ready

SESSION_PREFIX = "/net/openvpn/v3/sessions/"
IFACE = re.compile(r"^[A-Za-z0-9_.:-]{1,15}$")


@dataclass(frozen=True)
class Probe:
    state: str  # healthy, unhealthy (valid no-reply), unknown (detector failed)
    reason: str
    latency_ms: float | None = None


def ping(interface, peer, run=subprocess.run):
    """iputils exit 1 means no replies; exit 2/local errors are never a fault."""
    try:
        start = time.monotonic()
        r = run(
            ["ping", "-n", "-I", interface, "-c", "1", "-W", "2", peer],
            capture_output=True,
            text=True,
            timeout=4,
            env={**os.environ, "LC_ALL": "C"},
        )
        if r.returncode == 0:
            match = re.search(r"time[=<]([0-9.]+)", r.stdout)
            latency = float(match[1]) if match else (time.monotonic() - start) * 1000
            return Probe("healthy", "peer-reachable", round(latency, 1))
        # Some ping builds include local socket errors with exit 1 as well.
        # Treat ANY diagnostic on stderr as inconclusive, not peer failure.
        if r.returncode == 1 and not r.stderr.strip():
            return Probe("unhealthy", "peer-no-reply")
        return Probe("unknown", "probe-command-error")
    except (OSError, subprocess.SubprocessError):
        return Probe("unknown", "probe-execution-failed")


def combine(probes):
    if any(p.state == "healthy" for p in probes):
        return Probe("healthy", "uplink-reachable")
    if not probes or any(p.state == "unknown" for p in probes):
        return Probe("unknown", "uplink-detection-failed")
    return Probe("unhealthy", "uplink-no-reply")


@dataclass(frozen=True)
class VPN:
    id: str
    profile: str
    remote: str
    peer: str
    legacy_interface: str = ""
    config_path: str = ""
    auto_recover: bool = True


def load_vpns(config):
    result = []
    for value in config["vpns"]:
        profile = value["profile"]
        # Old configurations can migrate without reassigning their guard slots.
        identity = value.get("id", profile)
        if (
            not isinstance(profile, str)
            or not profile
            or not isinstance(identity, str)
            or not identity
        ):
            raise ValueError("VPN identity and profile required")
        remote, peer = str(ipaddress.IPv4Address(value["remote"])), str(
            ipaddress.IPv4Address(value["peer"])
        )
        legacy = value.get("interface", "")
        if legacy and not IFACE.fullmatch(legacy):
            raise ValueError("invalid legacy interface slot")
        config_path = value.get("config_path", "")
        if config_path and not re.fullmatch(
            r"/net/openvpn/v3/configuration/[A-Za-z0-9_]+", config_path
        ):
            raise ValueError("invalid configuration object path")
        auto_recover = value.get("auto_recover", True)
        if type(auto_recover) is not bool:
            raise ValueError("auto_recover must be boolean")
        result.append(
            VPN(identity, profile, remote, peer, legacy, config_path, auto_recover)
        )
    if not 1 <= len(result) <= 8 or len({v.id for v in result}) != len(result):
        raise ValueError("one to eight unique VPN identities required")
    if len({v.profile for v in result}) != len(result):
        raise ValueError("duplicate VPN profile")
    slots = [v.legacy_interface for v in result if v.legacy_interface]
    if len(set(slots)) != len(slots):
        raise ValueError("duplicate legacy guard slot")
    return result


@dataclass(frozen=True)
class Session:
    path: str
    profile: str
    config_path: str
    interface: str
    remote: str
    connected: bool
    owner: str  # unique D-Bus service owner, detects manager replacement
    phase: str = "CONN_CONNECTED"
    backend_pid: int = 0


class OpenVPN:
    """Use typed D-Bus properties, never parse localized CLI status text."""

    def snapshot(self):
        # Imported lazily: unit tests and configuration checks need no system bus.
        import dbus
        from openvpn3 import StatusMajor, StatusMinor

        deadline = time.monotonic() + 8

        def timeout():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("session inventory deadline")
            return min(3, remaining)

        bus = dbus.SystemBus()
        owner = bus_owner(bus, "net.openvpn.v3.sessions")
        manager = bus.get_object(owner, "/net/openvpn/v3/sessions", introspect=False)
        paths = call_ready(
            manager.get_dbus_method(
                "FetchAvailableSessions", "net.openvpn.v3.sessions"
            ),
            timeout=timeout(),
        )
        if len(paths) > 64:
            raise RuntimeError("excessive session inventory")
        sessions = []
        for path in paths:
            obj = bus.get_object(owner, path, introspect=False)
            p = obj.get_dbus_method("GetAll", "org.freedesktop.DBus.Properties")(
                "net.openvpn.v3.sessions", timeout=timeout()
            )
            status = p["status"]
            sessions.append(
                Session(
                    str(path),
                    str(p["config_name"]),
                    str(p["config_path"]),
                    str(p["device_name"]),
                    str(p["connected_to"][1]) if p.get("connected_to") else "",
                    int(status[0]) == StatusMajor.CONNECTION.value
                    and int(status[1]) == StatusMinor.CONN_CONNECTED.value,
                    owner,
                    StatusMinor(int(status[1])).name,
                    int(p.get("backend_pid", 0)),
                )
            )
        if str(bus.get_name_owner("net.openvpn.v3.sessions")) != owner:
            raise RuntimeError("session manager changed during inventory")
        return sessions


@dataclass(frozen=True)
class Observation:
    id: str
    state: str
    reason: str
    interface: str = ""
    session_path: str = ""
    latency_ms: float | None = None
    observed_at: str = ""
    session: Session | None = field(default=None, repr=False, compare=False)
    missing: object = field(default=None, repr=False, compare=False)
    action: str = field(default="restart", repr=False, compare=False)
    backend_generation: str | None = field(default=None, repr=False, compare=False)
    backend_fd_count: int | None = None
    backend_fd_limit: int | None = None

    def public(self):
        return {
            k: v
            for k, v in asdict(self).items()
            if k not in ("session", "missing", "action", "backend_generation")
        }


class LinuxNetwork:
    def validate(self, interface, peer):
        if not IFACE.fullmatch(interface):
            return False, "interface-unresolved"
        try:
            a = subprocess.run(
                ["ip", "-4", "-j", "address", "show", "dev", interface],
                capture_output=True,
                text=True,
                timeout=3,
                check=True,
            )
            devices = json.loads(a.stdout)
            if len(devices) != 1 or not any(
                x.get("scope") == "global" for x in devices[0]["addr_info"]
            ):
                return False, "interface-address-unavailable"
            # Unbound lookup is deliberate: detect a peer routed through ANOTHER VPN.
            r = subprocess.run(
                ["ip", "-4", "-j", "route", "get", peer],
                capture_output=True,
                text=True,
                timeout=3,
                check=True,
            )
            routes = json.loads(r.stdout)
            if len(routes) != 1 or routes[0].get("dev") != interface:
                return False, "peer-route-mismatch"
            return True, ""
        except (OSError, ValueError, KeyError, subprocess.SubprocessError):
            return False, "interface-route-query-failed"

    def remote_matches(self, session, remote, remotes):
        backends = backend_endpoints(session.interface, remotes)
        return (
            len(backends) == 1
            and next(iter(backends.values())) == {remote}
            and (not session.backend_pid or session.backend_pid in backends)
        )

    def unmanaged_interface(self, vpn, remotes):
        """Positive read-only evidence for an orphaned backend; never a restart ID."""
        try:
            route = subprocess.run(
                ["ip", "-4", "-j", "route", "get", vpn.peer],
                capture_output=True,
                text=True,
                timeout=3,
                check=True,
            )
            routes = json.loads(route.stdout)
            if len(routes) != 1:
                return "", "unmanaged-interface-unresolved"
            interface = routes[0].get("dev", "")
            valid, reason = self.validate(interface, vpn.peer)
            if not valid:
                return "", reason
            backends = backend_endpoints(interface, remotes)
            if len(backends) != 1 or next(iter(backends.values())) != {vpn.remote}:
                return "", "unmanaged-backend-ambiguous"
            return interface, ""
        except (OSError, ValueError, KeyError, subprocess.SubprocessError):
            return "", "unmanaged-query-failed"


def backend_endpoints(interface, remotes, proc=Path("/proc")):
    """Cross-check live backend executable, held TUN descriptor and server socket."""
    sockets = {}
    for name in ("udp", "tcp"):
        for row in (proc / "net" / name).read_text().splitlines()[1:]:
            fields = row.split()
            remote, port = fields[2].split(":")
            address = socket.inet_ntoa(bytes.fromhex(remote)[::-1])
            if fields[3] == "01" and int(port, 16) and address in remotes:
                sockets[fields[9]] = address
    matches = {}
    for process in proc.iterdir():
        if not process.name.isdigit():
            continue
        try:
            if (
                os.readlink(process / "exe")
                != "/usr/libexec/openvpn3-linux/openvpn3-service-client"
            ):
                continue
            owned_tunnels, owned_remotes = set(), set()
            for fd in (process / "fd").iterdir():
                target = os.readlink(fd)
                if target == "/dev/net/tun":
                    match = re.search(
                        r"^iff:\s*(\S+)",
                        (process / "fdinfo" / fd.name).read_text(),
                        re.M,
                    )
                    if match:
                        owned_tunnels.add(match[1])
                elif target.startswith("socket:[") and target[8:-1] in sockets:
                    owned_remotes.add(sockets[target[8:-1]])
            if interface in owned_tunnels:
                matches[int(process.name)] = owned_remotes
        except OSError:
            continue
    return matches


def identify(vpn, sessions):
    matches = [
        s
        for s in sessions
        if s.profile == vpn.profile
        and (not vpn.config_path or s.config_path == vpn.config_path)
    ]
    if not matches:
        return None, "session-unresolved"
    if len(matches) != 1:
        return None, "multiple-sessions"
    session = matches[0]
    if not re.fullmatch(SESSION_PREFIX + r"[A-Za-z0-9_]+", session.path):
        return None, "invalid-session-path"
    if session.remote and session.remote != vpn.remote:
        return None, "session-remote-mismatch"
    if not session.owner.startswith(":"):
        return None, "session-owner-unresolved"
    return session, ""


class Ledger:
    """Reserve before spawning; budgets survive controller restart and brief health."""

    def __init__(self, path):
        self.path = Path(path)
        self.data = {"last_time": 0, "vpns": {}}
        if self.path.exists():
            self.data = json.loads(self.path.read_text())
        self.validate()

    def validate(self):
        d = self.data
        if set(d) != {"last_time", "vpns"} or not isinstance(d["vpns"], dict):
            raise ValueError("invalid VPN recovery ledger")
        numbers = [d["last_time"]]
        for record in d["vpns"].values():
            if (
                set(record) - {"attempts", "next_allowed", "pending", "last_retire"}
                or not {"attempts", "next_allowed"} <= set(record)
                or record.get("pending")
                not in (None, "awaiting-absence", "start-claimed")
                or not isinstance(record["attempts"], list)
            ):
                raise ValueError("invalid VPN recovery record")
            if len(record["attempts"]) > 100:
                raise ValueError("oversized recovery record")
            numbers += record["attempts"] + [
                record["next_allowed"],
                record.get("last_retire", 0),
            ]
        if any(
            type(n) not in (float, int) or not math.isfinite(n) or n < 0
            for n in numbers
        ):
            raise ValueError("invalid recovery timestamp")

    def reserve(self, identity, now, rebuild=False):
        if now < self.data["last_time"]:
            return "clock-moved-backward"
        old = self.data["vpns"].get(identity, {"attempts": [], "next_allowed": 0})
        attempts = [t for t in old["attempts"] if now - t < 86400]
        if len(attempts) >= 6 or sum(now - t < 3600 for t in attempts) >= 3:
            return "recovery-budget-exhausted"
        if now < old["next_allowed"]:
            return "recovery-backoff"
        record = {
            "attempts": attempts + [now],
            "next_allowed": now + min(3600, 60 * 2 ** len(attempts)),
            "pending": "awaiting-absence" if rebuild else None,
            "last_retire": now if rebuild else old.get("last_retire", 0),
        }
        data = {"last_time": now, "vpns": {**self.data["vpns"], identity: record}}
        atomic_json(self.path, data, 0o600)
        self.data = data
        return ""

    def claim_rebuild_start(self, identity, now):
        if now < self.data["last_time"]:
            return False, "clock-moved-backward"
        record = self.data["vpns"].get(identity)
        if not record or record.get("pending") != "awaiting-absence":
            return False, ""
        data = {
            "last_time": now,
            "vpns": {
                **self.data["vpns"],
                identity: {**record, "pending": "start-claimed"},
            },
        }
        atomic_json(self.path, data, 0o600)
        self.data = data
        return True, ""

    def healthy(self, identity):
        record = self.data["vpns"].get(identity)
        if record and record.get("pending"):
            data = {
                **self.data,
                "vpns": {**self.data["vpns"], identity: {**record, "pending": None}},
            }
            atomic_json(self.path, data, 0o600)
            self.data = data

    def contain(self, identity, now):
        """Stopping a proven failing backend may proceed after the start budget
        is exhausted. It never grants a new start reservation."""
        if now < self.data["last_time"]:
            return "clock-moved-backward"
        record = self.data["vpns"][identity]
        if now - record.get("last_retire", 0) < 60:
            return "recovery-backoff"
        record = {
            **record,
            "last_retire": now,
            "pending": (
                "awaiting-absence"
                if record.get("pending") == "awaiting-absence"
                else None
            ),
        }
        data = {"last_time": now, "vpns": {**self.data["vpns"], identity: record}}
        atomic_json(self.path, data, 0o600)
        self.data = data
        return ""


def atomic_json(path, value, mode=0o644):
    path = Path(path)
    temporary = path.with_suffix(".tmp")
    with os.fdopen(
        os.open(temporary, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, mode), "w"
    ) as f:
        os.fchmod(f.fileno(), mode)
        json.dump(value, f, indent=2, allow_nan=False)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    temporary.replace(path)


class RestartTask(CommandTask):
    def __init__(self, session):
        # Address the exact D-Bus owner/path rather than starting a configuration.
        # A manager replacement cannot accidentally redirect this action.
        super().__init__(
            [
                "busctl",
                "--system",
                "--timeout=5",
                "call",
                session.owner,
                session.path,
                "net.openvpn.v3.sessions",
                "Restart",
            ],
            "restart",
            8,
        )


class Supervisor:
    def __init__(
        self,
        vpns,
        ledger,
        reader=None,
        network=None,
        probe=ping,
        launch=RestartTask,
        recovery=None,
    ):
        self.vpns, self.ledger = vpns, ledger
        self.reader, self.network = reader or OpenVPN(), network or LinuxNetwork()
        self.probe, self.launch = probe, launch
        self.failures, self.tasks = {}, {}
        self.last_check = None
        self.recovery = recovery or RecoveryOps(vpns)
        self.retry_since = {}

    def observe(self, vpn, sessions, stamp):
        session, reason = identify(vpn, sessions)
        if session is None:
            if (
                reason == "session-unresolved"
                and sum(v.remote == vpn.remote for v in self.vpns) == 1
            ):
                interface, fallback_reason = self.network.unmanaged_interface(
                    vpn, {v.remote for v in self.vpns}
                )
                if interface and not any(s.interface == interface for s in sessions):
                    probe = self.probe(interface, vpn.peer)
                    if probe.state == "healthy":
                        return Observation(
                            vpn.id,
                            "healthy",
                            "peer-reachable-unmanaged",
                            interface,
                            latency_ms=probe.latency_ms,
                            observed_at=stamp,
                        )
                    return Observation(
                        vpn.id,
                        "unknown",
                        (
                            probe.reason
                            if probe.state == "unknown"
                            else "unmanaged-peer-not-confirmed"
                        ),
                        interface,
                        observed_at=stamp,
                    )
                reason = fallback_reason or "unmanaged-interface-ambiguous"
                if not interface and reason not in (
                    "unmanaged-query-failed",
                    "interface-route-query-failed",
                ):
                    proof, absence_reason = self.recovery.missing(vpn, sessions)
                    if proof is not None:
                        return Observation(
                            vpn.id,
                            "unhealthy",
                            "session-missing-confirmed",
                            observed_at=stamp,
                            missing=proof,
                            action="start",
                        )
                    reason = absence_reason or reason
            return Observation(vpn.id, "unknown", reason, observed_at=stamp)
        if not session.remote and not self.network.remote_matches(
            session, vpn.remote, {v.remote for v in self.vpns}
        ):
            # A reconnect can remove the TUN before the backend exits. v23's
            # authoritative backend PID plus its server socket still proves the
            # remote, without guessing from an interface number.
            if self.recovery.resource(session, vpn) is None:
                return Observation(
                    vpn.id,
                    "unknown",
                    "session-remote-unresolved",
                    session.interface,
                    session.path,
                    observed_at=stamp,
                )
        valid, reason = self.network.validate(session.interface, vpn.peer)
        if not valid:
            return Observation(
                vpn.id,
                "unknown",
                reason,
                session.interface,
                session.path,
                observed_at=stamp,
                session=session,
            )
        # Shared interfaces are ambiguous regardless of which profile was queried.
        if sum(s.interface == session.interface for s in sessions) != 1:
            return Observation(
                vpn.id,
                "unknown",
                "shared-session-interface",
                session.interface,
                session.path,
                observed_at=stamp,
            )
        result = self.probe(session.interface, vpn.peer)
        # Connecting/paused/authentication states must not receive a second restart.
        # Actual reachability takes precedence over a stale "disconnected" property.
        if result.state == "unhealthy" and session.phase not in (
            "CONN_CONNECTED",
            "CONN_DISCONNECTED",
            "CONN_FAILED",
        ):
            result = Probe("unknown", "session-transition-or-authentication")
        return Observation(
            vpn.id,
            result.state,
            result.reason,
            session.interface,
            session.path,
            result.latency_ms,
            stamp,
            session,
        )

    def resource_observation(self, vpn, observation, uplink, now):
        o = observation
        if o.session is None:
            self.retry_since.pop(vpn.id, None)
            return o
        try:
            backend = self.recovery.resource(o.session, vpn)
            if backend is None:
                return o
            o = replace(
                o,
                backend_fd_count=backend.fd_count,
                backend_fd_limit=backend.fd_limit,
                backend_generation=backend.start,
            )
            if o.state == "healthy" or uplink.state == "unknown":
                self.retry_since.pop(vpn.id, None)
                return o
            # A missing link is a fact only after an independent successful link
            # inventory. Query errors and mismatched routes never authorize action.
            known_fault = o.reason in (
                "peer-no-reply",
                "session-transition-or-authentication",
                "interface-address-unavailable",
            )
            if o.reason == "interface-route-query-failed":
                known_fault = self.recovery.interface_absent(o.session.interface)
            retrying = o.session.phase in (
                "CONN_INIT",
                "CONN_CONNECTING",
                "CONN_RECONNECTING",
                "CONN_FAILED",
                "CONN_DISCONNECTED",
            )
            if not known_fault or (
                not retrying and o.session.phase != "CONN_CONNECTED"
            ):
                self.retry_since.pop(vpn.id, None)
                return o
            key = (o.session.owner, o.session.path, backend.pid, backend.start)
            previous_key, since = self.retry_since.get(vpn.id, (None, now))
            if previous_key != key:
                since = now
            self.retry_since[vpn.id] = key, since
            limit = 60 if uplink.state == "unhealthy" else 120
            if backend.pressure or (retrying and now - since >= limit):
                reason = (
                    "backend-fd-pressure"
                    if backend.pressure
                    else "backend-reconnect-exhausted"
                )
                return replace(o, state="unhealthy", reason=reason, action="retire")
        except Exception:
            # Failure to read process resources is never a reason to disconnect.
            self.retry_since.pop(vpn.id, None)
        return o

    @staticmethod
    def identity(o):
        if o.missing is not None:
            return ("missing", o.missing)
        if o.session:
            return (
                o.session.owner,
                o.session.path,
                o.interface,
                o.session.backend_pid,
                o.backend_generation,
            )
        return None

    def check(self, uplink, now=None):
        started = time.monotonic()
        now = time.time() if now is None else now
        stamp = datetime.datetime.fromtimestamp(now, datetime.timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%S%z"
        )
        for identity, task in list(self.tasks.items()):
            if not task.running():
                logging.info(
                    "VPN %s recovery command finished result=%s",
                    identity,
                    getattr(task, "result", None),
                )
                del self.tasks[identity]
        if self.last_check is not None and (
            now - self.last_check > 30 or now <= self.last_check
        ):
            self.failures.clear()
            self.retry_since.clear()
        self.last_check = now
        try:
            self.recovery.reset()
            maintenance = self.recovery.maintenance()
            sessions = self.reader.snapshot()
            observations = [
                self.resource_observation(
                    v, self.observe(v, sessions, stamp), uplink, now
                )
                for v in self.vpns
            ]
        except Exception as error:
            logging.warning(
                "VPN inventory or recovery proof unavailable (%s)", type(error).__name__
            )
            maintenance = True
            self.retry_since.clear()
            observations = [
                Observation(v.id, "unknown", "session-query-failed", observed_at=stamp)
                for v in self.vpns
            ]
        # Confirmed absence is unhealthy, distinct from a failed detector. Local
        # detector failures still inhibit recovery across this observation cycle.
        non_detector_states = {
            "session-transition-or-authentication",
            "unmanaged-peer-not-confirmed",
            "backend-present-or-starting",
            "service-start-in-progress",
            "session-present",
        }
        can_recover = uplink.state == "healthy" and all(
            o.state != "unknown" or o.reason in non_detector_states
            for o in observations
        )
        details = {}
        for vpn, o in zip(self.vpns, observations):
            key = self.identity(o)
            previous_key, count = self.failures.get(vpn.id, (None, 0))
            eligible = can_recover or (
                uplink.state == "unhealthy" and o.action == "retire"
            )
            eligible = eligible and not maintenance and vpn.auto_recover
            count = (
                (count + 1 if key == previous_key else 1)
                if o.state == "unhealthy" and eligible
                else 0
            )
            self.failures[vpn.id] = key, count
            reason = (
                "manual-session-ownership"
                if o.state == "healthy" and o.session is None
                else "not-needed" if o.state == "healthy" else "detection-inconclusive"
            )
            if o.state == "healthy":
                try:
                    self.ledger.healthy(vpn.id)
                except OSError:
                    reason = "recovery-check-or-command-failed"
            if maintenance or not vpn.auto_recover:
                reason = "recovery-maintenance"
            elif vpn.id in self.tasks:
                reason = "recovery-in-flight"
                self.failures[vpn.id] = key, 0
            elif o.missing is not None and uplink.state != "healthy":
                reason = "recovery-waiting-uplink"
            elif count >= 3:
                # Serialise mutations across VPNs. The next cycle inventories the
                # just-started backend before considering any further creation.
                if self.tasks:
                    reason = "another-recovery-in-flight"
                else:
                    try:
                        self.recovery.reset()
                        fresh = self.resource_observation(
                            vpn,
                            self.observe(vpn, self.reader.snapshot(), stamp),
                            uplink,
                            now,
                        )
                        if (
                            self.identity(fresh) != key
                            or fresh.state != "unhealthy"
                            or fresh.action != o.action
                            or time.monotonic() - started > 25
                            or self.recovery.maintenance()
                        ):
                            reason = "recovery-revalidation-failed"
                            o = (
                                fresh
                                if fresh.state in ("unknown", "healthy")
                                else replace(
                                    fresh,
                                    state="unknown",
                                    reason="session-changed-or-observation-stale",
                                )
                            )
                            self.failures[vpn.id] = key, 0
                        else:
                            claimed = False
                            if o.action == "start":
                                claimed, reason = self.ledger.claim_rebuild_start(
                                    vpn.id, now
                                )
                            else:
                                reason = ""
                            containment = False
                            if not claimed and not reason:
                                reason = self.ledger.reserve(
                                    vpn.id, now, rebuild=o.action == "retire"
                                )
                                if o.action == "retire" and reason in (
                                    "recovery-budget-exhausted",
                                    "recovery-backoff",
                                ):
                                    reason = self.ledger.contain(vpn.id, now)
                                    containment = not reason
                            if not reason:
                                if o.action == "start":
                                    task = self.recovery.start(vpn, o.missing)
                                    reason = "recovery-start-requested"
                                elif o.action == "retire":
                                    task = self.recovery.disconnect(o.session)
                                    reason = (
                                        "recovery-containment-requested"
                                        if containment
                                        else "recovery-rebuild-requested"
                                    )
                                else:
                                    task = self.launch(o.session)
                                    reason = "recovery-requested"
                                self.tasks[vpn.id] = task
                                self.failures[vpn.id] = key, 0
                                logging.info("VPN %s %s", vpn.id, reason)
                    except Exception as error:
                        logging.warning(
                            "VPN %s recovery failed (%s)", vpn.id, type(error).__name__
                        )
                        reason = "recovery-check-or-command-failed"
                        self.failures[vpn.id] = key, 0
            details[vpn.id] = {
                **o.public(),
                "consecutive_failures": self.failures[vpn.id][1],
                "recovery": reason,
            }
        legacy = {
            v.legacy_interface
            or v.id: details[v.id]["state"] == "healthy"
            and (
                not v.legacy_interface
                or details[v.id]["interface"] == v.legacy_interface
            )
            for v in self.vpns
        }
        return legacy, details
