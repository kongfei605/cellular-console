#!/usr/bin/env python3
"""Install only the separate audio service. Never change modem/network services."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

if os.geteuid() != 0:
    raise SystemExit('Run with sudo')
binary = Path(sys.argv[1]).resolve()
unit = Path(sys.argv[2]).resolve()
if '-gateway-key' in unit.read_text():
    raise SystemExit('Integrated gateway unit requires unify-web.py; refusing a partial audio-only migration')
root = Path('/opt/ec20-audio-loopback')
config = Path('/etc/ec20-audio-loopback')
if not Path('/etc/ec20-console/credentials.json').is_file():
    raise SystemExit('Existing administrator credentials not found')
config.mkdir(mode=0o700, exist_ok=True)
os.chmod(config, 0o700)
cert, key = config / 'server.crt', config / 'server.key'
if not cert.exists() and not key.exists():
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-sha256', '-nodes', '-days', '180', '-subj', '/CN=EC20 Audio Loopback 192.0.2.10', '-addext', 'subjectAltName=IP:192.0.2.10', '-addext', 'basicConstraints=critical,CA:FALSE', '-addext', 'keyUsage=critical,digitalSignature', '-addext', 'extendedKeyUsage=serverAuth', '-keyout', str(key), '-out', str(cert)], check=True)
if not key.exists() or not cert.exists():
    raise SystemExit('Incomplete TLS credentials; refusing to overwrite')
os.chmod(key, 0o600)
os.chmod(cert, 0o644)
digest = hashlib.sha256(binary.read_bytes()).hexdigest()
release = root / 'releases' / digest[:16]
release.mkdir(parents=True, exist_ok=True)
subprocess.run(['install', '-m', '0755', str(binary), str(release / 'ec20-audio-loopback')], check=True)
current = root / 'current'
previous = str(current.resolve()) if current.exists() else None
if previous and previous != str(release):
    p = root / 'previous.new'
    p.unlink(missing_ok=True)
    p.symlink_to(previous)
    p.replace(root / 'previous')
link = root / 'current.new'
link.unlink(missing_ok=True)
link.symlink_to(release)
link.replace(current)
subprocess.run(['install', '-m', '0644', str(unit), '/etc/systemd/system/ec20-audio-loopback.service'], check=True)
subprocess.run(['systemctl', 'daemon-reload'], check=True)
subprocess.run(['systemctl', 'enable', '--now', 'ec20-audio-loopback.service'], check=True)
if previous:
    subprocess.run(['systemctl', 'restart', 'ec20-audio-loopback.service'], check=True)
print(json.dumps({'release': str(release), 'sha256': digest, 'https': 'https://192.0.2.10:8788', 'udp': '192.0.2.10:8789', 'certificate': str(cert), 'network_changes': False}))
