#!/usr/bin/python3
"""One guarded QPCMV initialization per EC20 USB generation; never changes USB/RF configuration."""
from host_profile import load as load_host, FIRMWARE
import argparse,datetime,fcntl,json,os,re,select,subprocess,termios,time
from pathlib import Path

RUN=Path('/run/ec20-voice-route');STATE=Path('/var/lib/ec20-voice-route')
HOLD='ec20-voice-route-hold.service'
HOST=None
def host():
    global HOST
    if HOST is None:HOST=load_host()
    return HOST
def cmd(*args,timeout=10):return subprocess.check_output(args,text=True,stderr=subprocess.PIPE,timeout=timeout).strip()
def save(path,data,mode=0o600):
    p=path.with_suffix('.tmp')
    with os.fdopen(os.open(p,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,mode),'w') as f:
        os.fchmod(f.fileno(),mode);json.dump(data,f);f.flush();os.fsync(f.fileno())
    p.replace(path)
    fd=os.open(path.parent,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)
def probe():return json.loads(cmd('/usr/local/libexec/ec20-uac-ready','-usb-path',host()['usb_path'],timeout=5))['device']
def boot():return Path('/proc/sys/kernel/random/boot_id').read_text().strip()
def signature():
    return (cmd('ip','-4','-o','addr','show','dev',host()['wired_interface']).split('\\')[0].strip(),
            cmd('systemctl','show','NetworkManager','-p','MainPID','--value'),
            cmd('systemctl','show','ec20-failover','-p','MainPID','--value'))
def guard():
    f=json.loads(Path('/run/ec20-failover/status.json').read_text())
    assert 0<=time.time()-datetime.datetime.fromisoformat(f['updated']).timestamp()<15
    assert f['active']=='wired' and f['wired_healthy'] and f['wired_link'] and set(f['vpns'])==set(host()['vpn_interfaces']) and all(f['vpns'].values())
    assert (Path('/sys/class/net')/host()['wired_interface']/'carrier').read_text().strip()=='1'
def confirm_links():
    net=json.loads(Path('/etc/ec20-failover.json').read_text())
    assert net['wired_interface']==host()['wired_interface'] and net['cell_interface']==host()['cell_interface']
    assert len(net['vpns'])==len(host()['vpn_interfaces']) and {v['interface'] for v in net['vpns']}==set(host()['vpn_interfaces'])
    assert any(subprocess.run(['ping','-n','-I',host()['wired_interface'],'-c','1','-W','2',x],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=4).returncode==0 for x in net['probe_targets'])
    for v in net['vpns']:assert subprocess.run(['ping','-n','-I',v['interface'],'-c','1','-W','2',v['peer']],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=4).returncode==0
def secondary():
    usb=(Path('/sys/bus/usb/devices')/host()['usb_path']).resolve()
    assert (usb/'idVendor').read_text().strip()=='2c7c' and (usb/'idProduct').read_text().strip()=='0125'
    for tty in Path('/sys/class/tty').glob('ttyUSB*'):
        dev=(tty/'device').resolve()
        if usb in dev.parents:
            for p in [dev,*dev.parents]:
                if (p/'bInterfaceNumber').exists() and (p/'bInterfaceNumber').read_text().strip()=='03':return '/dev/'+tty.name
    raise RuntimeError('secondary AT port absent')
def available(port):
    p=subprocess.run(['fuser',port],capture_output=True,timeout=4)
    assert p.returncode in (0,1),'port observation failed'
    return p.returncode==1
class RouteAT:
    def __init__(self,port,check):self.port=port;self.check=check;self.fd=None;self.old=None
    def __enter__(self):
        assert available(self.port),'secondary AT port owned'
        self.fd=os.open(self.port,os.O_RDWR|os.O_NOCTTY|os.O_NONBLOCK)
        try:
            fcntl.ioctl(self.fd,termios.TIOCEXCL);self.old=termios.tcgetattr(self.fd);a=termios.tcgetattr(self.fd)
            a[0]=a[1]=a[3]=0;a[2]=termios.CLOCAL|termios.CREAD|termios.CS8;a[4]=a[5]=termios.B115200;a[6][termios.VMIN]=a[6][termios.VTIME]=0
            termios.tcsetattr(self.fd,termios.TCSANOW,a);return self
        except BaseException:os.close(self.fd);raise
    def exchange(self,text):
        if text not in ['AT+CGMR','AT+QPCMV=1,2','AT+QPCMV?']:raise ValueError('fixed voice routing commands only')
        self.check();termios.tcflush(self.fd,termios.TCIFLUSH);os.write(self.fd,(text+'\r').encode());data=b'';end=time.monotonic()+3
        while time.monotonic()<end:
            if select.select([self.fd],[],[],.1)[0]:
                b=os.read(self.fd,2048)
                if not b:break
                data+=b;assert len(data)<8192
                s=data.decode('ascii','replace').replace('\r','')
                if re.search(r'^ERROR$|^\+CME ERROR:',s,re.M):raise RuntimeError('USB voice route rejected')
                if re.search(r'^OK$',s,re.M):return s
        raise TimeoutError('USB voice routing timed out')
    def __exit__(self,*args):
        if self.fd is not None:
            try:
                if self.old is not None:termios.tcsetattr(self.fd,termios.TCSANOW,self.old)
                fcntl.ioctl(self.fd,termios.TIOCNXCL)
            except OSError:pass
            os.close(self.fd)
def route_on(reply):return any(re.fullmatch(r'\+QPCMV:\s*1(?:,\s*2)?',x.strip()) for x in reply.splitlines())
def restore():
    p=RUN/'owner.json'
    if p.exists():
        d=json.loads(p.read_text())
        if d.get('owned') is True and d.get('unit')==HOLD:
            subprocess.run(['systemctl','stop',HOLD],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=10)
            p.unlink(missing_ok=True)
def main():
    a=argparse.ArgumentParser(description=__doc__);a.add_argument('--restore',action='store_true');args=a.parse_args()
    assert os.geteuid()==0
    RUN.mkdir(mode=0o755,exist_ok=True);STATE.mkdir(mode=0o700,exist_ok=True)
    if args.restore:restore();return
    if not Path('/etc/ec20-call-audio.enabled').exists():return
    with (RUN/'lock').open('w') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        try:d=probe()
        except subprocess.CalledProcessError as e:
            if e.returncode==2:return
            raise
        generation=boot()+'|'+d['fingerprint'];ready=RUN/'ready.json'
        if ready.exists():
            old=json.loads(ready.read_text())
            if old.get('verified') is True and old.get('boot')==boot() and old.get('fingerprint')==d['fingerprint']:return
        try:guard();confirm_links()
        except (AssertionError,OSError,subprocess.SubprocessError):
            return
        wire=signature();mm_pid=cmd('systemctl','show','ModemManager','-p','MainPID','--value')
        assert not Path('/run/ec20-uac-query/state.json').exists()
        assert 'masked' not in cmd('systemctl','show','ModemManager','-p','UnitFileState','--value')
        attempt=STATE/'attempt.json'
        if attempt.exists() and json.loads(attempt.read_text()).get('generation')==generation:return
        paths=json.loads(cmd('mmcli','-L','-J'))['modem-list']
        if len(paths)!=1:return
        modem=json.loads(cmd('mmcli','-m',paths[0],'-J'))['modem']['generic']
        assert modem['device-identifier']==host()['device_id'] and modem['revision']==FIRMWARE
        if modem['state']!='connected':return
        calls=json.loads(cmd('mmcli','-m',paths[0],'--voice-list-calls','-J')).get('modem.voice.call',[])
        for call in calls:
            if json.loads(cmd('mmcli','-o',call,'-J'))['call']['properties']['state']!='terminated':return
        outbox=Path('/var/lib/ec20-console/outbox.json')
        if outbox.exists() and any(x['status'] in ['pending','creating','ready','submitting'] for x in json.loads(outbox.read_text())):return
        save(attempt,dict(generation=generation,at=time.time()))
        save(RUN/'owner.json',dict(owned=True,unit=HOLD,created=time.time()))
        subprocess.run(['systemd-run','--unit='+HOLD,'--collect','--property=RuntimeMaxSec=45s','/usr/bin/mmcli','-m',paths[0],'--inhibit'],check=True)
        try:
            end=time.monotonic()+12;port=secondary()
            while not available(port):
                assert time.monotonic()<end,'MM has not released secondary AT';guard();time.sleep(.25)
            def check():
                guard();assert signature()==wire and probe()['fingerprint']==d['fingerprint']
                assert cmd('systemctl','show','ModemManager','-p','MainPID','--value')==mm_pid
            with RouteAT(port,check) as at:
                assert FIRMWARE in at.exchange('AT+CGMR');at.exchange('AT+QPCMV=1,2');assert route_on(at.exchange('AT+QPCMV?'))
            check();save(ready,dict(fingerprint=d['fingerprint'],boot=boot(),verified=True,at=time.time()),0o644)
            print('EC20 USB voice route verified for current generation',flush=True)
        finally:
            restore()
if __name__=='__main__':main()
