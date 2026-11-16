"""Read-only absence/resource proofs and bounded OpenVPN control commands."""

from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import stat
import subprocess
import time

POLICY = "identity-bounded-v2"
CLIENT = "/usr/libexec/openvpn3-linux/openvpn3-service-client"


def bus_owner(bus, name):
    try:
        return str(bus.get_name_owner(name))
    except Exception as error:
        if (
            not hasattr(error, "get_dbus_name")
            or error.get_dbus_name() != "org.freedesktop.DBus.Error.NameHasNoOwner"
        ):
            raise
        # These idle-exiting metadata services are D-Bus activated. Starting a
        # manager is not starting a VPN/backend and cannot create a tunnel.
        bus.start_service_by_name(name)
        return str(bus.get_name_owner(name))


def call_ready(method, *args, timeout=3):
    """A manager can own its bus name before its object is exported. Retry only
    that readiness race, never interpret a D-Bus failure as an empty inventory."""
    deadline = time.monotonic() + 2
    while True:
        try:
            return method(*args, timeout=timeout)
        except Exception as error:
            name = error.get_dbus_name() if hasattr(error, "get_dbus_name") else ""
            if (
                name
                not in (
                    "org.freedesktop.DBus.Error.UnknownMethod",
                    "org.freedesktop.DBus.Error.UnknownObject",
                )
                or time.monotonic() >= deadline
            ):
                raise
            time.sleep(0.1)


@dataclass(frozen=True)
class Backend:
    pid: int
    start: str
    interfaces: frozenset
    remotes: frozenset
    fd_count: int
    fd_limit: int

    @property
    def pressure(self):
        return self.fd_count >= max(1, int(self.fd_limit * 0.75))


@dataclass(frozen=True)
class StartProof:
    option: str
    target: str
    fingerprint: tuple


class CommandTask:
    def __init__(self, command, kind, seconds):
        self.kind = kind
        self.result = None
        self.process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        self.deadline = time.monotonic() + seconds

    def running(self):
        result = self.process.poll()
        if result is not None:
            self.result = result
            return False
        if time.monotonic() >= self.deadline:
            try:
                os.killpg(self.process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.result = self.process.wait()
            return False
        return True


def backend_inventory(remotes, proc=Path("/proc")):
    """Do not infer absence from a failed process/socket inventory."""
    sockets = {}
    for protocol in ("udp", "tcp"):
        for line in (proc / "net" / protocol).read_text().splitlines()[1:]:
            fields = line.split()
            address, port = fields[2].split(":")
            address = socket.inet_ntoa(bytes.fromhex(address)[::-1])
            if fields[3] == "01" and int(port, 16) and address in remotes:
                sockets[fields[9]] = address
    result = []
    for p in proc.iterdir():
        if not p.name.isdigit():
            continue
        try:
            if os.readlink(p / "exe") != CLIENT:
                continue
            start = (p / "stat").read_text().rsplit(")", 1)[1].split()[19]
            fd_paths = list((p / "fd").iterdir())
            interfaces, endpoints = set(), set()
            for fd in fd_paths:
                try:
                    target = os.readlink(fd)
                    if target == "/dev/net/tun":
                        match = re.search(
                            r"^iff:\s*(\S+)", (p / "fdinfo" / fd.name).read_text(), re.M
                        )
                        if match:
                            interfaces.add(match[1])
                    elif target.startswith("socket:[") and target[8:-1] in sockets:
                        endpoints.add(sockets[target[8:-1]])
                except FileNotFoundError:
                    # A closed fd is normal; a vanished backend is handled below.
                    continue
            limits = next(
                l
                for l in (p / "limits").read_text().splitlines()
                if l.startswith("Max open files")
            )
            soft_limit = limits.split()[3]
            limit = int(soft_limit) if soft_limit != "unlimited" else 1048576
            if (p / "stat").read_text().rsplit(")", 1)[1].split()[19] != start:
                raise RuntimeError("backend generation changed")
            result.append(
                Backend(
                    int(p.name),
                    start,
                    frozenset(interfaces),
                    frozenset(endpoints),
                    len(fd_paths),
                    limit,
                )
            )
        except FileNotFoundError:
            # A process that has already exited cannot hold a live tunnel.
            continue
    return result


class RecoveryOps:
    def __init__(
        self,
        vpns,
        maintenance="/run/ec20-failover/vpn-maintenance",
        uplink_interfaces=(),
    ):
        self.vpns = vpns
        self.uplink_interfaces = frozenset(uplink_interfaces)
        self.maintenance_path = Path(maintenance)
        self.cache = None

    def reset(self):
        self.cache = None

    def maintenance(self):
        return self.maintenance_path.exists()

    def inventory(self):
        if self.cache is None:
            self.cache = backend_inventory({v.remote for v in self.vpns})
        return self.cache

    def resource(self, session, vpn):
        if not session.backend_pid:
            return None
        for backend in self.inventory():
            if backend.pid == session.backend_pid and backend.remotes == {vpn.remote}:
                return backend
        return None

    def peer_uses_tunnel(self, peer):
        links = subprocess.run(
            ["ip", "-j", "link", "show", "type", "tun"],
            capture_output=True,
            text=True,
            timeout=3,
            check=True,
        )
        names = {v["ifname"] for v in json.loads(links.stdout)}
        route = subprocess.run(
            ["ip", "-4", "-j", "route", "get", peer],
            capture_output=True,
            text=True,
            timeout=3,
            check=True,
        )
        routes = json.loads(route.stdout)
        if len(routes) != 1:
            raise RuntimeError("peer route is not unique")
        device = routes[0].get("dev")
        return device in names or bool(
            self.uplink_interfaces and device not in self.uplink_interfaces
        )

    def interface_absent(self, interface):
        r = subprocess.run(
            ["ip", "-j", "link", "show"],
            capture_output=True,
            text=True,
            timeout=3,
            check=True,
        )
        return interface not in {v["ifname"] for v in json.loads(r.stdout)}

    def service_busy(self, vpn):
        if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", vpn.profile):
            return False
        r = subprocess.run(
            [
                "systemctl",
                "show",
                "openvpn3-session@" + vpn.profile + ".service",
                "-p",
                "ActiveState",
                "-p",
                "MainPID",
                "-p",
                "ControlPID",
            ],
            capture_output=True,
            text=True,
            timeout=3,
            check=True,
        )
        values = dict(
            line.split("=", 1) for line in r.stdout.splitlines() if "=" in line
        )
        return (
            values.get("ActiveState")
            in ("active", "activating", "deactivating", "reloading")
            or values.get("MainPID", "0") != "0"
            or values.get("ControlPID", "0") != "0"
        )

    def start_target(self, vpn):
        import dbus

        bus = dbus.SystemBus()
        owner = bus_owner(bus, "net.openvpn.v3.configuration")
        manager = bus.get_object(
            owner, "/net/openvpn/v3/configuration", introspect=False
        )
        paths = list(
            call_ready(
                manager.get_dbus_method(
                    "LookupConfigName", "net.openvpn.v3.configuration"
                ),
                vpn.profile,
            )
        )
        if str(bus.get_name_owner("net.openvpn.v3.configuration")) != owner:
            raise RuntimeError("configuration manager changed")
        if len(paths) == 1:
            path = str(paths[0])
            if not re.fullmatch(r"/net/openvpn/v3/configuration/[A-Za-z0-9_]+", path):
                raise RuntimeError("invalid configuration path")
            if vpn.config_path and vpn.config_path != path:
                raise RuntimeError("configuration identity changed")
            return StartProof("--config-path", path, (owner, path))
        if paths or vpn.config_path:
            raise RuntimeError("configuration missing or ambiguous")
        # Autoload profiles can be single-use and disappear from the manager.
        # Only a pinned, root-owned regular file in the existing autoload directory
        # is eligible; no arbitrary import paths or symlinks are accepted.
        path = Path(vpn.profile)
        if path.parent != Path("/etc/openvpn3/autoload") or path.suffix != ".ovpn":
            raise RuntimeError("configuration unavailable")
        for directory in (Path("/etc"), Path("/etc/openvpn3"), path.parent):
            info = directory.lstat()
            if (
                info.st_uid != 0
                or not stat.S_ISDIR(info.st_mode)
                or info.st_mode & 0o022
            ):
                raise RuntimeError("unsafe autoload directory")
        with os.fdopen(
            os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb"
        ) as f:
            info = os.fstat(f.fileno())
            if (
                info.st_uid != 0
                or not stat.S_ISREG(info.st_mode)
                or info.st_mode & 0o022
                or info.st_size > 1048576
            ):
                raise RuntimeError("unsafe autoload configuration")
            digest = hashlib.sha256(f.read(1048577)).hexdigest()
        return StartProof(
            "--config", str(path), (info.st_dev, info.st_ino, info.st_mtime_ns, digest)
        )

    def missing(self, vpn, sessions):
        if any(
            s.profile == vpn.profile
            or (vpn.config_path and s.config_path == vpn.config_path)
            for s in sessions
        ):
            return None, "session-present"
        managed = {s.backend_pid for s in sessions if s.backend_pid}
        for backend in self.inventory():
            if backend.pid in managed:
                continue
            # Even an unregistered process without a TUN can be mid-startup.
            if vpn.remote in backend.remotes or not backend.remotes:
                return None, "backend-present-or-starting"
        if self.peer_uses_tunnel(vpn.peer):
            return None, "peer-tunnel-present"
        if self.service_busy(vpn):
            return None, "service-start-in-progress"
        return self.start_target(vpn), ""

    def start(self, vpn, proof):
        return CommandTask(
            [
                "openvpn3",
                "session-start",
                proof.option,
                proof.target,
                "--timeout",
                "30",
            ],
            "start",
            35,
        )

    def disconnect(self, session):
        return CommandTask(
            [
                "busctl",
                "--system",
                "--timeout=5",
                "call",
                session.owner,
                session.path,
                "net.openvpn.v3.sessions",
                "Disconnect",
            ],
            "retire",
            8,
        )
