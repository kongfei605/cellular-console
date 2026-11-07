#!/usr/bin/python3
"""Bounded GNSS-only test. Never stop MM/PPP or open a raw serial device."""
import datetime,hashlib,json,os,re,shlex,subprocess,sys,time
from pathlib import Path
ROOT=Path('/var/lib/ec20-gnss-check/safe-probe-20261107')
DEST='org.freedesktop.ModemManager1';OBJ='/org/freedesktop/ModemManager1/Modem/0';LOC=DEST+'.Modem.Location'
SERVICES=['ec20-console','ec20-audio-loopback','ec20-failover','ModemManager','NetworkManager']
FILES=['/usr/local/sbin/ec20-failover','/etc/ec20-failover.json','/etc/NetworkManager/system-connections/ec20-cellular.nmconnection']
def cmd(*args,timeout=15):return subprocess.check_output(args,text=True,timeout=timeout,stderr=subprocess.PIPE).strip()
def prop(iface,name):return shlex.split(cmd('busctl','get-property',DEST,OBJ,iface,name))[1]
def owner():return shlex.split(cmd('busctl','call','org.freedesktop.DBus','/org/freedesktop/DBus','org.freedesktop.DBus','GetNameOwner','s',DEST))[1]
def setup(mask,signals):cmd('busctl','--timeout=25','call',DEST,OBJ,LOC,'Setup','ub',str(mask),'true' if signals else 'false',timeout=28)
def state():return dict(pids={s:cmd('systemctl','show','-p','MainPID','--value',s) for s in SERVICES},ppp_pid=cmd('pgrep','-x','pppd'),ppp_index=Path('/sys/class/net/ppp0/ifindex').read_text().strip(),ppp_address=cmd('ip','-4','-o','addr','show','dev','ppp0'),routes=cmd('ip','-4','route','show','default'),usb_devnum=Path('/sys/bus/usb/devices/1-2/devnum').read_text().strip(),hashes={p:hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in FILES})
def health(before):
 assert state()==before,'protected process/PPP/USB/route/config changed'
 f=json.loads(Path('/run/ec20-failover/status.json').read_text());t=datetime.datetime.strptime(f['updated'],'%Y-%m-%dT%H:%M:%S%z').timestamp();assert abs(time.time()-t)<30 and f['active']=='wired' and f['wired_healthy'] and f['cellular_present'] and f['cellular_healthy'] and len(f['vpns'])==3 and all(f['vpns'].values()),'network health guard failed'
 return f
if os.geteuid()!=0:raise SystemExit('sudo required')
if '--restore' in sys.argv:
 before=json.loads((ROOT/'before.json').read_text());assert owner()==before['owner'],'MM identity changed; refusing to target another instance';assert prop(DEST+'.Modem','DeviceIdentifier')==before['device_id'];setup(before['enabled'],before['signals']);assert int(prop(LOC,'Enabled'))==before['enabled'] and (prop(LOC,'SignalsLocation')=='true')==before['signals'];print('Original location source mask and signalling restored; no service changes');raise SystemExit(0)
assert prop(DEST+'.Modem','DeviceIdentifier')=='0000000000000000000000000000000000000000';assert prop(DEST+'.Modem','Revision')=='EC20CEFAGR08A03M4G';assert int(prop(LOC,'Capabilities'))&6==6
assert int(prop(LOC,'Enabled'))==1 and prop(LOC,'SignalsLocation')=='false','unexpected prior GNSS state'
baseline=state();health(baseline)
# Application validation guards also avoid competing with active voice/SMS.
import http.cookiejar,urllib.request,ssl
origin='https://192.0.2.10:8788';c=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile='/etc/ec20-audio-loopback/server.crt')))
creds=Path('/etc/ec20-console/credentials.json').read_bytes();request=urllib.request.Request(origin+'/api/login',data=creds,headers={'Origin':origin,'Content-Type':'application/json'})
with c.open(request,timeout=10) as r:session=json.load(r)
with c.open(origin+'/api/status',timeout=10) as r:status=json.load(r)
assert status['sms_send_allowed'] and not [x for x in status['calls'] if x['state_code']!=7],'call/SMS active; defer probe'
ROOT.mkdir(mode=0o700,parents=True,exist_ok=False)
before={'owner':owner(),'device_id':prop(DEST+'.Modem','DeviceIdentifier'),'enabled':int(prop(LOC,'Enabled')),'signals':False,'baseline':baseline}
(ROOT/'before.json').write_text(json.dumps(before,indent=2));os.chmod(ROOT/'before.json',0o600)
import shutil
shutil.copy2(__file__,ROOT/'probe.py');os.chmod(ROOT/'probe.py',0o700)
subprocess.run(['systemd-run','--unit=ec20-gnss-probe-restore','--on-active=120s','--timer-property=AccuracySec=1s','/usr/bin/python3',str(ROOT/'probe.py'),'--restore'],check=True)
summary={'setup_accepted':False,'healthy_samples':0,'nmea_observed':False,'raw_fix_observed':False,'original_restored':False,'services_restarted':False,'module_reset':False,'ppp_unchanged':True}
try:
 setup(before['enabled']|6,False);assert int(prop(LOC,'Enabled'))==7;summary['setup_accepted']=True;print('GNSS raw/NMEA enabled through ModemManager; observing network continuity',flush=True)
 for i in range(70):
  health(baseline);summary['healthy_samples']+=1
  if i%5==0:
   location=json.loads(cmd('mmcli','-m','0','--location-get','-J'))
   p=ROOT/'sample-latest.json';p.write_text(json.dumps(location,ensure_ascii=False));os.chmod(p,0o600)
   encoded=json.dumps(location);summary['nmea_observed']|=bool(re.search(r'\$[A-Z0-9]{5},',encoded))
   def find_fix(v):
    if isinstance(v,dict):
     for key,value in v.items():
      if key=='latitude' and str(value) not in ['--','', 'None']:return True
      if find_fix(value):return True
    elif isinstance(v,list):return any(find_fix(x) for x in v)
    return False
   summary['raw_fix_observed']|=find_fix(location)
  time.sleep(1)
finally:
 try:
  assert owner()==before['owner'];setup(before['enabled'],before['signals']);summary['original_restored']=int(prop(LOC,'Enabled'))==before['enabled'] and prop(LOC,'SignalsLocation')=='false';health(baseline)
  if summary['original_restored']:subprocess.run(['systemctl','stop','ec20-gnss-probe-restore.timer'],check=True)
 finally:
  (ROOT/'summary.json').write_text(json.dumps(summary,indent=2));os.chmod(ROOT/'summary.json',0o600);print(json.dumps(summary),flush=True)
