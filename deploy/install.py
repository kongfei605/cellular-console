#!/usr/bin/python3
"""Install one isolated app. Never restart or modify existing network services."""
import hashlib
import json
import os
from pathlib import Path
import pwd
import secrets
import shutil
import subprocess

stage=Path(__file__).resolve().parent
binary=stage/'ec20-console-linux-amd64'
release=hashlib.sha256(binary.read_bytes()).hexdigest()[:16]
def run(args):
    r=subprocess.run(args,check=True,capture_output=True,text=True,timeout=25)
    return r.stdout
try:account=pwd.getpwnam('ec20-console')
except KeyError:
    run(['useradd','--system','--user-group','--home-dir','/var/lib/ec20-console','--shell','/usr/sbin/nologin','ec20-console'])
    account=pwd.getpwnam('ec20-console')
config=Path('/etc/ec20-console');config.mkdir(mode=0o750,exist_ok=True)
os.chown(config,0,account.pw_gid);config.chmod(0o750)
credentials=config/'credentials.json'
if not credentials.exists():
    fd=os.open(credentials,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o640)
    with os.fdopen(fd,'w') as f:json.dump(dict(username='admin',password=secrets.token_urlsafe(24)),f)
os.chown(credentials,0,account.pw_gid);credentials.chmod(0o640)
base=Path('/opt/ec20-console');target=base/'releases'/release
target.mkdir(parents=True,exist_ok=True)
shutil.copy2(binary,target/'ec20-console');(target/'ec20-console').chmod(0o755)
current=base/'current';previous=base/'previous'
if current.is_symlink():
    if previous.is_symlink():previous.unlink()
    previous.symlink_to(current.resolve())
pending=base/'current.pending'
if pending.is_symlink():pending.unlink()
pending.symlink_to(target);pending.replace(current)
shutil.copy2(stage/'ec20-console.service','/etc/systemd/system/ec20-console.service')
shutil.copy2(stage/'49-ec20-console.rules','/etc/polkit-1/rules.d/49-ec20-console.rules')
Path('/etc/systemd/system/ec20-console.service').chmod(0o644)
Path('/etc/polkit-1/rules.d/49-ec20-console.rules').chmod(0o644)
run(['systemctl','daemon-reload'])
run(['systemd-analyze','verify','/etc/systemd/system/ec20-console.service'])
# Timed rollback only stops this app; all network and modem services stay intact.
run(['systemd-run','--unit=ec20-console-deploy-rollback','--on-active=15m',
     '--timer-property=AccuracySec=1s','/usr/bin/systemctl','disable','--now','ec20-console.service'])
run(['systemctl','enable','--now','ec20-console.service'])
print(json.dumps(dict(installed=True,release=release,url='http://192.0.2.10:8787',
                     credentials_file=str(credentials),username='admin',rollback_armed=True)))
