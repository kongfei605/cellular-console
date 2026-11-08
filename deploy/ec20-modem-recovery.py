#!/usr/bin/python3
"""Bounded recovery of a USB-present but MM-missing EC20. Never manages routes/VPNs."""
import argparse
import datetime
import fcntl
import hashlib
import json
import logging
import os
from pathlib import Path
import re
import select
import signal
import subprocess
import termios
import time

CONFIG = Path('/etc/ec20-modem-recovery.json')
STATE = Path('/var/lib/ec20-modem-recovery')
RUNTIME = Path('/run/ec20-modem-recovery')
HELPER = '/usr/local/libexec/ec20-modem-recovery.py'
RESTORE_UNIT = 'ec20-modem-recovery-mm-restore'
MM = 'ModemManager.service'
DEFAULTS = dict(enabled=False, usb_path='1-2', vendor='2c7c', product='0125',
                device_id='0000000000000000000000000000000000000000',
                firmware='EC20CEFAGR08A03M4G', fault_seconds=90,
                scan_wait=30, rediscover_wait=75, reset_wait=120,
                verify_seconds=20, interval_seconds=5, cycle_spacing=900,
                cycle_per_hour=3, cycle_per_day=6, reset_spacing=21600,
                reset_per_day=2, failure_breaker_seconds=3600)


def command(args, timeout=10):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE, timeout=timeout).strip()


def atomic_json(path, value, mode=0o600):
    temporary = path.with_suffix('.tmp')
    with os.fdopen(os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode), 'w') as f:
        os.fchmod(f.fileno(), mode)
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write('\n'); f.flush(); os.fsync(f.fileno())
    temporary.replace(path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)


def load_config():
    settings = json.loads(CONFIG.read_text())
    if not isinstance(settings, dict) or set(settings) - set(DEFAULTS):
        raise ValueError('unknown recovery configuration')
    cfg = {**DEFAULTS, **settings}
    if type(cfg['enabled']) is not bool or cfg['usb_path'] != '1-2' or (cfg['vendor'], cfg['product']) != ('2c7c', '0125'):
        raise ValueError('unsupported hardware profile')
    for key in ['device_id', 'firmware']:
        if cfg[key] != DEFAULTS[key]: raise ValueError('unverified hardware profile')
    # Production budgets cannot be shortened by a config typo or a test setting.
    for key in ['fault_seconds', 'scan_wait', 'rediscover_wait', 'reset_wait', 'verify_seconds', 'cycle_spacing', 'reset_spacing', 'failure_breaker_seconds']:
        if type(cfg[key]) is not int or not DEFAULTS[key] <= cfg[key] <= 86400:
            raise ValueError('unsafe time budget: ' + key)
    for key in ['cycle_per_hour', 'cycle_per_day', 'reset_per_day']:
        if type(cfg[key]) is not int or not 1 <= cfg[key] <= DEFAULTS[key]:
            raise ValueError('unsafe action limit: ' + key)
    if not 5 <= cfg['interval_seconds'] <= 30: raise ValueError('unsafe polling interval')
    return cfg


class Ledger:
    def __init__(self, path):
        self.path = path
        self.data = dict(cycles=[], resets=[], failures=0, blocked_until=0, last_time=0)
        if path.exists():
            self.data = json.loads(path.read_text())
            if (set(self.data) != {'cycles', 'resets', 'failures', 'blocked_until', 'last_time'}
                    or not all(type(self.data[k]) in (int, float) and self.data[k] >= 0 for k in ['failures', 'blocked_until', 'last_time'])
                    or not all(isinstance(self.data[k], list) and len(self.data[k]) <= 100 and all(type(v) in (int, float) and v >= 0 for v in self.data[k]) for k in ['cycles', 'resets'])):
                raise ValueError('invalid recovery ledger; refusing actions')

    def allowed(self, kind, now, cfg):
        if now < self.data['last_time'] - 5: return '系统时间回退，暂停恢复'
        if now < self.data['blocked_until']: return '失败熔断冷却中'
        values = self.data['cycles' if kind == 'cycle' else 'resets']
        spacing = cfg['cycle_spacing' if kind == 'cycle' else 'reset_spacing']
        if values and now - max(values) < spacing: return '恢复限频冷却中'
        if kind == 'cycle' and sum(now - t < 3600 for t in values) >= cfg['cycle_per_hour']:
            return '达到每小时恢复上限'
        limit = cfg['cycle_per_day' if kind == 'cycle' else 'reset_per_day']
        if sum(now - t < 86400 for t in values) >= limit: return '达到24小时恢复上限'
        return ''

    def reserve(self, kind, now, cfg):
        reason = self.allowed(kind, now, cfg)
        if reason: raise RuntimeError(reason)
        data = {**self.data, 'cycles': [t for t in self.data['cycles'] if now-t < 86400],
                'resets': [t for t in self.data['resets'] if now-t < 86400], 'last_time': now}
        data['cycles' if kind == 'cycle' else 'resets'].append(now)
        atomic_json(self.path, data); self.data = data

    def finish(self, success, now, cfg):
        data = {**self.data, 'last_time': max(now, self.data['last_time'])}
        data['failures'] = 0 if success else data['failures'] + 1
        if data['failures'] >= 3: data['blocked_until'] = now + cfg['failure_breaker_seconds']
        atomic_json(self.path, data); self.data = data


def network_guard(s):
    return (s.get('fresh') and s.get('wired') and s.get('vpn') and s.get('usb')
            and not s.get('paused') and not s.get('other_modems'))


def missing_fault(s):
    return (network_guard(s) and s.get('mm_active') and not s.get('mm_masked')
            and s.get('mm_known') and not s.get('target') and not s.get('ppp')
            and not s.get('cell_healthy') and not s.get('sms_busy') and not s.get('ppp_running'))


def recovered(s):
    return (network_guard(s) and s.get('target') and s.get('connected') and s.get('ppp')
            and s.get('cell_healthy') and s.get('ip_matches'))


class LinuxOps:
    def __init__(self, cfg):
        self.cfg = cfg
        self.net = json.loads(Path('/etc/ec20-failover.json').read_text())
        assert self.net['wired_interface'] == 'enp11s0' and self.net['cell_interface'] == 'ppp0'
        assert len(self.net['vpns']) == 3
        self.usb = Path('/sys/bus/usb/devices') / cfg['usb_path']

    def usb_identity(self):
        try:
            if ((self.usb/'idVendor').read_text().strip(), (self.usb/'idProduct').read_text().strip(),
                    (self.usb/'authorized').read_text().strip()) != (self.cfg['vendor'], self.cfg['product'], '1'):
                return ''
            return str(self.usb.resolve()) + ':' + (self.usb/'devnum').read_text().strip()
        except OSError: return ''

    def snapshot(self):
        s = dict(usb=self.usb_identity(), mm_known=False, target=False, other_modems=False,
                 connected=False, ppp=Path('/sys/class/net/ppp0').exists(), ip_matches=False)
        f = json.loads(Path('/run/ec20-failover/status.json').read_text())
        age = time.time() - datetime.datetime.fromisoformat(f['updated']).timestamp()
        s['fresh'] = 0 <= age < 15
        addresses = json.loads(command(['ip', '-4', '-j', 'addr', 'show', 'dev', 'enp11s0']))
        ips = [a['local'] for d in addresses for a in d.get('addr_info', []) if a.get('scope') == 'global']
        carrier = Path('/sys/class/net/enp11s0/carrier').read_text().strip() == '1'
        s['wired'] = (f.get('active') == 'wired' and f.get('wired_healthy') is True and
                      f.get('wired_link') is True and carrier and f.get('wired_ip') in ips)
        s['vpn'] = len(f.get('vpns', {})) == 3 and all(f['vpns'].get(v['interface']) is True for v in self.net['vpns'])
        s['cell_healthy'] = f.get('cellular_healthy') is True and bool(f.get('cellular_ip'))
        s['signature'] = (tuple(sorted(ips)), command(['systemctl', 'show', '-p', 'MainPID', '--value', 'NetworkManager']),
                          command(['systemctl', 'show', '-p', 'MainPID', '--value', 'ec20-failover']))
        if s['ppp']:
            try:
                ppp = json.loads(command(['ip', '-4', '-j', 'addr', 'show', 'dev', 'ppp0']))
                s['ip_matches'] = any(a.get('local') == f.get('cellular_ip') for d in ppp for a in d.get('addr_info', []))
            except subprocess.CalledProcessError:
                if Path('/sys/class/net/ppp0').exists(): raise
                s['ppp'] = False  # Normal link removal between exists() and ip(8).
        s['paused'] = (RUNTIME/'paused').exists() or Path('/run/ec20-uac-query/state.json').exists()
        s['mm_active'] = command(['systemctl', 'show', MM, '-p', 'ActiveState', '--value']) == 'active'
        s['mm_masked'] = 'masked' in command(['systemctl', 'show', MM, '-p', 'UnitFileState', '--value'])
        outbox = Path('/var/lib/ec20-console/outbox.json')
        s['sms_busy'] = False
        ppp_process = subprocess.run(['pgrep', '-x', 'pppd'], capture_output=True, timeout=5)
        if ppp_process.returncode not in (0, 1): raise RuntimeError('PPP process observation failed')
        s['ppp_running'] = ppp_process.returncode == 0
        if outbox.exists():
            data = json.loads(outbox.read_text())
            records = data if isinstance(data, list) else data.get('messages', [])
            s['sms_busy'] = any(m.get('status') in ('pending', 'creating', 'ready', 'submitting') for m in records)
        if s['mm_active'] and not s['mm_masked']:
            try:
                paths = json.loads(command(['mmcli', '-L', '-J'], 7))['modem-list']
                if len(paths) > 4: raise RuntimeError('unexpected modem count')
                for path in paths:
                    m = json.loads(command(['mmcli', '-m', path, '-J'], 7))['modem']['generic']
                    if m['device-identifier'] == self.cfg['device_id']:
                        s['target'] = True; s['connected'] = m['state'] == 'connected'
                    else: s['other_modems'] = True
                s['mm_known'] = True
            except (subprocess.SubprocessError, KeyError, ValueError): pass
        return s

    def confirm_wired(self):
        # Independently bound probes; never changes routes, DNS or a VPN session.
        ok = False
        for target in self.net['probe_targets']:
            if subprocess.run(['ping', '-n', '-I', 'enp11s0', '-c', '1', '-W', '2', target], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=4).returncode == 0:
                ok = True; break
        if not ok: return False
        for vpn in self.net['vpns']:
            if subprocess.run(['ping', '-n', '-I', vpn['interface'], '-c', '1', '-W', '2', vpn['peer']], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=4).returncode != 0:
                return False
        return True

    def scan(self): command(['mmcli', '--scan-modems'], 10)
    def rediscover(self): command(['systemctl', 'restart', MM], 30)

    def diagnostics(self, event):
        folder = STATE / ('incident-' + str(int(event)))
        folder.mkdir(mode=0o700, exist_ok=False)
        for name, args in [('mm.log', ['journalctl', '-u', MM, '-n', '80', '--no-pager', '-o', 'short-iso']),
                           ('kernel.log', ['journalctl', '-k', '-n', '80', '--no-pager', '-o', 'short-iso'])]:
            (folder/name).write_text(command(args, 10))

    def reset(self, guard):
        # A separate systemd timer and ExecStopPost restore only a mask we own.
        # If a prior maintenance owner exists, never adopt/unmask it.
        guard()
        if (RUNTIME/'owner.json').exists() or Path('/run/systemd/system/ModemManager.service').exists():
            raise RuntimeError('MM maintenance ownership conflict')
        atomic_json(RUNTIME/'owner.json', {'owned': True, 'boot': Path('/proc/sys/kernel/random/boot_id').read_text().strip()})
        try:
            command(['systemd-run', '--unit='+RESTORE_UNIT, '--on-active=90s', '--timer-property=AccuracySec=1s',
                     '/usr/bin/python3', HELPER, '--restore-owned-mm'], 10)
            deadline=time.monotonic()+60
            guard(); command(['systemctl', 'mask', '--runtime', MM], 10)
            command(['systemctl', 'stop', MM], 30)
            if command(['systemctl', 'show', MM, '-p', 'MainPID', '--value']) != '0':
                raise RuntimeError('MM still owns modem')
            # MM is intentionally masked only inside this narrow AT window.
            def at_guard():
                if time.monotonic()>deadline-15: raise RuntimeError('AT maintenance time budget exhausted')
                guard(maintenance=True)
                if time.monotonic()>deadline-15: raise RuntimeError('AT maintenance time budget exhausted')
                if command(['systemctl', 'show', MM, '-p', 'MainPID', '--value']) != '0':
                    raise RuntimeError('MM unexpectedly running')
            at_guard(); port = secondary_port(self.usb, self.cfg)
            result = subprocess.run(['fuser', port], capture_output=True, timeout=5)
            if result.returncode != 1: raise RuntimeError('secondary AT port owned')
            with FixedAT(port) as at:
                at_guard()
                if not at.exchange('AT', 3): raise RuntimeError('AT unresponsive; onsite power cycle required')
                at_guard()
                if self.cfg['firmware'] not in at.exchange('AT+CGMR', 3):
                    raise RuntimeError('firmware profile mismatch')
                at_guard()
                try: reply = at.exchange('AT+CFUN=1,1', 10)
                except OSError: reply = ''  # USB may disappear before OK is returned.
                return dict(reset_command_sent=True, acknowledged=bool(re.search(r'^OK$', reply, re.M)))
        finally:
            restore_owned_mm()
            subprocess.run(['systemctl', 'stop', RESTORE_UNIT+'.timer'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)

    def verify_data(self):
        r = subprocess.run(['curl', '--noproxy', '*', '--interface', 'ppp0', '-4', '--connect-timeout', '5', '--max-time', '12',
                            '-sS', '-o', '/dev/null', '-w', '%{http_code}', 'https://www.baidu.com/'], capture_output=True, text=True, timeout=15)
        return r.returncode == 0 and r.stdout.strip() == '200'


def secondary_port(usb, cfg):
    if ((usb/'idVendor').read_text().strip(), (usb/'idProduct').read_text().strip()) != (cfg['vendor'], cfg['product']):
        raise RuntimeError('USB identity changed')
    for tty in Path('/sys/class/tty').glob('ttyUSB*'):
        dev = (tty/'device').resolve()
        if usb.resolve() in dev.parents:
            for parent in [dev, *dev.parents]:
                if (parent/'bInterfaceNumber').exists() and (parent/'bInterfaceNumber').read_text().strip() == '03':
                    return '/dev/'+tty.name
    raise RuntimeError('verified secondary AT port absent')


class FixedAT:
    def __init__(self, port): self.port = port; self.fd = None; self.saved = None
    def __enter__(self):
        self.fd = os.open(self.port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        try:
            fcntl.ioctl(self.fd, termios.TIOCEXCL)
            self.saved = termios.tcgetattr(self.fd); a = termios.tcgetattr(self.fd)
            a[0] = a[1] = a[3] = 0; a[2] = termios.CLOCAL | termios.CREAD | termios.CS8
            a[4] = a[5] = termios.B115200; a[6][termios.VMIN] = a[6][termios.VTIME] = 0
            termios.tcsetattr(self.fd, termios.TCSANOW, a)
            return self
        except BaseException:
            os.close(self.fd); self.fd = None; raise
    def exchange(self, text, timeout):
        if text not in ('AT', 'AT+CGMR', 'AT+CFUN=1,1'): raise ValueError('fixed recovery commands only')
        termios.tcflush(self.fd, termios.TCIFLUSH); os.write(self.fd, (text+'\r').encode('ascii'))
        data = b''; end = time.monotonic()+timeout
        while time.monotonic() < end:
            if select.select([self.fd], [], [], .15)[0]:
                b = os.read(self.fd, 4096)
                if not b: break
                data += b
                if len(data) > 16384: raise RuntimeError('oversized AT reply')
                reply = data.decode('ascii', 'replace').replace('\r', '')
                if re.search(r'^(OK|ERROR|\+CME ERROR:.*)$', reply, re.M):
                    return reply if re.search(r'^OK$', reply, re.M) else ''
        return ''
    def __exit__(self, *args):
        if self.fd is not None:
            try:
                if self.saved is not None: termios.tcsetattr(self.fd, termios.TCSANOW, self.saved)
                fcntl.ioctl(self.fd, termios.TIOCNXCL)
            except OSError: pass
            os.close(self.fd)


def restore_owned_mm():
    path = RUNTIME/'owner.json'
    if not path.exists(): return
    state = json.loads(path.read_text())
    if not state.get('owned') or state.get('boot') != Path('/proc/sys/kernel/random/boot_id').read_text().strip():
        raise RuntimeError('unowned or previous-boot MM maintenance')
    if Path('/etc/systemd/system/ModemManager.service').is_symlink():
        raise RuntimeError('persistent MM override; do not unmask')
    command(['systemctl', 'unmask', '--runtime', MM], 10)
    command(['systemctl', 'start', '--no-block', MM], 10)
    path.unlink()


class Recovery:
    def __init__(self, cfg, ops, ledger, clock=time, status=RUNTIME/'status.json'):
        self.cfg=cfg; self.ops=ops; self.ledger=ledger; self.clock=clock; self.status=status
        self.fault_since=None; self.signature=None; self.phase='idle'; self.last_result=None
        self.last_reason=None

    def publish(self, phase, reason=''):
        changed=self.phase!=phase or self.last_reason!=reason
        self.phase=phase; self.last_reason=reason
        atomic_json(self.status, dict(updated=self.clock.time(), phase=phase, reason=reason,
                    enabled=self.cfg['enabled'], last_result=self.last_result,
                    cycles_last_24h=sum(self.clock.time()-v < 86400 for v in self.ledger.data['cycles']),
                    resets_last_24h=sum(self.clock.time()-v < 86400 for v in self.ledger.data['resets'])), 0o644)
        if changed: logging.info('phase=%s reason=%s',phase,reason)

    def guard(self, maintenance=False):
        s=self.ops.snapshot()
        if not network_guard(s) or s['signature'] != self.signature[1]:
            raise RuntimeError('wired/VPN/maintenance guard changed; aborting escalation')
        if s.get('sms_busy') or s.get('ppp') or s.get('target') or s.get('cell_healthy') or s.get('ppp_running'):
            raise RuntimeError('modem/data became available or SMS busy; aborting escalation')
        if s['usb'] != self.signature[0]: raise RuntimeError('USB re-enumerated; allow normal reconnect')
        if not maintenance and not missing_fault(s): raise RuntimeError('MM state no longer matches fault')
        if not self.ops.confirm_wired(): raise RuntimeError('independent wired/VPN probes failed')
        return s

    def wait(self, seconds):
        end=self.clock.monotonic()+seconds; healthy_since=None
        while self.clock.monotonic() <= end or (healthy_since is not None and self.clock.monotonic() <= healthy_since+self.cfg['verify_seconds']):
            s=self.ops.snapshot()
            if not network_guard(s) or s['signature'] != self.signature[1]:
                raise RuntimeError('wired/VPN/maintenance changed during recovery')
            if recovered(s):
                if healthy_since is None: healthy_since=self.clock.monotonic()
                if self.clock.monotonic()-healthy_since >= self.cfg['verify_seconds']:
                    if self.ops.confirm_wired() and self.ops.verify_data(): return True
                    healthy_since=None
            else: healthy_since=None
            self.clock.sleep(self.cfg['interval_seconds'])
        return False

    def tick(self):
        if not self.cfg['enabled']:
            self.fault_since=None; self.publish('disabled'); return
        s=self.ops.snapshot()
        signature=(s.get('usb'),s.get('signature'))
        if not missing_fault(s):
            self.fault_since=None; self.signature=None
            self.publish('healthy' if recovered(s) else 'paused', '无匹配故障或安全条件未满足'); return
        if self.signature != signature or self.fault_since is None:
            self.signature=signature; self.fault_since=self.clock.monotonic()
        if self.clock.monotonic()-self.fault_since < self.cfg['fault_seconds']:
            self.publish('observing','等待故障与有线健康连续稳定90秒'); return
        reason=self.ledger.allowed('cycle',self.clock.time(),self.cfg)
        if reason: self.publish('cooldown',reason); return
        self.guard()
        self.ledger.reserve('cycle',self.clock.time(),self.cfg)
        success=False
        try:
            self.ops.diagnostics(self.clock.time())
            self.publish('rescanning'); self.guard(); self.ops.scan()
            success=self.wait(self.cfg['scan_wait'])
            if not success:
                self.publish('rediscovering'); self.guard(); self.ops.rediscover()
                success=self.wait(self.cfg['rediscover_wait'])
            if not success:
                self.guard()
                reason=self.ledger.allowed('reset',self.clock.time(),self.cfg)
                if reason: raise RuntimeError(reason)
                self.ledger.reserve('reset',self.clock.time(),self.cfg)
                self.publish('restarting'); result=self.ops.reset(self.guard)
                self.publish('verifying', 'AT重启请求已提交，等待实际数据恢复')
                success=self.wait(self.cfg['reset_wait'])
                if not result.get('reset_command_sent'): success=False
            self.last_result=dict(at=self.clock.time(), success=success)
            self.publish('recovered' if success else 'failed', '' if success else '恢复未通过；等待冷却或现场处理')
        except Exception as e:
            success=False
            self.last_result=dict(at=self.clock.time(),success=False)
            self.publish('failed',str(e))
        finally:
            self.ledger.finish(success,self.clock.time(),self.cfg)
            self.fault_since=None; self.signature=None


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--restore-owned-mm',action='store_true')
    parser.add_argument('--check',action='store_true',help='read-only snapshot; no service/AT actions')
    args=parser.parse_args()
    if os.geteuid()!=0: raise SystemExit('root required')
    logging.basicConfig(level=logging.INFO,format='%(asctime)s %(levelname)s %(message)s')
    if args.restore_owned_mm: restore_owned_mm(); return
    cfg=load_config(); ops=LinuxOps(cfg)
    if args.check:
        print(json.dumps(ops.snapshot(),ensure_ascii=False,default=list)); return
    STATE.mkdir(mode=0o700,parents=True,exist_ok=True); RUNTIME.mkdir(mode=0o755,parents=True,exist_ok=True)
    with (RUNTIME/'daemon.lock').open('w') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        # A corrupt ledger exits before any mutation and remains intact for diagnosis.
        recovery=Recovery(cfg,ops,Ledger(STATE/'ledger.json'))
        def stop(*_): raise SystemExit
        signal.signal(signal.SIGTERM,stop); signal.signal(signal.SIGINT,stop)
        try:
            while True:
                try: recovery.tick()
                except Exception:
                    recovery.fault_since=None; recovery.signature=None
                    logging.exception('Recovery observation failed; no new action')
                time.sleep(cfg['interval_seconds'])
        finally: restore_owned_mm()


if __name__=='__main__': main()
