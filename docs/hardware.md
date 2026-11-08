# GNSS and optional modem recovery

## GNSS

GNSS control is opt-in, through a root broker on a peer-checked Unix socket. The console account is not given general Device.Control. Enabling GNSS uses ModemManager Location.Setup only for the reviewed EC20F firmware/MM profile and preserves existing location sources. It does not reset the module or change USB/RF/network configuration.

The page distinguishes engine state, an actual fix, missing satellite reports, cache update times and unavailable diagnostics. No coordinates means no confirmed fix; it does not prove an antenna fault. Antenna connection/health remains **unknown** unless a supported diagnostic can establish it. Check the correct GNSS antenna connector, cable and clear sky view locally. Do not confuse a cellular antenna with the GNSS antenna.

## Automatic recovery

The optional root service addresses the narrow fault **verified USB device present, but MM no longer recognizes it**. It does not replace a cellular dialer or failover controller. Absent hardware does not trigger repeated resets.

It requires fresh wired-primary/VPN observations and independent bound probes, the configured identity/topology, no competing modem, no PPP still running and no active SMS/maintenance ownership. Actions escalate from scan to MM rediscovery, then a guarded fixed firmware-checked EC20 reset if needed. A reset temporarily interrupts cellular standby.

Defaults cannot be shortened by configuration: at least 90 seconds sustained fault; recovery cycles at least 15 minutes apart, at most 3/hour and 6/day; resets at least 6 hours apart, at most 2/day. Failed recovery cools down and preserves its persistent ledger. Success requires fresh MM/PPP observations and bound cellular HTTPS verification, not merely an AT OK.

Maintenance coordination uses owned runtime state. The voice initializer has a separate bounded inhibit holder; recovery skips that ownership window. Independent restoration and ExecStopPost release only a mask/inhibit owned by that helper. Never clear the ledger or remove another task's maintenance marker to force a retry.

Defaults ship `enabled:false` and a blank `verify_url`. Before deliberately enabling, configure a credential-free HTTPS `verify_url` on an approved remote endpoint that returns 200; it will be probed through the selected cellular interface. Then run `ec20-modem-recovery.py --check`, verify network-controller autoconnect, backup/restore and independent SSH. Keep the required tools and `host_profile.py` installed together. Unknown firmware/configuration fails closed. Real hotplug, data failover and incoming audio acceptance require a staffed hardware test; CI simulations do not establish these properties on a new board.
