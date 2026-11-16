# VPN identity, observations and bounded recovery

The independent root controller lives under `controller/`. It is never installed
or started by the console, renderer or build. Adoption requires an explicitly
authorized deployment, independent management connection, backup and rollback.
Updating the console alone does not change the network controller.

It assumes existing NetworkManager cellular profiles and PPP table 2020 policy
routing. It is not a network bootstrapper. Dependencies: Python 3.11+,
`python3-dbus`, OpenVPN 3 Linux's Python package, `ip`, iputils `ping`, `nmcli`,
`busctl` and GNU `timeout`.

## Identity and observations

Each VPN has a stable `id`, exact OpenVPN `profile` name, approved IPv4 server
`remote` and VPN-internal `peer`. Optional `config_path` pins an imported
configuration object. `interface` is a legacy hardware-guard slot, never a
runtime discovery hint. `auto_recover` defaults to true and must be a boolean.
Setting it false disables recovery for that VPN without removing it from the
required VPN set or relaxing hardware guards.

The reader checks typed session metadata, exact profile, uniqueness, server,
unique manager owner and actual interface. The peer route must use the same
interface, then a bound ping tests actual traffic. OpenVPN v23 omits
`connected_to`; its exact backend PID, TUN descriptor and connected server socket
provide independent server confirmation. Idle-exiting metadata managers can be
D-Bus activated, with a bounded readiness check. Manager activation creates no
VPN session. A failed manager query never means an empty session inventory.

An orphaned backend can be displayed as healthy only when a unique executable
owns both the peer-routed TUN and the approved server socket, and a bound peer
probe succeeds. It remains read-only: no guessed ownership, process killing or
shared manager restart is used to recover an orphan.

The producer's `vpn_status` map is keyed by stable ID. It reports `state`
(`healthy`, `unhealthy`, `unknown`), `reason`, actual `interface`, optional
`latency_ms`, probe `observed_at`, `consecutive_failures`, `recovery` and optional
`backend_fd_count`/`backend_fd_limit`. The console expires observations after 30
seconds; republishing the status file does not refresh an old probe.

- A reply is healthy even if a cached session status lags behind reality.
- iputils exit 1 without stderr is a valid no-reply observation.
- Resource/permission errors, missing commands, subprocess timeouts and failed
  route/interface/session queries are unknown and never create a failure streak.
- One successful uplink target proves reachability. No successes plus any
  detector error is unknown. Unknown breaks wired failure/recovery continuity.
- A legacy false boolean is unknown in the UI; it cannot explain its detector.

## Public egress and failover routing

The wired gateway comes from the live NM gateway, DHCP `routers` lease option,
explicit static profile, or a cache pinned to the unchanged wired address. A
missing live default never erases a valid DHCP gateway; an address change cannot
reuse a previous network's cached gateway.

The controller preserves the NM/DHCP wired default. A verified active PPP uplink
uses an owned metric-50 default; wired-primary mode uses PPP metric 700 as standby.
Only obsolete PPP defaults tagged with the controller's protocol 196 are removed.
The default on an unrelated USB network interface is never deleted. PPP loss
therefore leaves the working wired default available, and a healthy wired link
can return immediately after definite PPP failure. Unknown or unhealthy cellular
observations cannot initiate a switch to PPP. Gateway lookup failures do not
mutate routes. These rules avoid both a missing generic default and an unrelated
USB default taking priority over the selected PPP path.

## Confirmed absence and creation

Policy `identity-bounded-v2` adds a distinct confirmed-missing observation. It
requires ALL of the following, repeated in three consecutive fresh samples:

1. A successful complete session inventory contains no matching profile/path.
2. Process and socket inventory contains no unregistered backend for that server,
   including a backend that has not yet created a TUN. Unmapped startup processes
   with no identifiable server block creation.
3. The peer route uses a configured uplink rather than an existing TUN or another
   tunnel, and the systemd profile wrapper is not
   active, starting or stopping.
4. Exactly one imported configuration is available, or an explicitly configured
   `/etc/openvpn3/autoload/*.ovpn` file and its parent directories are root-owned,
   nonwritable by group/others, regular and not symlinks. The file content hash or
   imported manager owner/path pins the target across revalidation.
5. The active uplink is healthy, maintenance is off, and recovery is enabled.

Immediately before action, all evidence is refreshed and compared. A newly
appeared backend/session, changed configuration, error or cycle longer than 25
seconds cancels creation. Startup uses the exact imported `--config-path` or
verified autoload file, with a 30-second connection timeout and 35-second local
command deadline. There is at most one in-flight recovery command globally;
other VPNs are inventoried again before their turn. A normal session that is still
connecting does not starve other independently confirmed missing profiles.

## Long reconnects and file-descriptor protection

A healthy peer is never disconnected for resource cleanup. A failing managed
session can be retired only with an exact session owner/path/backend PID,
verified executable, process generation and approved server socket. It needs
three valid fault samples and one of these additional conditions:

- FD usage reaches 75% of that process's soft open-file limit; or
- a confirmed reconnect continues for 120 seconds with a healthy uplink; or
- reconnect continues for 60 seconds while the uplink is definitively unhealthy.

An unknown uplink, failed resource query, mismatched route, ambiguous identity,
paused/authentication state or successful peer probe does not authorize retirement.
A missing interface is usable evidence only after a successful independent link
inventory. Retirement invokes `Disconnect` on that exact existing session and
unique manager owner. It never restarts ModemManager, NetworkManager, PPP, another
VPN or a shared OpenVPN manager. No PID is killed by this recovery path.

If the uplink is down, retirement stops the backend's internal retry loop and
waits. New creation requires healthy upstream plus fresh confirmed absence,
including proof that the old backend has exited. Stop+rebuild uses one persisted
reservation. Its start claim is saved before launch and cannot be replayed after
controller restart. Even if startup budgets are exhausted, a proven failing
backend can be contained at most once per minute; containment does not grant a
free new start. A healthy managed session with ordinary valid peer failures
retains the bounded, identity-pinned `Restart` path.

## Budgets, maintenance and compatibility

The root-private ledger reserves before commands. A controller-wide file lock
prevents concurrent supervisors. Backoff starts at 60 seconds, doubles and is
capped at one hour; each VPN has at most three new recovery reservations per
rolling hour and six per rolling 24 hours. Limits survive process restart and
brief success. Corruption, failed writes and clock rollback fail closed. Old
v1 ledgers migrate without dropping historical attempts. Recovery commands and
completion status are logged without credentials or configuration contents.

For planned maintenance, create `/run/ec20-failover/vpn-maintenance` **before**
stopping a VPN. Observations continue but all actions pause. Remove the marker
to resume fresh recovery checks. Alternatively set a VPN's `auto_recover:false`
in the private config and reload the controller. Keep the VPN in the required set.

The legacy `vpns` map and pinned call/audio/GNSS hardware guards remain unchanged.
A legacy slot becomes true only if the actual interface still equals that slot.
Thus dynamically identified VPNs may be healthy while an outdated hardware
profile remains locked. This change never silently relaxes those guards.

Local checks do not contact any device:

```sh
python3 controller/ec20_failover.py --check --config configs/failover-controller.example.json
python3 -m unittest discover -s controller -p 'test_*.py'
node scripts/test_vpn_ui.cjs
```

The example contains reserved endpoints and a synthetic cellular UUID. Runtime
requires explicit `--run`, root on Linux, and separately reviewed deployment.
Tests include a real child-process start/exit/recreation cycle, fake D-Bus,
filesystem ownership, orphan/startup races, outage recovery, resource protection,
persistent budgets and detector failures. They do not disconnect physical VPNs
or establish EC20 firmware/power stability. Real outage acceptance requires an
independent management path and a separate authorized maintenance window.

API reference: [OpenVPN session manager](https://github.com/OpenVPN/openvpn3-linux/blob/master/docs/dbus/dbus-service-net.openvpn.v3.sessions.md).
The v23 adapter is also checked against the installed OpenVPN Python package.
