#!/usr/bin/python3
"""Host 69 GNSS console upgrade. Only the console is restarted; timed rollback is independent."""
import datetime,hashlib,http.cookiejar,json,os,shlex,shutil,ssl,subprocess,sys,time,urllib.request
from pathlib import Path
STAGE=Path(__file__).resolve().parent
BACKUP=Path('/var/lib/ec20-console/backups/gnss-20261107')
ORIGIN='https://192.0.2.10:8788'
DEST='org.freedesktop.ModemManager1';OBJ='/org/freedesktop/ModemManager1/Modem/0';LOC=DEST+'.Modem.Location'
UNIT=Path('/etc/systemd/system/ec20-console.service');RULE=Path('/etc/polkit-1/rules.d/49-ec20-console.rules')
SERVICES=['ec20-audio-loopback','ec20-failover','ModemManager','NetworkManager']
FILES=['/usr/local/sbin/ec20-failover','/etc/ec20-failover.json','/etc/NetworkManager/system-connections/ec20-cellular.nmconnection','/etc/ec20-console/credentials.json','/etc/ec20-audio-loopback/server.crt','/etc/ec20-audio-loopback/server.key','/etc/ec20-console/audio-gateway.key','/etc/systemd/system/ec20-audio-loopback.service']
def cmd(*args,timeout=15):return subprocess.check_output(args,text=True,timeout=timeout,stderr=subprocess.PIPE).strip()
def sha(p):return hashlib.sha256(Path(p).read_bytes()).hexdigest()
def prop(iface,name):return shlex.split(cmd('busctl','get-property',DEST,OBJ,iface,name))[1]
def owner():return shlex.split(cmd('busctl','call','org.freedesktop.DBus','/org/freedesktop/DBus','org.freedesktop.DBus','GetNameOwner','s',DEST))[1]
def network():
 f=json.loads(Path('/run/ec20-failover/status.json').read_text());stamp=datetime.datetime.strptime(f['updated'],'%Y-%m-%dT%H:%M:%S%z').timestamp()
 assert abs(time.time()-stamp)<30 and f['active']=='wired' and f['wired_healthy'] and f['cellular_present'] and f['cellular_healthy'] and f['cellular_ip'] and len(f['vpns'])==3 and all(f['vpns'].values()),'wired/cellular/VPN guard failed'
 return f
# This baseline deliberately excludes the console that is being updated.
def state():return dict(pids={s:cmd('systemctl','show','-p','MainPID','--value',s) for s in SERVICES},ppp_pid=cmd('pgrep','-x','pppd'),ppp_index=Path('/sys/class/net/ppp0/ifindex').read_text().strip(),ppp_address=cmd('ip','-4','-o','addr','show','dev','ppp0'),routes=cmd('ip','-4','route','show','default'),usb_devnum=Path('/sys/bus/usb/devices/1-2/devnum').read_text().strip(),hashes={p:sha(p) for p in FILES},audio_release=str(Path('/opt/ec20-audio-loopback/current').resolve()))
def data_hashes():return {n:sha(p) if p.exists() else None for n in ['inbox.json','outbox.json','workspace.json'] for p in [Path('/var/lib/ec20-console')/n]}
def link(path,target):
 pending=Path(str(path)+'.pending')
 if pending.is_symlink():pending.unlink()
 pending.symlink_to(target);pending.replace(path)
def rollback():
 before=json.loads((BACKUP/'before.json').read_text())
 subprocess.run(['systemctl','stop','ec20-console'],check=True)
 problem=None
 try:
  assert owner()==before['owner'] and prop(DEST+'.Modem','DeviceIdentifier')==before['device_id'],'MM identity changed; GNSS restoration not attempted'
  assert int(prop(LOC,'Enabled')) in [before['enabled'],before['enabled']|6] and prop(LOC,'SignalsLocation')=='false','external location settings changed; GNSS restoration not attempted'
  cmd('busctl','--timeout=25','call',before['owner'],OBJ,LOC,'Setup','ub',str(before['enabled']),'true' if before['signals'] else 'false',timeout=28)
 except Exception as e:problem=str(e)
 for target,name in [(UNIT,'ec20-console.service'),(RULE,'49-ec20-console.rules')]:shutil.copyfile(BACKUP/name,target);os.chmod(target,0o644)
 link(Path('/opt/ec20-console/current'),before['release'])
 subprocess.run(['systemctl','daemon-reload'],check=True);subprocess.run(['systemctl','start','ec20-console'],check=True)
 if problem:raise SystemExit('Web console restored; '+problem)
 print('Restored console, Location policy and original GNSS sources; network/audio services untouched')
def client():return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile='/etc/ec20-audio-loopback/server.crt')))
def request(c,path,body=None,csrf=''):
 h={};data=None
 if body is not None:h={'Origin':ORIGIN,'Content-Type':'application/json','X-CSRF-Token':csrf};data=json.dumps(body).encode()
 with c.open(urllib.request.Request(ORIGIN+path,data=data,headers=h),timeout=35) as r:return json.load(r)
def preflight():
 network();assert prop(DEST+'.Modem','DeviceIdentifier')=='0000000000000000000000000000000000000000' and prop(DEST+'.Modem','Revision')=='EC20CEFAGR08A03M4G'
 assert int(prop(LOC,'Enabled'))==1 and prop(LOC,'SignalsLocation')=='false','original GNSS state differs'
 c=client();session=request(c,'/api/login',json.loads(Path('/etc/ec20-console/credentials.json').read_text()))
 try:
  s=request(c,'/api/status');assert s['version']=='1.4.0' and s['sms_send_allowed'] and not [x for x in s['calls'] if x['state_code']!=7],'call active or console not ready'
  assert not request(c,'/api/audio/status')['active'],'audio session active'
  assert not [x for x in (request(c,'/api/outbox') or []) if x['status'] in ['pending','creating','ready','submitting']],'SMS pending'
 finally:request(c,'/api/logout',{},session['csrf'])
 baseline=state();assert all(v!='0' for v in baseline['pids'].values());return baseline
if __name__=='__main__':
 if os.geteuid()!=0:raise SystemExit('sudo required on host 69')
 if '--rollback' in sys.argv:rollback();raise SystemExit(0)
 if '--check-only' in sys.argv:preflight();print('GNSS web upgrade preflight passed; no service changes');raise SystemExit(0)
 baseline=preflight()
 for n in ['ec20-console-linux-amd64','ec20-console.service','49-ec20-console.rules']:assert (STAGE/n).is_file(),n
 BACKUP.mkdir(mode=0o700,parents=True,exist_ok=False)
 before={'release':str(Path('/opt/ec20-console/current').resolve()),'console_pid':cmd('systemctl','show','-p','MainPID','--value','ec20-console'),'owner':owner(),'device_id':prop(DEST+'.Modem','DeviceIdentifier'),'enabled':int(prop(LOC,'Enabled')),'signals':False,'baseline':baseline,'data_hashes':data_hashes(),'sms_paths':json.loads(cmd('mmcli','-m','0','--messaging-list-sms','-J'))['modem.messaging.sms']}
 for source in [UNIT,RULE]:shutil.copy2(source,BACKUP/source.name)
 (BACKUP/'before.json').write_text(json.dumps(before,indent=2));os.chmod(BACKUP/'before.json',0o600)
 shutil.copy2(__file__,BACKUP/'rollback.py');os.chmod(BACKUP/'rollback.py',0o700)
 subprocess.run(['systemd-run','--unit=ec20-gnss-web-rollback','--on-active=15m','--timer-property=AccuracySec=1s','/usr/bin/python3',str(BACKUP/'rollback.py'),'--rollback'],check=True)
 try:
  target=Path('/opt/ec20-console/releases')/sha(STAGE/'ec20-console-linux-amd64')[:16];target.mkdir(parents=True,exist_ok=True)
  shutil.copyfile(STAGE/'ec20-console-linux-amd64',target/'ec20-console');os.chmod(target/'ec20-console',0o755)
  for destination,name in [(UNIT,'ec20-console.service'),(RULE,'49-ec20-console.rules')]:shutil.copyfile(STAGE/name,destination);os.chmod(destination,0o644)
  link(Path('/opt/ec20-console/previous'),before['release']);link(Path('/opt/ec20-console/current'),target)
  subprocess.run(['systemctl','daemon-reload'],check=True);subprocess.run(['systemctl','restart','ec20-console'],check=True)
  network();assert state()==baseline,'protected state changed during upgrade'
 except Exception:
  rollback();subprocess.run(['systemctl','stop','ec20-gnss-web-rollback.timer'],check=True);raise
 print(json.dumps(dict(upgraded=True,release=str(target),rollback_armed=True,only_console_restarted=True)))
