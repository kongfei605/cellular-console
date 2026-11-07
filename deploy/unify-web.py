#!/usr/bin/python3
"""Upgrade only the EC20 web services, retaining an independent timed rollback."""
import datetime,hashlib,http.cookiejar,json,os,secrets,shutil,ssl,subprocess,sys,time,urllib.request
from pathlib import Path
STAGE=Path(__file__).resolve().parent
BACKUP=Path('/var/lib/ec20-console/backups/https-unified-20261107')
ORIGIN='https://192.0.2.10:8788'
SERVICES=['ec20-console','ec20-audio-loopback','ec20-failover','ModemManager','NetworkManager']
PROTECTED=['/usr/local/sbin/ec20-failover','/etc/ec20-failover.json','/etc/NetworkManager/system-connections/ec20-cellular.nmconnection','/etc/polkit-1/rules.d/49-ec20-console.rules','/etc/ec20-console/credentials.json','/etc/ec20-audio-loopback/server.crt','/etc/ec20-audio-loopback/server.key']
def run(*args):return subprocess.check_output(args,text=True).strip()
def sha(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def link(path,target):
 pending=Path(str(path)+'.pending')
 if pending.is_symlink():pending.unlink()
 pending.symlink_to(target);pending.replace(path)
def rollback():
 before=json.loads((BACKUP/'before.json').read_text())
 subprocess.run(['systemctl','stop','ec20-console','ec20-audio-loopback'],check=True)
 for service in ['ec20-console','ec20-audio-loopback']:
  shutil.copyfile(BACKUP/(service+'.service'),Path('/etc/systemd/system')/(service+'.service'));os.chmod(Path('/etc/systemd/system')/(service+'.service'),0o644)
  link(Path('/opt')/service/'current',before['releases'][service])
 subprocess.run(['systemctl','daemon-reload'],check=True)
 subprocess.run(['systemctl','start','ec20-audio-loopback','ec20-console'],check=True)
 print('Rolled back both web services; original data, TLS trust, SSH and network services retained')
def opener(base):
 jar=http.cookiejar.CookieJar();handlers=[urllib.request.HTTPCookieProcessor(jar)]
 if base.startswith('https:'):handlers.append(urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile='/etc/ec20-audio-loopback/server.crt')))
 return urllib.request.build_opener(*handlers)
def request(client,base,path,body=None,csrf=''):
 headers={};data=None
 if body is not None:headers={'Content-Type':'application/json','Origin':base,'X-CSRF-Token':csrf};data=json.dumps(body).encode()
 with client.open(urllib.request.Request(base+path,data=data,headers=headers),timeout=10) as r:return json.load(r)
def preflight():
 f=json.loads(Path('/run/ec20-failover/status.json').read_text());stamp=datetime.datetime.strptime(f['updated'],'%Y-%m-%dT%H:%M:%S%z');assert abs(time.time()-stamp.timestamp())<35,'stale failover status'
 assert f['active']=='wired' and f['wired_healthy'] and f['cellular_present'] and f['cellular_healthy'] and all(f['vpns'].values()),'healthy wired/cellular/VPN required'
 creds=json.loads(Path('/etc/ec20-console/credentials.json').read_text())
 base='http://192.0.2.10:8787';c=opener(base);main_session=request(c,base,'/api/login',creds);status=request(c,base,'/api/status');request(c,base,'/api/logout',{},main_session['csrf']);assert status['sms_send_allowed'] and not [x for x in status['calls'] if x['state_code']!=7],'active call/SMS; defer upgrade'
 # A temporary audio login never stops another owner's existing media.
 c=opener(ORIGIN);session=request(c,ORIGIN,'/api/login',creds);audio=request(c,ORIGIN,'/api/loopback');assert not audio['active'],'audio test active; defer upgrade';request(c,ORIGIN,'/api/logout',{},session['csrf'])
 assert not run('ss','-lntH','sport = :8790'),'private port 8790 already in use'
 return f
if os.geteuid()!=0:raise SystemExit('run with sudo on host 69')
if '--rollback' in sys.argv:rollback();raise SystemExit(0)
if '--check-only' in sys.argv:preflight();print('Web upgrade preflight passed; no service changes');raise SystemExit(0)
preflight()
for name in ['ec20-console-linux-amd64','ec20-audio-loopback-linux-amd64','ec20-console.service','ec20-audio-loopback.service']:assert (STAGE/name).is_file(),name
BACKUP.mkdir(mode=0o700,parents=True,exist_ok=False)
before={'releases':{s:str((Path('/opt')/s/'current').resolve()) for s in SERVICES[:2]},'pids':{s:run('systemctl','show','-p','MainPID','--value',s) for s in SERVICES},'hashes':{p:sha(p) for p in PROTECTED},'routes':run('ip','-4','route','show','default'),'usb_devnum':Path('/sys/bus/usb/devices/1-2/devnum').read_text().strip(),'sms_paths':json.loads(run('mmcli','-m','0','--messaging-list-sms','-J'))['modem.messaging.sms'],'data_hashes':{}}
for name in ['inbox.json','outbox.json','workspace.json']:
 p=Path('/var/lib/ec20-console')/name;before['data_hashes'][name]=sha(p) if p.exists() else None
 if p.exists():shutil.copy2(p,BACKUP/name);os.chmod(BACKUP/name,0o600)
for service in SERVICES[:2]:shutil.copy2(Path('/etc/systemd/system')/(service+'.service'),BACKUP/(service+'.service'))
(BACKUP/'before.json').write_text(json.dumps(before,indent=2));os.chmod(BACKUP/'before.json',0o600)
shutil.copy2(__file__,BACKUP/'rollback.py');os.chmod(BACKUP/'rollback.py',0o700)
subprocess.run(['systemd-run','--unit=ec20-web-unify-rollback','--on-active=10m','--timer-property=AccuracySec=1s','/usr/bin/python3',str(BACKUP/'rollback.py'),'--rollback'],check=True)
key=Path('/etc/ec20-console/audio-gateway.key')
if not key.exists():
 fd=os.open(key,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as f:f.write(secrets.token_hex(32))
os.chown(key,0,0);os.chmod(key,0o600)
releases={}
for service in SERVICES[:2]:
 binary=STAGE/(service+'-linux-amd64');target=Path('/opt')/service/'releases'/sha(binary)[:16];target.mkdir(parents=True,exist_ok=True);shutil.copyfile(binary,target/service);os.chmod(target/service,0o755);releases[service]=str(target)
subprocess.run(['systemctl','stop','ec20-audio-loopback'],check=True)
for service in SERVICES[:2]:
 unit=Path('/etc/systemd/system')/(service+'.service');shutil.copyfile(STAGE/(service+'.service'),unit);os.chmod(unit,0o644)
 link(Path('/opt')/service/'previous',before['releases'][service]);link(Path('/opt')/service/'current',releases[service])
subprocess.run(['systemctl','daemon-reload'],check=True)
subprocess.run(['systemctl','start','ec20-audio-loopback'],check=True)
subprocess.run(['systemctl','restart','ec20-console'],check=True)
print(json.dumps(dict(upgraded=True,releases=releases,rollback_armed=True,canonical_origin=ORIGIN,legacy_http_redirect='192.0.2.10:8787',private_backend='127.0.0.1:8790',media_udp='192.0.2.10:8789')))
