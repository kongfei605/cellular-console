# Contributing

Use Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`), keep changes focused, and run the README checks. Use synthetic data and GitHub's no-reply author email if keeping personal addresses private. Do not commit local profiles, credentials, certificates, SMS/contact/history records, real test numbers or machine paths.

Release checks treat standard merge-commit author/committer email fields as
public Git attribution. This exception does not remove or hide that attribution;
personal emails elsewhere, credentials and private network addresses remain
subject to the privacy checks.

Keep actual wired/VPN addresses and gateways in private operator notes, never in
public documentation, example configurations or commit messages. Use reserved
documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`) for
examples. Release checks reject private IPv4 addresses in those public contexts.

Changes to hardware behavior need tests for negative cases: unknown identity/firmware, stale state, mismatched VPN set, reused USB/card/call identifiers, simultaneous maintenance and failed restoration. Keep demo mode hardware-free. A new profile must document the actual modem firmware, MM behavior, port ownership, data coexistence and staffed recovery results; making the JSON parser accept arbitrary model/version strings is not sufficient.

Do not dial, send SMS, enable GNSS, reboot modules, edit routing or deploy to another person's host as part of ordinary tests. Request explicit authorized hardware acceptance separately. Browser audio tests must distinguish received physical PCM from software echo and actual hearing from transport counters.

When dependencies change, regenerate `THIRD_PARTY_NOTICES.txt` using `python3 scripts/update_notices.py`. Preserve upstream notices and identify new bundled assets. Runtime tool binaries are not vendored into this repository.
