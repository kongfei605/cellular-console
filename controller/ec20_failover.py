#!/usr/bin/env python3
"""Optional local source for the independent wired/EC20 controller.

No installation is performed by this repository. --check validates only;
route/VPN control requires an explicit --run on a root Linux host.
"""

import argparse
import concurrent.futures
import fcntl
import ipaddress
import json
import logging
import os
import re
from pathlib import Path
import subprocess
import sys
import time

from vpn_recovery import POLICY, RecoveryOps
from vpn_health import (
    IFACE,
    Ledger,
    Probe,
    Supervisor,
    atomic_json,
    combine,
    load_vpns,
    ping,
)


def run(args, timeout=8):
    return subprocess.run(
        args,
        capture_output=True,
        text=True,
        timeout=timeout,
        env={**os.environ, "LC_ALL": "C"},
    )


def ip(*args):
    r = run(["ip", "-4", *args])
    if r.returncode:
        raise RuntimeError("route query/update failed")
    return r


def addresses(iface):
    r = run(["ip", "-4", "-j", "address", "show", "dev", iface])
    if r.returncode:
        if not Path("/sys/class/net", iface).exists():
            return None
        raise RuntimeError("interface query failed")
    for dev in json.loads(r.stdout):
        for address in dev.get("addr_info", []):
            if address.get("scope") == "global":
                return address["local"]
    return None


def load_config(path):
    config = json.loads(Path(path).read_text())
    for name in ("wired_interface", "cell_interface"):
        if not IFACE.fullmatch(config[name]):
            raise ValueError("invalid interface")
    if config["wired_interface"] == config["cell_interface"]:
        raise ValueError("uplink interfaces must differ")
    for name, minimum, maximum in [
        ("interval_seconds", 5, 30),
        ("failure_count", 3, 100),
        ("recovery_seconds", 30, 3600),
    ]:
        if type(config[name]) is not int or not minimum <= config[name] <= maximum:
            raise ValueError("invalid controller timing")
    if not 1 <= len(config["probe_targets"]) <= 4:
        raise ValueError("one to four uplink probe targets required")
    for target in config["probe_targets"]:
        ipaddress.IPv4Address(target)
    if not config.get("cell_usb_ids") or not config.get("cell_uuid"):
        raise ValueError("cellular USB identities and connection UUID required")
    for pair in config["cell_usb_ids"]:
        if len(pair) != 2 or any(
            len(v) != 4 or any(c not in "0123456789abcdefABCDEF" for c in v)
            for v in pair
        ):
            raise ValueError("invalid USB identity")
    load_vpns(config)
    return config


class Controller:
    def __init__(self, config, status, ledger):
        self.config, self.status = config, Path(status)
        self.wired, self.cell = config["wired_interface"], config["cell_interface"]
        vpns = load_vpns(config)
        self.supervisor = Supervisor(
            vpns,
            Ledger(ledger),
            recovery=RecoveryOps(vpns, uplink_interfaces=(self.wired, self.cell)),
        )
        previous = json.loads(self.status.read_text()) if self.status.exists() else {}
        self.active = (
            previous.get("active")
            if previous.get("active") in ("wired", "cellular")
            else None
        )
        self.failures, self.good_since = 0, None
        self.last_vpn_check = None
        self.last_dial, self.dial_process = 0, None
        self.dial_attempts = previous.get("cellular_dial_attempts", 0)
        self.cell_present = None
        self.last_switch = previous.get("last_switch")
        self.gw = None
        self.gateway_address = None
        self.vpn_health, self.vpn_status = {}, {}

    def cellular_devices(self):
        wanted = {
            tuple(v.lower() for v in pair) for pair in self.config["cell_usb_ids"]
        }
        devices = []
        for dev in Path("/sys/bus/usb/devices").iterdir():
            try:
                identity = (
                    (dev / "idVendor").read_text().strip().lower(),
                    (dev / "idProduct").read_text().strip().lower(),
                )
                if (
                    identity in wanted
                    and (dev / "authorized").read_text().strip() == "1"
                ):
                    devices.append(dev.resolve())
            except OSError:
                continue
        return devices

    def cellular_modem_state(self, devices):
        r = run(["nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device", "status"])
        if r.returncode:
            raise RuntimeError("modem inventory unavailable")
        for line in r.stdout.splitlines():
            parts = line.split(":", 2)
            if len(parts) != 3 or parts[1] != "gsm":
                continue
            path = (Path("/sys/class/tty") / parts[0] / "device").resolve()
            if any(dev in path.parents for dev in devices):
                return parts[2]
        return None

    def uplink(self, iface):
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            return combine(
                list(
                    pool.map(
                        lambda target: ping(iface, target), self.config["probe_targets"]
                    )
                )
            )

    def wired_probe_route(self, address):
        if not address:
            self.gw, self.gateway_address = None, None
            return
        result = run(["nmcli", "-g", "IP4.GATEWAY", "device", "show", self.wired])
        if result.returncode:
            raise RuntimeError("wired gateway query failed")
        gateway = result.stdout.strip().splitlines()
        candidate = gateway[0] if gateway and gateway[0] not in ("", "--") else None
        if not candidate:
            # IP4.GATEWAY reflects live kernel routes and becomes empty if a
            # previous controller deleted the DHCP default. The lease remains
            # authoritative even while cellular is preferred.
            lease = run(["nmcli", "-g", "DHCP4.OPTION", "device", "show", self.wired])
            if lease.returncode:
                raise RuntimeError("wired DHCP gateway query failed")
            match = re.search(
                r"(?:^|[|\n])\s*routers\s*=\s*([0-9.]+)(?:\s|$)", lease.stdout
            )
            if match:
                candidate = match[1]
        if not candidate and self.config.get("wired_uuid"):
            static = run(
                [
                    "nmcli",
                    "-g",
                    "ipv4.gateway",
                    "connection",
                    "show",
                    self.config["wired_uuid"],
                ]
            )
            if static.returncode:
                raise RuntimeError("static wired gateway query failed")
            candidate = static.stdout.strip() or None
        if not candidate and getattr(self, "gateway_address", None) == address:
            candidate = self.gw
        self.gw = str(ipaddress.IPv4Address(candidate)) if candidate else None
        self.gateway_address = address
        if self.gw:
            ip(
                "route",
                "replace",
                "table",
                "2021",
                "default",
                "via",
                self.gw,
                "dev",
                self.wired,
                "onlink",
                "src",
                address,
                "proto",
                "196",
            )
            rules = json.loads(ip("-j", "rule", "show").stdout)
            for i, target in enumerate(self.config["probe_targets"]):
                priority = 20190 + i
                existing = next(
                    (r for r in rules if r.get("priority") == priority), None
                )
                if existing and existing.get("dst") not in (target, target + "/32"):
                    ip("rule", "del", "priority", str(priority))
                    existing = None
                if not existing:
                    ip(
                        "rule",
                        "add",
                        "priority",
                        str(priority),
                        "to",
                        target + "/32",
                        "oif",
                        self.wired,
                        "lookup",
                        "2021",
                    )

    def vpn_route(self, remote, iface, address, gateway=None):
        routes = json.loads(ip("-j", "route", "show", "exact", remote + "/32").stdout)
        good = []
        for route in routes:
            if route.get("dev") == iface and route.get("gateway") == gateway:
                good.append(route)
                continue
            args = ["route", "del", remote + "/32", "dev", route["dev"]]
            if route.get("gateway"):
                args += ["via", route["gateway"]]
            args += ["metric", str(route.get("metric", 0))]
            if route.get("protocol") is not None:
                args += ["proto", str(route["protocol"])]
            ip(*args)
        if any(str(r.get("protocol")) != "196" for r in good):
            for route in good:
                if str(route.get("protocol")) == "196":
                    ip(
                        "route",
                        "del",
                        remote + "/32",
                        "dev",
                        iface,
                        "metric",
                        str(route.get("metric", 0)),
                        "proto",
                        "196",
                    )
            return
        if good:
            return
        args = [
            "route",
            "replace",
            remote + "/32",
            "dev",
            iface,
            "src",
            address,
            "metric",
            "10",
            "proto",
            "196",
        ]
        if gateway:
            args += ["via", gateway]
        ip(*args)

    def enforce_routes(self, wired_ip, cell_ip):
        # Preserve the NM/DHCP wired default. PPP preference is expressed through
        # an owned lower metric, not by deleting another manager's working route.
        if self.active == "wired" and wired_ip and self.gw:
            ip(
                "route",
                "replace",
                "default",
                "via",
                self.gw,
                "dev",
                self.wired,
                "src",
                wired_ip,
                "metric",
                "100",
                "proto",
                "dhcp",
            )
        metric = 50 if self.active == "cellular" else 700
        if cell_ip:
            ip(
                "route",
                "replace",
                "default",
                "dev",
                self.cell,
                "src",
                cell_ip,
                "metric",
                str(metric),
                "proto",
                "196",
            )
        for route in json.loads(ip("-j", "route", "show", "default").stdout):
            if (
                route.get("dev") == self.cell
                and str(route.get("protocol")) == "196"
                and (not cell_ip or route.get("metric", 0) != metric)
            ):
                ip(
                    "route",
                    "del",
                    "default",
                    "dev",
                    self.cell,
                    "metric",
                    str(route.get("metric", 0)),
                    "proto",
                    "196",
                )
        rules = json.loads(ip("-j", "rule", "show").stdout)
        if self.active == "cellular":
            for i, vpn in enumerate(self.config["vpns"]):
                if not any(r.get("priority") == 20180 + i for r in rules):
                    ip(
                        "rule",
                        "add",
                        "priority",
                        str(20180 + i),
                        "to",
                        vpn["remote"] + "/32",
                        "lookup",
                        "2020",
                    )
            if cell_ip:
                for vpn in self.config["vpns"]:
                    self.vpn_route(vpn["remote"], self.cell, cell_ip)
        elif self.active == "wired" and wired_ip and self.gw:
            for vpn in self.config["vpns"]:
                self.vpn_route(vpn["remote"], self.wired, wired_ip, self.gw)
            for i in range(len(self.config["vpns"])):
                if any(r.get("priority") == 20180 + i for r in rules):
                    ip("rule", "del", "priority", str(20180 + i))

    def select_uplink(self, wired, cell_ip, cell_present, now, cellular=None):
        if wired.state == "healthy":
            self.failures = 0
            if self.good_since is None:
                self.good_since = now
        else:
            # Unknown breaks continuity and must never advance a failure counter.
            self.good_since = None
            self.failures = self.failures + 1 if wired.state == "unhealthy" else 0
        cell_ready = bool(
            cell_present
            and cell_ip
            and cellular is not None
            and cellular.state == "healthy"
        )
        previous = self.active
        if self.active is None:
            self.active = (
                "cellular" if wired.state != "healthy" and cell_ready else "wired"
            )
        elif (
            self.active == "wired"
            and cell_ready
            and self.failures >= self.config["failure_count"]
        ):
            self.active = "cellular"
        elif (
            self.active == "cellular"
            and wired.state == "healthy"
            and (
                not cell_present
                or not cell_ip
                or (cellular is not None and cellular.state == "unhealthy")
                or now - self.good_since >= self.config["recovery_seconds"]
            )
        ):
            self.active = "wired"
        if previous != self.active:
            self.last_switch = time.strftime("%Y-%m-%dT%H:%M:%S%z")
            logging.info("UPLINK %s -> %s", previous, self.active)
        # Uplink/PPP changes never force a VPN restart. Actual evidence goes through
        # the same health streak, identity checks and persistent recovery budget.

    def tick(self):
        now = time.monotonic()
        wired_ip, cell_ip = addresses(self.wired), addresses(self.cell)
        devices = self.cellular_devices()
        cell_present = bool(devices)
        if self.cell_present != cell_present:
            self.cell_present, self.last_dial = cell_present, 0
        if not cell_present:
            cell_ip = None
        try:
            wired_link = (
                Path("/sys/class/net", self.wired, "carrier").read_text().strip() == "1"
            )
        except OSError:
            wired_link = None
        self.wired_probe_route(wired_ip)
        if wired_link is False or not wired_ip:
            wired = Probe("unhealthy", "wired-link-or-address-absent")
        elif wired_link is None or not self.gw:
            wired = Probe("unknown", "wired-link-or-gateway-unresolved")
        else:
            wired = self.uplink(self.wired)
        cellular = (
            self.uplink(self.cell)
            if cell_ip
            else Probe("unhealthy", "cell-address-absent")
        )
        modem_state = None
        if cell_present and not cell_ip:
            modem_state = self.cellular_modem_state(devices)
            dial_running = (
                self.dial_process is not None and self.dial_process.poll() is None
            )
            if (
                modem_state
                and not modem_state.startswith(("connecting", "connected"))
                and not dial_running
                and now - self.last_dial > 35
            ):
                self.last_dial = now
                self.dial_attempts += 1
                self.dial_process = subprocess.Popen(
                    [
                        "timeout",
                        "32",
                        "nmcli",
                        "--wait",
                        "30",
                        "connection",
                        "up",
                        self.config["cell_uuid"],
                    ],
                    stdin=subprocess.DEVNULL,
                )
        self.select_uplink(wired, cell_ip, cell_present, now, cellular)
        self.enforce_routes(wired_ip, cell_ip)
        active_probe = cellular if self.active == "cellular" else wired
        if self.last_vpn_check is None or now - self.last_vpn_check >= 20:
            self.vpn_health, self.vpn_status = self.supervisor.check(active_probe)
            self.last_vpn_check = time.monotonic()
        atomic_json(
            self.status,
            dict(
                active=self.active,
                wired_healthy=wired.state == "healthy",
                wired_link=bool(wired_link),
                wired_probe=vars(wired),
                wired_failures=self.failures,
                wired_ip=wired_ip,
                wired_gateway=self.gw,
                cellular_ip=cell_ip,
                cellular_healthy=cellular.state == "healthy",
                cellular_probe=vars(cellular),
                cellular_present=cell_present,
                cellular_dial_attempts=self.dial_attempts,
                cellular_connection_state=(
                    "absent"
                    if not cell_present
                    else (
                        "connected"
                        if cell_ip
                        else "waiting-for-modem" if modem_state is None else modem_state
                    )
                ),
                vpns=self.vpn_health,
                vpn_status=self.vpn_status,
                vpn_recovery_policy=POLICY,
                last_switch=self.last_switch,
                updated=time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                wired_recovery_seconds=(
                    round(now - self.good_since, 1)
                    if self.good_since is not None
                    else 0
                ),
            ),
        )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--run", action="store_true")
    parser.add_argument("--config", default="/etc/ec20-failover.json")
    parser.add_argument("--status", default="/run/ec20-failover/status.json")
    parser.add_argument("--ledger", default="/var/lib/ec20-failover/vpn-recovery.json")
    args = parser.parse_args()
    config = load_config(args.config)
    if args.check:
        print("Configuration valid; no host operations performed")
        return
    if sys.platform != "linux" or os.geteuid() != 0:
        parser.error("--run requires root on Linux")
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s"
    )
    Path(args.status).parent.mkdir(parents=True, exist_ok=True)
    Path(args.ledger).parent.mkdir(parents=True, exist_ok=True)
    # Exclusive across controller processes, including while an old one exits.
    with Path(args.ledger).with_suffix(".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        controller = Controller(config, args.status, args.ledger)
        while True:
            started = time.monotonic()
            try:
                controller.tick()
            except Exception:
                logging.exception("Iteration failed; no forced VPN recovery")
                controller.supervisor.failures.clear()
                controller.supervisor.retry_since.clear()
                controller.good_since, controller.failures = None, 0
            time.sleep(
                max(0.2, config["interval_seconds"] - (time.monotonic() - started))
            )


if __name__ == "__main__":
    main()
