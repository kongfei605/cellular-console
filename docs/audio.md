# Software and real call audio

The unified browser page is `/#audio` on the console HTTPS origin. Its private backend listens on `127.0.0.1:8790`; WebRTC media uses the configured private host IP on UDP 8789. The backend remains a separate process and accepts delegated console sessions through a shared internal key. It does not need the administrator password.

For software loopback, use the rendered base units or run the audio executable with explicit `-listen`, `-media`, `-gateway-key` and the console's matching `-audio-backend`/`-audio-gateway-key` flags. Gateway mode accepts only literal loopback HTTP control. Browser microphone access still needs a trusted HTTPS origin and browser/OS permission. The local console demo alone does not create an audio backend or a trusted certificate.

## Real EC20 calls

1. Verify the reviewed firmware and USB identity/topology. The EC20 must expose exactly one duplex `S16_LE`, 8000 Hz, mono UAC endpoint. Generic/default sound cards are never substituted.
2. First-time USB composition changes are a **separate hardware maintenance operation**. They may reset/re-enumerate the modem and interrupt cellular standby. This release does not include a universal UAC-enable/reboot script: use vendor documentation for the exact firmware, preserve the original full USB layout and prepare local recovery before making any change. Never copy another modem's USB tuple or alter QDAI/APN to make the UI appear ready.
3. Opt into the configured ALSA account, UAC helper and real-audio flags as described in [deployment](deployment.md). The helper sets/reads only `CGMR`, `QPCMV=1,2` and `QPCMV?`, under coordinated MM inhibit. It never reserves the secondary AT port permanently.
4. Wait for the generation-bound root readiness credential. Dial/answer in call control; when the unique call is active, select **接入当前通话音频**. Use headphones.
5. Speak different phrases in each direction and have the other side repeat them. RTP counters prove transport activity, not that both ends hear intelligible audio. Test incoming answer separately.

Audio flow: browser microphone → PCMU WebRTC/DTLS-SRTP → Go bridge → EC20 USB playback → cellular peer; the return direction uses independent USB capture. Audio is not recorded. Software loopback and real call mode are distinct; real mode never substitutes echo.

The audio account receives access only to the configured EC20's ALSA nodes, with a closed device policy; it cannot open AT ports. Only the root helper controls the narrowly permitted voice route. Both MM AT ports stay available for data and online call/SMS management.

Lease and live guards pin the boot/USB generation, unique MM owner and full call path. Hangup, USB loss, another active call, stale state or unhealthy required networks closes PCM and media. Stopping audio alone does not hang up a cellular call. Sessions expire after three minutes. Per-generation route initialization is skipped when wired/VPN/cellular health is unavailable; data access takes priority.

For read-only diagnosis:

```sh
/usr/local/libexec/ec20-uac-ready -usb-path YOUR_USB_TOPOLOGY
/usr/local/libexec/ec20-uac-ready -voice-route
systemctl status ec20-voice-route.timer ec20-modem-recovery
```

The first command never opens sound/AT devices. `-voice-route` additionally reads MM and network guards; success is not a hearing test. Never clear a generation lease and concurrently change QPCMV outside a coordinated maintenance window.
