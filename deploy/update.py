#!/usr/bin/python3
import hashlib
from pathlib import Path
import shutil
import subprocess
stage=Path(__file__).resolve().parent
binary=stage/'ec20-console-linux-amd64'
release=hashlib.sha256(binary.read_bytes()).hexdigest()[:16]
base=Path('/opt/ec20-console');target=base/'releases'/release
target.mkdir(parents=True,exist_ok=True)
shutil.copy2(binary,target/'ec20-console');(target/'ec20-console').chmod(0o755)
previous=base/'previous'
if previous.is_symlink():previous.unlink()
previous.symlink_to((base/'current').resolve())
pending=base/'current.pending'
if pending.is_symlink():pending.unlink()
pending.symlink_to(target);pending.replace(base/'current')
subprocess.run(['systemctl','restart','ec20-console'],check=True)
print('Updated EC20 Console release '+release+'; network services untouched')
