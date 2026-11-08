#!/usr/bin/python3
"""Host 69: add an isolated recovery service, with an independent deployment rollback."""
import datetime
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import sys
import time
import urllib.request

STAGE=Path(__file__).resolve().parent
BACKUP=Path('/var/lib/ec20-modem-recovery-deploy/20261108-r2')
SERVICE='ec20-modem-recovery.service'
TIMER='ec20-modem-recovery-deploy-rollback-r2'
FILES=['/usr/local/sbin/ec20-failover','/etc/ec20-failover.json',
       '/etc/NetworkManager/system-connections/ec20-cellular.nmconnection',
       '/etc/systemd/system/ec20-console.service','/etc/systemd/system/ec20-audio-loopback.service',
       '/etc/systemd/system/ec20-gnss-broker.service','/etc/systemd/system/ec20-failover.service',
       '/etc/polkit-1/rules.d/49-ec20-console.rules','/etc/ec20-console/credentials.json']
PROTECTED=['ec20-failover','NetworkManager','ec20-console','ec20-audio-loopback','ec20-gnss-broker']
INSTALL={'ec20-modem-recovery.py':'/usr/local/libexec/ec20-modem-recovery.py',
         'ec20-modem-recovery.service':'/etc/systemd/system/ec20-modem-recovery.service',
         'ec20-modem-recovery.json':'/etc/ec20-modem-recovery.json'}


def cmd(*args,timeout=15):return subprocess.check_output(args,text=True,stderr=subprocess.PIPE,timeout=timeout).strip()
def sha(p):return hashlib.sha256(Path(p).read_bytes()).hexdigest()
def network():
    d=json.loads(Path('/run/ec20-failover/status.json').read_text())
    assert 0<=time.time()-datetime.datetime.fromisoformat(d['updated']).timestamp()<25
    assert d['active']=='wired' and d['wired_healthy'] and d['cellular_healthy'] and len(d['vpns'])==3 and all(d['vpns'].values())
    return d
def protected():
    # Ignore DHCP lease lifetime countdown while preserving interface/address/route identity.
    return dict(pids={s:cmd('systemctl','show',s,'-p','MainPID','--value') for s in PROTECTED},
                wired=cmd('ip','-4','-o','addr','show','dev','enp11s0').split('\\')[0].strip(),
                routes=cmd('ip','-4','route','show','dev','enp11s0'),hashes={p:sha(p) for p in FILES})
def data_hashes():
    return {n:sha(p) if p.exists() else None for n in ['inbox.json','outbox.json','workspace.json','forwarding.json']
            for p in [Path('/var/lib/ec20-console')/n]}
def preflight():
    network()
    modems=json.loads(cmd('mmcli','-L','-J'))['modem-list'];assert len(modems)==1
    m=json.loads(cmd('mmcli','-m',modems[0],'-J'))['modem']['generic']
    assert m['device-identifier']=='0000000000000000000000000000000000000000' and m['state']=='connected'
    assert cmd('systemctl','is-enabled','ModemManager')=='enabled'
    assert not Path('/run/ec20-uac-query/state.json').exists()
    origin='https://192.0.2.10:8788'
    client=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
            urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile='/etc/ec20-audio-loopback/server.crt')))
    def req(path,body=None,csrf=''):
        headers={} if body is None else {'Origin':origin,'Content-Type':'application/json','X-CSRF-Token':csrf}
        with client.open(urllib.request.Request(origin+path,data=None if body is None else json.dumps(body).encode(),headers=headers),timeout=12) as r:return json.load(r)
    session=req('/api/login',json.loads(Path('/etc/ec20-console/credentials.json').read_text()))
    try:
        status=req('/api/status');assert status['sms_send_allowed'] and not [c for c in status['calls'] if c['state_code']!=7]
        assert not req('/api/audio/status')['active']
        assert not req('/api/forwarding')['enabled']
    finally:req('/api/logout',{},session['csrf'])
    return dict(protected=protected(),data_hashes=data_hashes(),mm_pid=cmd('systemctl','show','ModemManager','-p','MainPID','--value'),
                ppp_pid=cmd('pgrep','-x','pppd'),ppp_index=Path('/sys/class/net/ppp0/ifindex').read_text().strip(),
                ppp_ip=network()['cellular_ip'],usb_devnum=Path('/sys/bus/usb/devices/1-2/devnum').read_text().strip())
def rollback():
    subprocess.run(['systemctl','stop','ec20-modem-recovery-test-inhibit.service'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    subprocess.run(['systemctl','disable','--now',SERVICE],check=True)
    # ExecStopPost and this idempotent helper only undo owned MM maintenance.
    subprocess.run(['/usr/bin/python3',INSTALL['ec20-modem-recovery.py'],'--restore-owned-mm'],check=True)
    subprocess.run(['systemctl','stop',TIMER+'.timer'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    print('New recovery service disabled; original failover, wired, VPN and console configuration untouched')


if __name__=='__main__':
    if os.geteuid()!=0:raise SystemExit('sudo required on host 69')
    if '--rollback' in sys.argv:rollback();raise SystemExit
    before=preflight()
    assert all(not Path(p).exists() for p in INSTALL.values()),'recovery service already installed; no overwrite'
    if '--check-only' in sys.argv:print('Recovery installation preflight passed; no changes');raise SystemExit
    for name in INSTALL:assert (STAGE/name).is_file()
    BACKUP.mkdir(mode=0o700,parents=True,exist_ok=False)
    (BACKUP/'before.json').write_text(json.dumps(before,indent=2));os.chmod(BACKUP/'before.json',0o600)
    shutil.copy2(__file__,BACKUP/'rollback.py');os.chmod(BACKUP/'rollback.py',0o700)
    subprocess.run(['systemd-run','--unit='+TIMER,'--on-active=10m','--timer-property=AccuracySec=1s',
                    '/usr/bin/python3',str(BACKUP/'rollback.py'),'--rollback'],check=True)
    try:
        for name,dest in INSTALL.items():
            shutil.copyfile(STAGE/name,dest);os.chown(dest,0,0)
            os.chmod(dest,0o700 if name.endswith('.py') else 0o600 if name.endswith('.json') else 0o644)
        subprocess.run(['systemctl','daemon-reload'],check=True)
        # Read-only hardware observation before starting the sandboxed daemon.
        observation=json.loads(cmd('/usr/bin/python3',INSTALL['ec20-modem-recovery.py'],'--check',timeout=30))
        assert observation['target'] and observation['connected'] and observation['ppp'] and observation['cell_healthy']
        assert observation['fresh'] and observation['wired'] and observation['vpn'] and not observation['paused']
        subprocess.run(['systemctl','enable','--now',SERVICE],check=True)
        time.sleep(6)
        assert cmd('systemctl','is-active',SERVICE)=='active'
        status=json.loads(Path('/run/ec20-modem-recovery/status.json').read_text());assert status['phase']=='healthy'
        assert preflight()==before,'protected state or healthy data session changed on install'
    except Exception:rollback();raise
    print(json.dumps(dict(installed=True,healthy_noop=True,rollback_armed=True,original_services_unchanged=True)))
