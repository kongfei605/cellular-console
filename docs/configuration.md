# Configuration and safety boundaries

## Local host profile

`configs/host.example.json` describes the **shape**, not a usable device configuration. Keep the edited file in `.local/host.json`, which Git ignores. Install it as `/etc/ec20-toolbox/host.json`, owned by root, mode 0644, in a root-owned directory. Runtime readers reject symlinks, non-regular files, oversized/unknown fields, writable-by-group/others files and placeholder identities.

| Field | Meaning |
| --- | --- |
| `schema_version` | Must be integer 1 |
| `profile` | Must be `ec20f-r08a03-ppp`; cannot whitelist arbitrary firmware |
| `device_id` | Local ModemManager `device-identifier`, not a telephone number or IMEI |
| `usb_path` | Physical USB topology, e.g. `1-2` or `2-3.4`; not a sound-card number |
| `wired_interface` | Existing wired interface; no interface is created |
| `cell_interface` | Existing PPP interface for this reviewed profile |
| `vpn_interfaces` | Exact expected VPN set; one to eight distinct interfaces |

Find identifiers with read-only `mmcli -L`, `mmcli -m <path> -J`, `lsusb -t`, and `ip link`. Check the USB topology against the same selected modem. Do not publish local identifiers or configuration. USB VID/PID remains pinned to `2c7c:0125`. Firmware, modem model and GNSS/MM compatibility checks remain in code; editing JSON cannot relax those checks.

The console, GNSS broker, audio backend and UAC checker accept `-host-config`; the console/audio backend also accept `-failover-state`. The packaged Python helpers use the fixed root-managed `/etc/ec20-toolbox/host.json` and standard failover paths below. If changing helper paths, review every corresponding service and producer together. Demo mode bypasses the hardware profile and never constructs a hardware backend.

## Independent network controller

Before physical-modem use, provide an independently managed `ec20-failover.service` and real health observations. This repository does not supply that controller or a cellular connection profile. Do not write a fake status file to enable calls.

The existing controller must atomically publish `/run/ec20-failover/status.json` with:

- `active`: `wired` or `cellular`.
- `wired_healthy`, `wired_link`, `wired_ip`, `wired_gateway`.
- `cellular_present`, `cellular_healthy`, `cellular_ip`, `cellular_connection_state`.
- `vpns`: a map of every configured interface to a real health boolean.
- `updated`: fresh time in `2006-01-02T15:04:05-0700` format, e.g. a numeric timezone without a colon. Update at least every five seconds.
- Optional UI fields: `last_switch`, `wired_failures`, `wired_recovery_seconds`, `cellular_dial_attempts`.

Its `/etc/ec20-failover.json` must have the fields shown in `configs/failover-config.example.json`. The documentation IPs in that example are **not live probe endpoints**; replace them with approved reachable targets/peers. Interface names and VPN sets must match the host profile. Automatic recovery additionally requires NetworkManager and a working cellular autoconnect policy owned by the network controller.

The console treats stale or absent observations as unavailable. Hardware guards additionally compare current PPP addresses, USB generation, pinned call/MM owner and the exact configured VPN set. Choosing fewer VPNs is an explicit deployment change: configure the actual intended set, never omit failed required tunnels.

## Secrets and data

- Administrator JSON: `/etc/ec20-console/credentials.json`, root:ec20-console 0640.
- TLS leaf certificate and key: `/etc/ec20-audio-loopback/server.crt` and `.key`, root-only directory; systemd supplies service copies with `LoadCredential`.
- Internal audio key: `/etc/ec20-console/audio-gateway.key`, at least 32 random characters, root-only; supplied with `LoadCredential`.
- Application JSON: `/var/lib/ec20-console`, private SMS, contacts, forwarding settings and call history. Back up privately.

No credentials, certificates, recordings, private runtime data or real destination numbers are shipped. Forwarding headers can contain secrets and must be treated like credentials. `.gitignore` reduces accidental staging; CI scanning is an additional check, not permission to commit private files.
