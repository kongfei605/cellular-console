# Linux deployment, updates and rollback

This is a manually reviewed systemd deployment for a trusted private network. It is not a network bootstrapper. Keep an independently verified management connection and wired/cellular controller operational throughout. These instructions do not stop or restart ModemManager, NetworkManager, PPP, VPNs or the network controller.

## 1. Build and render

Prerequisites: Linux with systemd `LoadCredential` support, Go 1.26+, Python 3.11+, ModemManager, polkit, and the independent health producer described in [configuration](configuration.md). Real call audio additionally needs the system `alsa-utils` package (`arecord`, `aplay`). Optional maintenance helpers use `mmcli`, `ip`, `ping`, `curl`, `fuser` and `systemd-run`.

```sh
make build build-audio build-uac-ready build-gnss
mkdir -p .local
cp configs/host.example.json .local/host.json
# Edit .local/host.json with read-only observations from your own host.
python3 deploy/render.py --profile .local/host.json \
  --public-host 127.0.0.1 --output .local/rendered
```

For a remote private/VPN IP, replace `127.0.0.1` with that **local IPv4 address** and obtain a trusted certificate covering it. The renderer rejects hostnames, wildcard addresses and systemd argument/specifier injection; reviewed units bind only that address. Do not expose this single-admin console directly to the public Internet.

Review `.local/rendered/plan.json` and every generated file. Templates under `deploy/` contain placeholders and are **not directly installable**. Rendering is unprivileged, refuses an existing output directory, and never applies system changes.

## 2. Prepare accounts and private files

Create the `ec20-console` system group/user if absent, with no login shell and no general `dialout`/`audio` membership. Create `/etc/ec20-toolbox` (root, 0755), `/etc/ec20-console` (root:ec20-console, 0750), and `/etc/ec20-audio-loopback` (root, 0700). Install the host profile root:root 0644. Do not copy local developer filesystem paths into services.

For a **new** installation only, create a random credential JSON with `scripts/init_credentials.py` into an ignored local directory, then install it root:ec20-console 0640. Never overwrite an existing account during an update. Install the trusted certificate/key and an independently generated audio gateway key as root-only files. The services use credential copies; do not make the TLS key readable to the console account.

Review the coarse ModemManager polkit grants: Voice, Messaging and Location are broader than individual application actions. Only the isolated console account receives these grants. Device.Control, firmware and general network control are not granted to it.

## 3. Install application files

Install the built console/audio binaries into distinct `/opt/ec20-console/releases/<binary-sha-prefix>/` and `/opt/ec20-audio-loopback/releases/<binary-sha-prefix>/` directories. Each directory contains its correspondingly named executable. Create each `/opt/<service>/current` symlink using an atomic rename. Preserve prior links and binaries for rollback.

Install the **reviewed generated** files from `.local/rendered/etc/` into the matching `/etc/` paths with root ownership, and helpers into the matching `/usr/local/libexec/` paths. This is an explicit administrator copy step, not an instruction to recursively overwrite all of `/etc`. Unit/rule/helper source files are 0644/0755 as generated; the host profile must pass the runtime ownership checks.

Before starting anything:

```sh
sudo systemd-analyze verify /etc/systemd/system/ec20-console.service \
  /etc/systemd/system/ec20-audio-loopback.service
sudo systemctl daemon-reload
sudo systemctl enable --now ec20-audio-loopback.service ec20-console.service
```

Then verify HTTPS login, the private audio gateway, fresh network state, SMS archive preservation and independent SSH reachability. These commands start only toolbox services. If state is missing/stale, leave protected controls locked and fix the independent producer.

## 4. Optional GNSS, UAC and recovery

Render into a new directory with `--enable-gnss`, `--enable-call-audio`, and/or `--include-recovery` only after the base console is validated.

- **GNSS:** install the broker binary in `/opt/ec20-gnss-broker/releases/<hash>/ec20-gnss-broker` and its `current` link; install the generated socket/service; enable `ec20-gnss-broker.socket`. The renderer adds the console GNSS flag. Startup does not enable GNSS automatically. Only the reviewed GNSS/MM profile is accepted. See [hardware](hardware.md).
- **Real audio:** UAC must already be enabled by an approved, firmware-specific procedure. Create the `ec20-call-audio` no-login system user/group; install the generated ALSA rule, audio drop-in, voice helper, shared `host_profile.py`, timer/service, marker, and `/usr/local/libexec/ec20-uac-ready`. Reload udev rules and trigger **only sound children of the configured USB parent**, never the whole USB bus. Restart the toolbox audio/console services and enable `ec20-voice-route.timer`. This timer may temporarily inhibit/reconnect the selected modem once per generation, only under healthy wired/VPN/cellular conditions with no call/SMS; coordinate this maintenance window. It never writes USB/RF configuration. See [audio](audio.md).
- **Recovery:** `--include-recovery` stages files with `enabled:false`; it never enables recovery. First run the root helper with `--check`, verify the existing controller's autoconnect and network contract, preserve the ledger, review budgets and arrange maintenance/rollback. Only then deliberately set `enabled:true` in root-owned `/etc/ec20-modem-recovery.json` and enable its service. Recovery can restart ModemManager or EC20. See [hardware](hardware.md).

Restarting **only** the console/audio/broker does not apply USB composition or reset a modem. Any action that can interrupt the cellular fallback requires an already verified wired management path.

## Updates and rollback

1. Back up current release links, units/drop-ins/rules, private configuration and application data to a root-only location outside the checkout. Preserve the modem recovery ledger. Confirm no call, audio session or SMS submission is active.
2. Render/review a new deployment and prepare binaries in new release directories. This repository's open-source configuration format is a migration from earlier host-specific deployments; do not replace an existing binary until its host profile and service arguments are prepared together.
3. Arrange an **independent local systemd rollback timer** for the exact reviewed software changes before switching links. Keep a concrete root-owned restoration script in the private backup. It should restore only toolbox links/files and restart those application services, never reset the network stack or remove modem devices. Do not start a generic rollback command copied from another host.
4. Validate login, calls/media cleanup, archive counts, bound cellular Internet access, VPNs and a persistent independent SSH heartbeat. Check both MM AT ports remain available. Cancel the deployment timer only after verification; keep the backup.
5. On failure restore previous toolbox files/links and verify data/SSH. If a hardware USB configuration rollback is separately required, re-check wired/VPN health **before** stopping MM or resetting hardware. When cellular is the sole management path, defer hardware rollback and preserve it.

The old one-host migration scripts and incident records are not shipped as public installers. They are retained in the original operator's private backup. No public command should assume another machine's device identity, network profile or rollback directory.
