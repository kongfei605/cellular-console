# Contributing

Use Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`), keep changes focused, and run the README checks. Use synthetic data and GitHub's no-reply author email if keeping personal addresses private. Do not commit local profiles, credentials, certificates, SMS/contact/history records, real test numbers or machine paths.

Changes to hardware behavior need tests for negative cases: unknown identity/firmware, stale state, mismatched VPN set, reused USB/card/call identifiers, simultaneous maintenance and failed restoration. Keep demo mode hardware-free. A new profile must document the actual modem firmware, MM behavior, port ownership, data coexistence and staffed recovery results; making the JSON parser accept arbitrary model/version strings is not sufficient.

Do not dial, send SMS, enable GNSS, reboot modules, edit routing or deploy to another person's host as part of ordinary tests. Request explicit authorized hardware acceptance separately. Browser audio tests must distinguish received physical PCM from software echo and actual hearing from transport counters.

When dependencies change, regenerate `THIRD_PARTY_NOTICES.txt` using `python3 scripts/update_notices.py`. Preserve upstream notices and identify new bundled assets. Runtime tool binaries are not vendored into this repository.
