#!/usr/bin/python3
"""Pinned EC20 UAC migration. Root-only backup, owned MM restoration and timed rollback."""
import argparse,datetime,fcntl,hashlib,http.cookiejar,importlib.util,json,os,re,select,shutil,ssl,subprocess,termios,time,urllib.request
from pathlib import Path
ROOT=Path('/var/lib/ec20-uac-enable-20261108')
STAGE=Path(__file__).resolve().parent
MM='ModemManager.service'
TIMER='ec20-uac-enable-rollback-r3'
OLD='0x2C7C,0x0125,1,1,1,1,1,0,0'
NEW='0x2C7C,0x0125,1,1,1,1,1,0,1'
DEVICE='0000000000000000000000000000000000000000'
FIRMWARE='EC20CEFAGR08A03M4G'
MANAGED={
 '/usr/local/libexec/ec20-uac-ready':'ec20-uac-ready-linux-amd64',
 '/usr/local/libexec/ec20-voice-route.py':'ec20-voice-route.py',
 '/usr/local/libexec/ec20-modem-recovery.py':'ec20-modem-recovery.py',
 '/etc/udev/rules.d/99-ec20-call-audio.rules':'99-ec20-call-audio.rules',
 '/etc/systemd/system/ec20-voice-route.service':'ec20-voice-route.service',
 '/etc/systemd/system/ec20-voice-route.timer':'ec20-voice-route.timer',
 '/etc/systemd/system/ec20-audio-loopback.service.d/40-uac.conf':'ec20-call-audio.override.conf',
 '/etc/systemd/system/ec20-console.service.d/40-uac.conf':'ec20-console-voice.override.conf',
 '/etc/ec20-call-audio.enabled':None}
NETWORK_FILES=['/usr/local/sbin/ec20-failover','/etc/ec20-failover.json','/etc/NetworkManager/system-connections/ec20-cellular.nmconnection',
 '/etc/systemd/system/ec20-failover.service','/etc/polkit-1/rules.d/49-ec20-console.rules']
def cmd(*args,timeout=15):return subprocess.check_output(args,text=True,stderr=subprocess.PIPE,timeout=timeout).strip()
def sha(p):return hashlib.sha256(Path(p).read_bytes()).hexdigest()
def save(p,v):p.write_text(json.dumps(v,indent=2));p.chmod(0o600)
def wired():
 s=json.loads(Path('/run/ec20-failover/status.json').read_text())
 assert 0<=time.time()-datetime.datetime.fromisoformat(s['updated']).timestamp()<20 and s['active']=='wired' and s['wired_healthy'] and s['wired_link'] and len(s['vpns'])==3 and all(s['vpns'].values()),'wired/VPN guard'
 assert Path('/sys/class/net/enp11s0/carrier').read_text().strip()=='1'
 return s
def protected():return dict(pids={s:cmd('systemctl','show',s,'-p','MainPID','--value') for s in ['NetworkManager','ec20-failover','ec20-gnss-broker']},
 wired=cmd('ip','-4','-o','addr','show','dev','enp11s0').split('\\')[0].strip(),routes=cmd('ip','-4','route','show','dev','enp11s0'),hashes={p:sha(p) for p in NETWORK_FILES})
def modem():
 paths=json.loads(cmd('mmcli','-L','-J'))['modem-list'];assert len(paths)==1
 m=json.loads(cmd('mmcli','-m',paths[0],'-J'))['modem']['generic'];assert m['device-identifier']==DEVICE and m['revision']==FIRMWARE
 return paths[0],m
def api():
 origin='https://192.0.2.10:8788';c=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile='/etc/ec20-audio-loopback/server.crt')))
 def request(path,body=None,csrf=''):
  h={} if body is None else {'Origin':origin,'Content-Type':'application/json','X-CSRF-Token':csrf}
  with c.open(urllib.request.Request(origin+path,data=None if body is None else json.dumps(body).encode(),headers=h),timeout=15) as r:return json.load(r)
 session=request('/api/login',json.loads(Path('/etc/ec20-console/credentials.json').read_text()))
 return request,session
def preflight():
 s=wired();assert s['cellular_healthy'] and s['cellular_ip'];_,m=modem();assert m['state']=='connected'
 assert cmd('systemctl','is-enabled','ModemManager')=='enabled' and not Path('/run/ec20-uac-query/state.json').exists()
 assert not Path('/run/ec20-voice-route/owner.json').exists()
 request,session=api()
 try:
  x=request('/api/status');assert x['sms_send_allowed'] and not [c for c in x['calls'] if c['state_code']!=7]
  assert not request('/api/audio/status')['active'];assert not request('/api/forwarding')['enabled']
 finally:request('/api/logout',{},session['csrf'])
 return protected()
def restore_mm():
 p=ROOT/'owner.json'
 if not p.exists():return
 if not json.loads(p.read_text()).get('owned'):return
 assert not Path('/etc/systemd/system/ModemManager.service').is_symlink(),'persistent MM override'
 cmd('systemctl','unmask','--runtime',MM);cmd('systemctl','start','--no-block',MM);p.unlink()
def stop_mm():
 wired()
 assert cmd('systemctl','is-enabled','ModemManager')=='enabled'
 assert not (ROOT/'owner.json').exists()
 timer='ec20-uac-enable-restore-mm-'+str(time.time_ns())
 save(ROOT/'owner.json',{'owned':True,'timer':timer})
 cmd('systemd-run','--unit='+timer,'--on-active=180s','--timer-property=AccuracySec=1s','/usr/bin/python3',str(ROOT/'rollback.py'),'--restore-mm')
 cmd('systemctl','mask','--runtime',MM);cmd('systemctl','stop',MM,timeout=35)
 assert cmd('systemctl','show',MM,'-p','MainPID','--value')=='0'
def restored():
 p=ROOT/'owner.json';timer=json.loads(p.read_text()).get('timer') if p.exists() else None
 restore_mm()
 if timer:subprocess.run(['systemctl','stop',timer+'.timer'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
class AT:
 def __enter__(self):
  # The reviewed UAC reader supplies strict USB/interface03 validation and owner checks.
  spec=importlib.util.spec_from_file_location('uac_read','/usr/local/libexec/ec20-check-uac.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
  port,_=m.validate_port('/dev/serial/by-id/usb-Android_Android_0123456789ABCDEF-if03-port0');assert not m.owners(port)
  self.fd=os.open(port,os.O_RDWR|os.O_NOCTTY|os.O_NONBLOCK)
  try:
   fcntl.ioctl(self.fd,termios.TIOCEXCL);self.old=termios.tcgetattr(self.fd);a=termios.tcgetattr(self.fd)
   a[0]=a[1]=a[3]=0;a[2]=termios.CLOCAL|termios.CREAD|termios.CS8;a[4]=a[5]=termios.B115200;a[6][termios.VMIN]=a[6][termios.VTIME]=0;termios.tcsetattr(self.fd,termios.TCSANOW,a);return self
  except BaseException:os.close(self.fd);raise
 def query(self,text):
  assert text in ['AT+CGMR','AT+QCFG="usbcfg"','AT+QCFG="usbcfg",'+OLD,'AT+QCFG="usbcfg",'+NEW,'AT+CFUN=1,1']
  wired();termios.tcflush(self.fd,termios.TCIFLUSH);os.write(self.fd,(text+'\r').encode());data=b'';end=time.monotonic()+(8 if text=='AT+CFUN=1,1' else 3)
  while time.monotonic()<end:
   if select.select([self.fd],[],[],.1)[0]:
    try:b=os.read(self.fd,4096)
    except OSError:
     if text=='AT+CFUN=1,1':return 'USB reset in progress'
     raise
    if not b:break
    data+=b;assert len(data)<16384;s=data.decode('ascii','replace').replace('\r','')
    if re.search(r'^ERROR$|^\+CME ERROR:',s,re.M):raise RuntimeError('module rejected UAC command')
    if re.search(r'^OK$',s,re.M):return s
  raise TimeoutError('UAC command did not return OK')
 def __exit__(self,*_):
  try:termios.tcsetattr(self.fd,termios.TCSANOW,self.old);fcntl.ioctl(self.fd,termios.TIOCNXCL)
  except OSError:pass
  os.close(self.fd)
def tuple_from(reply):
 line=next(x for x in reply.splitlines() if x.startswith('+QCFG: "usbcfg",'));fields=[x.strip().lower() for x in line.split(',')[1:]]
 assert len(fields)==9 and [int(x,0) for x in fields]==[0x2c7c,0x125,1,1,1,1,1,0,int(fields[-1])]
 assert fields[-1] in ('0','1');return NEW if fields[-1]=='1' else OLD
def switch(target):
 with AT() as at:
  assert FIRMWARE in at.query('AT+CGMR');current=tuple_from(at.query('AT+QCFG="usbcfg"'))
  if current==target:
   audio=any((p/'bInterfaceClass').read_text().strip()=='01' for p in Path('/sys/bus/usb/devices/1-2').glob('1-2:*') if (p/'bInterfaceClass').exists())
   if audio==(target==NEW):return False
   at.query('AT+CFUN=1,1');return True
  at.query('AT+QCFG="usbcfg",'+target);assert tuple_from(at.query('AT+QCFG="usbcfg"'))==target
  print('UAC function bit saved; restarting EC20',flush=True);at.query('AT+CFUN=1,1');return True
def wait_usb():
 end=time.monotonic()+60;seen=None;stable=None
 while time.monotonic()<end:
  wired()
  try:
   p=subprocess.run(['/usr/local/libexec/ec20-uac-ready'],capture_output=True,text=True,timeout=5)
   if p.returncode==0:
    d=json.loads(p.stdout)['device'];fingerprint=d['fingerprint']
    if seen!=fingerprint:seen=fingerprint;stable=time.monotonic()
    # This firmware enumerates twice during CFUN reboot. Reject the short first generation.
    if time.monotonic()-stable>=12 and Path('/dev/serial/by-id/usb-Android_Android_0123456789ABCDEF-if03-port0').exists():return d
   else:seen=None;stable=None
  except (OSError,subprocess.SubprocessError):seen=None;stable=None
  time.sleep(2)
 raise RuntimeError('stable EC20 UAC/secondary AT generation not ready')
def wait_cell():
 end=time.monotonic()+135;good=0
 while time.monotonic()<end:
  s=wired()
  try:_,m=modem();ok=m['state']=='connected' and s['cellular_healthy'] and s.get('cellular_ip') and Path('/sys/class/net/ppp0').exists()
  except Exception:ok=False
  good=good+1 if ok else 0
  if good>=3:
   r=subprocess.run(['curl','--noproxy','*','--interface','ppp0','-4','--connect-timeout','5','--max-time','12','-sS','-o','/dev/null','-w','%{http_code}','https://www.baidu.com/'],capture_output=True,text=True,timeout=15)
   if r.returncode==0 and r.stdout=='200':return
  time.sleep(10)
 raise RuntimeError('fresh cellular standby failed verification')
def restore_gnss(before):
 if before['gnss_sources']=='u 7':
  wired();path,_=modem();cmd('busctl','call','org.freedesktop.ModemManager1',path,'org.freedesktop.ModemManager1.Modem.Location','Setup','ub','7','false')
  assert cmd('busctl','get-property','org.freedesktop.ModemManager1',path,'org.freedesktop.ModemManager1.Modem.Location','Enabled')=='u 7'
def link(path,target):
 p=Path(str(path)+'.pending');p.unlink(missing_ok=True);p.symlink_to(target);p.replace(path)
def rollback_usb():
 # Added UAC endpoints can safely stay present while cellular is the only
 # remote path. Never tear that path down merely to restore the USB layout.
 wired()
 if not (ROOT/'owner.json').exists():stop_mm()
 deadline=time.monotonic()+50
 while True:
  try:switch(OLD);return
  except (FileNotFoundError,OSError,TimeoutError):
   if time.monotonic()>=deadline:raise
   wired();time.sleep(2)
def rollback():
 before=json.loads((ROOT/'before.json').read_text());errors=[]
 subprocess.run(['systemctl','stop','ec20-modem-recovery','ec20-voice-route.timer','ec20-voice-route.service'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 subprocess.run(['systemctl','disable','ec20-voice-route.timer'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 try:
  rollback_usb()
 except Exception as e:errors.append('USB rollback needs review: '+str(e))
 finally:restored()
 for dest,meta in before['files'].items():
  p=Path(dest)
  if meta is None:p.unlink(missing_ok=True)
  else:shutil.copy2(ROOT/'files'/meta['name'],p);os.chmod(p,meta['mode'])
 for name,target in before['releases'].items():link(Path('/opt')/name/'current',target)
 Path('/run/ec20-voice-route/ready.json').unlink(missing_ok=True)
 cmd('udevadm','control','--reload-rules');cmd('systemctl','daemon-reload')
 cmd('systemctl','restart','ec20-audio-loopback','ec20-console')
 try:wait_cell();restore_gnss(before)
 except Exception as e:errors.append('standby/GNSS recovery needs review: '+str(e))
 if before['recovery_active']:cmd('systemctl','start','ec20-modem-recovery')
 subprocess.run(['systemctl','stop',TIMER+'.timer'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 save(ROOT/'rollback-result.json',dict(errors=errors,network_config_unchanged=protected()==before['protected']))
 print(json.dumps(dict(rolled_back=True,errors=errors)),flush=True)
def install(resume=False):
 baseline=preflight()
 if (ROOT/'before.json').exists():assert resume,'migration already attempted; review backup first'
 else:assert not resume,'no original migration backup'
 ROOT.mkdir(mode=0o700,parents=True,exist_ok=True);(ROOT/'files').mkdir(mode=0o700,exist_ok=True)
 if resume:
  before=json.loads((ROOT/'before.json').read_text());assert before['protected']==baseline
 else:
  files={}
  for i,dest in enumerate(MANAGED):
   p=Path(dest);files[dest]=None
   if p.exists():
    name=str(i);shutil.copy2(p,ROOT/'files'/name);files[dest]=dict(name=name,mode=p.stat().st_mode&0o777)
  path,m=modem();sources=cmd('busctl','get-property','org.freedesktop.ModemManager1',path,'org.freedesktop.ModemManager1.Modem.Location','Enabled')
  assert sources in ['u 1','u 7'],'unknown GNSS state'
  before=dict(protected=baseline,files=files,releases={name:str((Path('/opt')/name/'current').resolve()) for name in ['ec20-console','ec20-audio-loopback']},recovery_active=cmd('systemctl','is-active','ec20-modem-recovery')=='active',gnss_sources=sources)
 if not resume:save(ROOT/'before.json',before)
 shutil.copy2(__file__,ROOT/'rollback.py');(ROOT/'rollback.py').chmod(0o700)
 cmd('systemd-run','--unit='+TIMER,'--on-active=20m','--timer-property=AccuracySec=1s','/usr/bin/python3',str(ROOT/'rollback.py'),'--rollback')
 try:
  cmd('systemctl','stop','ec20-modem-recovery')
  if subprocess.run(['getent','group','ec20-call-audio'],stdout=subprocess.DEVNULL).returncode:cmd('groupadd','--system','ec20-call-audio')
  if subprocess.run(['id','ec20-call-audio'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode:cmd('useradd','--system','--gid','ec20-call-audio','--no-create-home','--home-dir','/nonexistent','--shell','/usr/sbin/nologin','ec20-call-audio')
  for dest,source in MANAGED.items():
   p=Path(dest);p.parent.mkdir(parents=True,exist_ok=True)
   if source is None:p.write_text('enabled\n')
   else:shutil.copyfile(STAGE/source,p)
   p.chmod(0o755 if dest.endswith('.py') or dest.endswith('ec20-uac-ready') else 0o600 if dest.endswith('.enabled') else 0o644)
  cmd('udevadm','control','--reload-rules');cmd('systemctl','daemon-reload')
  stop_mm()
  try:
   switch(NEW);wait_usb()
   # Resume may reuse an already enumerated UAC device; reloading rules alone
   # does not update its existing nodes. Trigger only this USB's sound children.
   cmd('udevadm','trigger','--action=change','--subsystem-match=sound','--parent-match=/sys/bus/usb/devices/1-2','--settle')
   cmd('/usr/bin/python3','/usr/local/libexec/ec20-voice-route.py','--prepare-stopped',timeout=20)
  finally:restored()
  wait_cell();restore_gnss(before);assert protected()==baseline
  # Both AT ports must remain managed: one for PPP, one for calls/SMS/management.
  _,m=modem();assert any('ttyUSB3 (at)'==x for x in m['ports']) and m['primary-port']=='ttyUSB2'
  for account in ['ec20-console','ec20-call-audio']:
   data=json.loads(cmd('runuser','-u',account,'--','/usr/local/libexec/ec20-uac-ready','-voice-route',timeout=10));assert data['ready']
  for name,binary in [('ec20-console','ec20-console-linux-amd64'),('ec20-audio-loopback','ec20-audio-loopback-linux-amd64')]:
   target=Path('/opt')/name/'releases'/sha(STAGE/binary)[:16];target.mkdir(parents=True,exist_ok=True)
   shutil.copyfile(STAGE/binary,target/name);(target/name).chmod(0o755);link(Path('/opt')/name/'current',target)
  cmd('systemctl','restart','ec20-audio-loopback','ec20-console')
  cmd('systemctl','enable','--now','ec20-voice-route.timer');cmd('systemctl','start','ec20-modem-recovery')
  time.sleep(3);network=wired();assert network['cellular_healthy'];assert protected()==baseline
  request,session=api()
  try:
   assert request('/api/status')['version']=='1.7.0';assert request('/api/audio/status')['voice_ready']
  finally:request('/api/logout',{},session['csrf'])
  save(ROOT/'installation.json',dict(uac_enabled=True,route_verified=True,cellular_https=200,protected_network_unchanged=True,both_mm_at_ports_preserved=True,rollback_armed=True))
  print(json.dumps(dict(installed=True,uac_ready=True,both_mm_at_ports_preserved=True,rollback_armed=True)),flush=True)
 except Exception as e:
  detail=dict(error=str(e),stderr=str(getattr(e,'stderr','')),stdout=str(getattr(e,'output','')))
  save(ROOT/'failure.json',detail)
  for stream in Path('/proc/asound').glob('card*/stream0'):
   (ROOT/('failure-'+stream.parent.name+'-stream.txt')).write_text(stream.read_text())
  rollback();raise
if __name__=='__main__':
 assert os.geteuid()==0
 p=argparse.ArgumentParser();p.add_argument('--rollback',action='store_true');p.add_argument('--restore-mm',action='store_true');p.add_argument('--check',action='store_true');p.add_argument('--resume',action='store_true');a=p.parse_args()
 if a.restore_mm:restore_mm()
 elif a.rollback:rollback()
 elif a.check:preflight();print('UAC migration preflight passed; no changes')
 else:install(a.resume)
