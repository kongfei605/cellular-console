# SMS forwarding

Configure rules in the authenticated **短信转发** page. Forwarding starts disabled.

Rules may match selected sender numbers and/or content keywords, using the rule's displayed match-any/match-all choice. Add a webhook destination, an SMS destination, or both. Webhooks support multiple configured HTTP headers; values can be sensitive and are not limited to a Bearer token. Header names/values and URLs are validated. Private, local and metadata-service targets are blocked by the webhook destination policy; do not bypass that policy to test a receiver.

SMS forwarding uses the selected modem/SIM, incurs normal carrier charges, and follows network/send guards. Forward markers, duplicate/rate limits and persisted job state reduce loops and repeat submission. Carrier submission is not delivery confirmation. Interrupted/unknown sends must be checked before retrying; service startup must not blindly resend them.

Keep forwarding off during installation and upgrades until the intended receivers are reviewed. Do not use a real personal number or production webhook as a demo fixture. Never commit forwarding settings, message bodies, authentication headers, inbox/outbox or contact/history JSON. Backups containing them belong in a private root-only directory outside Git.
