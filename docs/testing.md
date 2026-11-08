# Tests and release checks

Run the commands in README before committing. Tests use local synthetic SMS/calls, generated integer audio and temporary credentials. They never dial real numbers or require a modem.

- Go: HTTP login/origin/CSRF, SMS idempotency and errors, forwarding validation, persistent archives, call ownership, PCM/G.711 conversion, distinct PCM directions through real local ICE/DTLS-SRTP, cleanup and live guard failure.
- Python: hardware-profile validation, render-only deployment defaults, owned maintenance restoration, recovery escalation/circuit-breaker/rate limits and USB/AT ownership checks using fakes/PTYs.
- Release privacy: tracked files and reachable Git blobs/commit metadata; Gitleaks adds common credential rules. Only rule names/paths are printed by the privacy checker. Runtime files are ignored and must remain untracked.
- License inventory: `scripts/update_notices.py --check` reads actual linked package modules for Linux and Darwin and verifies their pinned license texts.
- CI: Linux/macOS native tests plus Linux/macOS amd64/arm64 cross-builds. Physical RF, antenna, USB reboots, real call hearing and network failover remain manual tests.

For hardware acceptance, obtain an explicitly authorized receiver, preserve an independent SSH heartbeat, verify wired/VPN/cellular health before and after, test outgoing and incoming calls, both audio directions, hangup/media cleanup, and any hotplug/reset behavior in a staffed maintenance window. Never substitute software echo for real audio or infer a GNSS fix from a running engine.

This code previously passed two-way outgoing call listening on its reviewed hardware/firmware profile. The open-source configuration migration has automated/local validation; it has not been deployed over the original host. New physical installations still need their own acceptance.

## Publishing boundary

The open-source preparation does not create a GitHub repository or publish anything. Local historical identifiers were sanitized after a verified external private backup. The original operational backup must never be uploaded. Before publishing, inspect every branch/tag and the exact source/binary archive; run privacy and Gitleaks against the final reachable history. Never publish `.git` administration/backup files as a source archive.
