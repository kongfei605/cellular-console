# macOS direct hardware support

## Build and run

Install Xcode Command Line Tools, Go 1.26+ and Python 3.11+. Run:

```sh
sh scripts/run_macos.sh -ec20-voice -gnss-control
```

The launcher builds `bin/ec20-console`, `bin/ec20-audio-loopback` and
`bin/ec20-native`, creates credentials only if absent, and starts the console
at `http://127.0.0.1:8787/`. Credentials and the `data` directory default to
`~/Library/Application Support/Cellular Console/`. Stop with Ctrl+C.
The username is `admin`; read the generated password locally. No default
password, login item or system-wide daemon is installed.

`make build-mac` builds without launching. The Go binaries remain CGO-free.
The native helper links only Apple's IOKit, CoreAudio and AudioToolbox frameworks,
embeds its microphone usage description, and receives an ad hoc local signature.
Keep `ec20-native` beside the console binary, or specify `-mac-helper`.
`go run .` needs an explicit `-mac-helper "$PWD/bin/ec20-native"`.

## Device and network selection

`-backend auto` selects `macos` on macOS and `modemmanager` on Linux.
`-demo` remains hardware-free on either platform. Explicit `-backend modemmanager`
retains the Linux connection/configuration path.

The native helper enumerates USB VID/PID `2c7c:0125` and discovers the ECM BSD
interface from the same USB tree. If there is more than one modem, select an
eight-digit hexadecimal location using `-mac-usb-location`; `bin/ec20-native list`
prints the available locations, USB generations and network interface names.
The backend hashes the queried modem identity and pins it after the first
successful connection. `-device-id` can pin that hash across process restarts.
Moving to another physical USB port requires restarting or updating the selector.

Other AT clients, including EC25Toolbox, must disconnect first. The helper opens
only vendor AT interface 2, discovers its bulk pipes and uses ordinary exclusive
interface ownership. It never seizes the device, detaches ECM/audio drivers,
changes a USB composition, changes an APN, reconnects cellular service or resets
the modem. A timed-out AT session is discarded; a new session cancels an abandoned
SMS prompt and waits for a quiet interval before issuing queries.

The local network observer reads the default route and system byte counters.
Every eight seconds it performs bounded HTTPS HEAD requests through both the
primary interface and the ECM interface, binding each socket to its interface
and IPv4 address. The default endpoint is Apple's small connectivity page;
`-mac-probe-url https://example.com/health` selects your credential-free endpoint
returning 200. The endpoint receives ordinary HTTPS requests from both networks.
`-mac-primary-interface` optionally pins the primary interface. Cellular health
is never inferred from its address or from the primary network's success.

No route, DNS, network service or VPN setting is changed. The Mac mode does not
implement an automatic failover controller or VPN health monitoring. An unknown
default route (including an unhandled VPN route) keeps protected actions blocked.
Fresh primary/cellular probes, an unchanged selected USB generation and an
unchanged default route are required for SMS/dial/GNSS enable. Cellular-primary,
stale observations or interface loss block those actions; hangup remains available.

## SMS, calls and GNSS

Writes are limited to `EC20F / EC20CEFAGR08A03M4G`. USB identity alone does not
unlock control on another firmware. Status reads remain available on other EC20
profiles when their standard AT queries work.

Stored SMS are read in PDU mode with `CMGL=4`, decoded as GSM-7 or UCS-2 and copied
to the existing private archive. This firmware does not accept the optional
CMGL mode argument, so listing can mark module messages as read. The console's
unread state is independent. Module messages are never deleted. Complete
concatenated messages are assembled; incomplete/ambiguous groups, binary SMS,
national-language shift tables and unsupported alphabets are not rendered as text.
This backend polls stored messages; direct-to-TE-only SMS routing is not configured.

Outgoing SMS use PDU mode, GSM-7 extension packing or UCS-2 and at most three
concatenated parts. A durable local handle is saved before submission. No failed,
timed-out, partially submitted or reconnected request is automatically sent again.
`sent` means every part received a modem/network submission reference, not a
recipient delivery receipt. Old ambiguous handles stay unknown after restarting.

Calls are polled with CLCC. The public call identifiers are local monotonic
handles, not reusable modem slot numbers. Dial/answer reject competing calls.
Single-call hangup uses CHUP; a selected call in a multi-call list uses CHLD 1x.
No arbitrary service codes or AT terminal are exposed. Calls too short to appear
between polls are not guaranteed to be recorded.

GNSS status uses QGPS and NMEA queries and reuses the existing fix validation,
satellite diagnostics and cache logic. `-gnss-control` makes the protected
start/stop UI available; starting requires the reviewed firmware and an already
enabled NMEA query source. The console does not silently change GNSS source
configuration or infer antenna health from a missing fix.

## CoreAudio and browser audio

Without `-ec20-voice`, Mac starts the software WebRTC loopback only.
With the flag, dial/answer first checks and, when necessary, prepares the reviewed
QPCMV USB voice route. Active-call media refuses an unprepared route.
No voice routing command is issued just by starting the server or viewing status.

The native audio adapter selects the exact USB device's input interface 7 and
output interface 8 by their CoreAudio UIDs. It requires one unambiguous EC20,
one input channel, one output channel and an existing 8000 Hz hardware rate.
It never falls back to the Mac microphone, speakers or default sound device.
Multiple EC20 devices with generic factory serials make audio selection ambiguous
and are rejected. This reviewed USB composition is required.

AudioQueue moves S16/8000/mono PCM through bounded memory rings and pipes to the
existing WebRTC/G.711 implementation. No microphone or call audio files are
created. Each call stream is guarded by current CLCC state, AT ownership, USB
generation and local network protection; losing any closes the stream. Stopping
audio does not hang up the telephone call. Permit microphone access if macOS
requests it when starting real media. Browser microphone permission is separate;
loopback localhost or trusted HTTPS is required for browser audio.

## Verification and remaining hardware acceptance

Automated coverage includes AT prompt fragmentation, session timeout isolation,
GSM/UCS-2 golden PDUs, concatenation, malformed input, call slot reuse, ambiguous
SMS submission, USB/route changes and native macOS interface counters. Existing
HTTP/auth, forwarding, persistence and WebRTC tests continue to run.

On the connected Apple Silicon Mac, read-only acceptance verified the reviewed
model/firmware, SIM readiness, LTE registration, stored SMS decoding, ECM HTTPS
reachability, primary/ECM byte counters, GNSS engine status and exact CoreAudio
endpoint discovery. Actual outbound SMS, dial/answer/hangup, a GNSS fix and
two-way call audio still require an authorized receiver and physical acceptance.
Software tests and endpoint enumeration do not establish those properties.

The Linux recovery scripts and systemd deployment remain Linux-only. Mac wake,
disconnect and reconnect trigger ordinary discovery and state checks; there is no
automatic modem reset or USB recovery action.
