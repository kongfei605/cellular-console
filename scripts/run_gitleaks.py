#!/usr/bin/env python3
"""Run a pinned, checksum-verified upstream Gitleaks without global installation."""
import argparse
import hashlib
import io
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile
import urllib.request

VERSION='8.30.1'
# From this upstream release's gitleaks_8.30.1_checksums.txt.
CHECKSUMS={
    ('Darwin','arm64'):('darwin_arm64','b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5'),
    ('Darwin','x86_64'):('darwin_x64','dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709'),
    ('Linux','x86_64'):('linux_x64','551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb'),
    ('Linux','aarch64'):('linux_arm64','e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080'),
}


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--binary',help='use an explicitly supplied local Gitleaks executable');a=p.parse_args()
    with tempfile.TemporaryDirectory(prefix='ec20-gitleaks-') as directory:
        if a.binary:
            binary=Path(a.binary).resolve()
            version=subprocess.check_output([str(binary),'version'],text=True).strip()
            if version!=VERSION:raise SystemExit('Gitleaks version does not match the pinned release')
        else:
            asset,digest=CHECKSUMS[(platform.system(),platform.machine())]
            url=f'https://github.com/gitleaks/gitleaks/releases/download/v{VERSION}/gitleaks_{VERSION}_{asset}.tar.gz'
            request=urllib.request.Request(url,headers={'User-Agent':'ec20-toolbox-release-check'})
            with urllib.request.urlopen(request,timeout=60) as response:data=response.read(30*1024*1024)
            if hashlib.sha256(data).hexdigest()!=digest:raise SystemExit('Gitleaks checksum mismatch')
            with tarfile.open(fileobj=io.BytesIO(data)) as archive:
                binary=Path(directory)/'gitleaks';binary.write_bytes(archive.extractfile('gitleaks').read());binary.chmod(0o700)
        root=Path(__file__).resolve().parents[1]
        # Redaction prevents findings from printing credential values. No report
        # containing matching values is written into the checkout.
        for mode in ['git','dir']:
            subprocess.run([str(binary),mode,'--no-banner','--redact','.'],cwd=root,check=True)
