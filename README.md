# EC20 Toolbox

A Go service and Chinese Web interface for Quectel EC20 modems on Linux. Includes status monitoring, SMS conversations and forwarding, call control, a WebRTC audio bridge, guarded GNSS control and optional modem recovery.

**Project status:** tested on one EC20F / `EC20CEFAGR08A03M4G` with ModemManager 1.20.4 and PPP. This is the supported hardware profile, not a claim that every EC20/EC25 firmware is compatible. Unknown profiles remain blocked for privileged operations.

## Platform support

| Usage | Linux | macOS |
| --- | --- | --- |
| Browser client for an existing Linux server | Yes | Yes |
| Local demonstration, no modem access | Yes | Yes; Apple Silicon verified |
| Software WebRTC audio loopback | Yes | Portable Go implementation; tests run in CI |
| Direct modem SMS/calls/GNSS | Reviewed profile only | No macOS hardware backend yet |
| Real modem call audio | EC20 ALSA UAC endpoints required | No CoreAudio adapter yet |
| Modem recovery / systemd deployment | Optional | Unsupported |

## Quick start: safe local demo

Requirements: Go **1.26 or newer** and Python **3.11 or newer**. No Node.js build or external frontend assets are required.

```sh
git clone https://github.com/kongfei605/ec20-toolbox.git
cd ec20-toolbox
python3 scripts/init_credentials.py --output .local/demo-credentials.json
go run . -demo -listen 127.0.0.1:8787 \
  -credentials .local/demo-credentials.json -data-dir .local/demo-data
```

Open <http://127.0.0.1:8787/>. The username is `admin`; read the randomly generated password from `.local/demo-credentials.json` in your own terminal. The helper refuses to overwrite an existing credentials file. Demo calls and SMS never reach a modem. Stop with Ctrl+C. The demo does not start a separate audio backend automatically; see [audio setup](docs/audio.md).

## Features

- Status: SIM, registration, radio signal, wired/cellular health and VPN state.
- SMS: local conversation archive, compose/reply, Unicode/GSM-7 segment checks, persistent send status and duplicate/rate guards.
- Forwarding: sender and keyword filters; optional webhook and/or SMS destinations. Webhooks support configurable headers. Forwarding is off until configured. See [forwarding](docs/forwarding.md).
- Calls: confirm before dialing; incoming-call display, answer and hangup; persistent observed call history and local contacts.
- Audio: authenticated HTTPS/WebRTC software loopback and optional real EC20 UAC calls. Real audio never falls back to software echo.
- GNSS: guarded start/stop, fix/satellite diagnostics, cache timestamps; antenna state is unknown when the hardware cannot report it.
- Recovery: an optional, disabled-by-default root helper for USB-present/MM-missing faults; wired/VPN checks, rate limits, owned restoration and data-path verification.

The independent wired/cellular failover controller is an **external dependency**, not installed or implemented by this repository. Its status/config contract is documented in [network integration](docs/configuration.md). The console never replaces routing, PPP, VPN or NetworkManager management. Missing/stale health data blocks protected operations.

## Linux installation

Start with [deployment](docs/deployment.md), [configuration](docs/configuration.md), and [security](SECURITY.md). Copy and edit `configs/host.example.json` locally; the placeholder device identity is deliberately invalid.

```sh
make build build-audio build-uac-ready build-gnss
python3 deploy/render.py --profile .local/host.json \
  --public-host 127.0.0.1 --output .local/rendered
```

The renderer **only writes files for review**. It does not run systemctl, install packages, change routes, connect a modem, send messages or restart hardware. Its default output enables neither GNSS control, real call audio nor recovery. Recovery remains disabled even when its files are included.

## Development

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s deploy -p 'test_*.py'
make build-native
python3 scripts/check_release.py --history
python3 scripts/update_notices.py --check
```

CI runs Linux/macOS tests, cross-builds Linux/macOS for amd64/arm64, checks privacy and scans history with Gitleaks. These jobs do not access physical modems, dial numbers or prove real RF/audio/GNSS behavior. Hardware acceptance and release boundaries are in [testing](docs/testing.md). See [contributing](CONTRIBUTING.md) for changes to device profiles.

## License and attribution

First-party code is under the [MIT license](LICENSE). Preserve [third-party notices](THIRD_PARTY_NOTICES.txt) when distributing binaries. System programs such as ALSA utilities and ModemManager are external dependencies, not bundled executables.

The feature organization and UI discussion drew inspiration from [EC25 Toolbox](https://github.com/skyrocketingHong/EC25Toolbox). This project does not include its Swift implementation or promise EC25 compatibility. G.711 test vectors are synthetic integers; no telephone or microphone recordings are included.
