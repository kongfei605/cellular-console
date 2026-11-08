# Security policy

This is a **single-administrator management console for a trusted private network**. It is not a public multi-tenant modem gateway. Use a trusted HTTPS certificate and restricted network access. Browser microphone permission belongs to the browser/OS; never disable certificate validation to make it work.

Keep the console unprivileged. Only narrow optional root helpers may touch GNSS/voice/recovery controls. Do not add the console/audio accounts to general `dialout`/`audio` groups or expose arbitrary AT/shell/network-control endpoints. The closed device policy, exact host identity, reviewed firmware checks, owned restoration, rate limits and fresh network guards are security/safety boundaries.

SMS, call history, contacts, precise GNSS coordinates, forwarding headers and backups are private data. Local diagnosis exports are filtered, but review any artifact before sharing. Never upload actual credentials or raw operational archives in a public issue.

Report a vulnerability through the repository's GitHub **Report a vulnerability / private security advisory** feature if enabled by its owner. If it is unavailable, first ask the maintainer for a private reporting channel without including exploit details or private records. No private-reporting endpoint is claimed to be active before a repository is published.

The latest reviewed branch is the maintenance target; there is no promised support window for historical host-specific builds. CI and secret scans do not prove that every firmware or remote reboot procedure is safe.
