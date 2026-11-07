#!/usr/bin/env python3
"""Read-only EC20 capability probe; daemon reconnect requires an explicit flag.

No modem reset, USB composition write, audio routing write or call is provided.
Default preflight never opens a serial port or changes a service.
"""
import argparse
import csv
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import select
import signal
import subprocess
import termios
import time

STATE = Path('/run/ec20-uac-query/state.json')
TIMER = 'ec20-uac-query-recovery'
HELPER = '/usr/local/libexec/ec20-check-uac.py'
STOP_TIMEOUT = 35
RECOVERY_DELAY = 90
QUERY_TIMEOUT = 2.5
QUERIES = ('AT', 'AT+CGMR', 'AT+QCFG="usbcfg"', 'AT+QPCMV=?', 'AT+QPCMV?', 'AT+QDAI?')
NETWORK_FILES = ('/usr/local/sbin/ec20-failover', '/etc/ec20-failover.json',
                 '/etc/NetworkManager/system-connections/ec20-cellular.nmconnection')


def run(args, timeout=15):
    return subprocess.run(args, capture_output=True, text=True, timeout=timeout, check=True).stdout.strip()


def parse_usb(text):
    match = re.search(r'^\+QCFG:\s*"usbcfg"\s*,([^\r\n]+)', text, re.I | re.M)
    if not match:
        return {'known': False, 'reason': 'No usbcfg response'}
    fields = next(csv.reader([match.group(1)]))
    try:
        values = [int(f.strip(), 16) if f.strip().lower().startswith('0x') else int(f.strip()) for f in fields]
    except ValueError:
        return {'known': False, 'reason': 'Malformed usbcfg values'}
    if len(values) not in (8, 9) or values[:2] != [0x2c7c, 0x0125] or any(v not in (0, 1) for v in values[2:]):
        return {'known': False, 'reason': 'Unrecognised USB layout; do not generate a write command'}
    flags = dict(zip(('diag', 'nmea', 'at', 'modem', 'rmnet', 'adb', 'uac'), values[2:]))
    return {'known': True, 'vid': '2c7c', 'pid': '0125', 'flags': flags,
            'uac_field_exposed': 'uac' in flags, 'uac_enabled': flags.get('uac') == 1}


def parse_modes(text):
    match = re.search(r'^\+QPCMV:\s*\([^)]*\)\s*,\s*\(([^)]*)\)', text, re.M)
    if not match:
        return {'known': False}
    modes = set()
    for part in match.group(1).split(','):
        part = part.strip()
        if re.fullmatch(r'[0-2]', part):
            modes.add(int(part))
        elif re.fullmatch(r'[0-2]-[0-2]', part) and part[0] <= part[-1]:
            modes.update(range(int(part[0]), int(part[-1]) + 1))
        else:
            return {'known': False}
    return {'known': True, 'modes': sorted(modes), 'uac_route_supported': 2 in modes}


def wired_guard(status, now=None):
    now = now or datetime.datetime.now(datetime.timezone.utc)
    updated = datetime.datetime.fromisoformat(status['updated'])
    if updated.tzinfo is None:
        raise RuntimeError('Status timestamp lacks timezone')
    age = (now - updated).total_seconds()
    if not 0 <= age <= 15 or status.get('active') != 'wired' or not status.get('wired_healthy') or not status.get('wired_link'):
        raise RuntimeError('Fresh healthy wired-primary status required')
    if not all(status.get('vpns', {}).get(n) is True for n in ('tun0', 'tun1', 'tun2')):
        raise RuntimeError('All three VPNs must be healthy')


def failover():
    return json.loads(Path('/run/ec20-failover/status.json').read_text())


def owners(port):
    found = []
    target = str(Path(port).resolve())
    for process in Path('/proc').glob('[0-9]*'):
        try:
            if any(str(fd.resolve()) == target for fd in (process / 'fd').iterdir()):
                found.append({'pid': int(process.name), 'process': (process / 'comm').read_text().strip()})
        except (OSError, PermissionError):
            continue
    return found


def validate_port(port):
    path = Path(port).resolve(strict=True)
    sys_path = (Path('/sys/class/tty') / path.name / 'device').resolve(strict=True)
    interface = next((p for p in (sys_path, *sys_path.parents) if (p / 'bInterfaceNumber').exists()), None)
    device = next((p for p in sys_path.parents if (p / 'idVendor').exists()), None)
    if interface is None or device is None or (interface / 'bInterfaceNumber').read_text().strip() != '03':
        raise RuntimeError('Only EC20 secondary interface 03 may be queried')
    if ((device / 'idVendor').read_text().strip(), (device / 'idProduct').read_text().strip()) != ('2c7c', '0125'):
        raise RuntimeError('Unexpected USB device')
    return str(path), str(device)


class ATReader:
    def __init__(self, port):
        self.port = port
        self.fd = None

    def __enter__(self):
        if owners(self.port):
            raise RuntimeError('Serial port is owned; refusing to open it')
        self.fd = os.open(self.port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        try:
            fcntl.ioctl(self.fd, termios.TIOCEXCL)
            self.saved = termios.tcgetattr(self.fd)
            attrs = termios.tcgetattr(self.fd)
            attrs[0] = attrs[1] = attrs[3] = 0
            attrs[2] = termios.CLOCAL | termios.CREAD | termios.CS8
            attrs[4] = attrs[5] = termios.B115200
            attrs[6][termios.VMIN] = attrs[6][termios.VTIME] = 0
            termios.tcsetattr(self.fd, termios.TCSANOW, attrs)
        except BaseException:
            os.close(self.fd)
            self.fd = None
            raise
        return self

    def query(self, command):
        if command not in QUERIES:
            raise ValueError('Only fixed read-only queries are permitted')
        wired_guard(failover())
        termios.tcflush(self.fd, termios.TCIFLUSH)
        os.write(self.fd, (command + '\r').encode('ascii'))
        data = b''
        end = time.monotonic() + QUERY_TIMEOUT
        while time.monotonic() < end:
            if not select.select([self.fd], [], [], .15)[0]:
                continue
            data += os.read(self.fd, 4096)
            if len(data) > 16384:
                raise RuntimeError('Oversized AT response')
            text = data.decode('ascii', errors='replace').replace('\r', '')
            if re.search(r'^(OK|ERROR|\+CME ERROR:.*)$', text, re.M):
                return text.strip()
        raise TimeoutError('AT query timed out: ' + command)

    def __exit__(self, *args):
        if self.fd is not None:
            try:
                termios.tcsetattr(self.fd, termios.TCSANOW, self.saved)
                fcntl.ioctl(self.fd, termios.TIOCNXCL)
            finally:
                os.close(self.fd)


def recover():
    # Runtime-only mask prevents D-Bus clients from reopening the AT port during
    # the bounded probe. A separate systemd timer runs this after process loss.
    if not STATE.exists():
        return
    state = json.loads(STATE.read_text())
    if not state.get('maintenance_owned'):
        raise RuntimeError('No owned maintenance state')
    run(['systemctl', 'unmask', '--runtime', 'ModemManager.service'])
    run(['systemctl', 'start', 'ModemManager.service'], timeout=25)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', default='/dev/serial/by-id/usb-Android_Android_0123456789ABCDEF-if03-port0')
    parser.add_argument('--allow-mm-reconnect', action='store_true', help='Explicitly allow a temporary loss of cellular standby; never reset the module')
    parser.add_argument('--recover', action='store_true', help=argparse.SUPPRESS)
    parser.add_argument('--report', default='/var/lib/ec20-uac-check/query.json')
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise SystemExit('Root is required for process ownership checks')
    if args.recover:
        recover()
        return
    lock = open('/run/lock/ec20-uac-check.lock', 'w')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    status = failover()
    wired_guard(status)
    port, usb_path = validate_port(args.port)
    report = {'mode': 'preflight', 'port': port, 'usb_path': usb_path, 'owners': owners(port),
              'module_reset': False, 'USB_config_written': False, 'audio_route_written': False,
              'failover_before': status}
    if not args.allow_mm_reconnect:
        report['next_step'] = 'Need approved short ModemManager outage to release the secondary AT port'
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return
    if not status.get('cellular_healthy') or not status.get('cellular_ip'):
        raise RuntimeError('Cellular standby is not currently healthy')
    if not Path(HELPER).is_file() or Path(HELPER).stat().st_uid != 0 or Path(HELPER).stat().st_mode & 0o022:
        raise RuntimeError('Install this reviewed helper root-owned at ' + HELPER)
    if STATE.exists():
        raise RuntimeError('Unresolved previous maintenance state')
    if run(['systemctl', 'is-active', 'ModemManager']) != 'active' or 'masked' in run(['systemctl', 'is-enabled', 'ModemManager']):
        raise RuntimeError('ModemManager must be active and unmasked before maintenance')
    modems = json.loads(run(['mmcli', '-L', '-J']))['modem-list']
    if len(modems) != 1:
        raise RuntimeError('Expected exactly one modem')
    for call in json.loads(run(['mmcli', '-m', modems[0], '--voice-list-calls', '-J'])).get('modem.voice.call', []):
        if json.loads(run(['mmcli', '-o', call, '-J']))['call']['properties']['state'] != 'terminated':
            raise RuntimeError('Active call exists; refusing maintenance')
    hashes = {p: hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in NETWORK_FILES}
    STATE.parent.mkdir(mode=0o700, exist_ok=True)
    STATE.write_text(json.dumps({'maintenance_owned': True, 'created': time.time()}))
    STATE.chmod(0o600)
    query_error = None
    restore_started = None
    try:
        run(['systemd-run', '--unit=' + TIMER, '--on-active=' + str(RECOVERY_DELAY) + 's', '--timer-property=AccuracySec=1s',
             '/usr/bin/python3', HELPER, '--recover'])
        run(['systemctl', 'mask', '--runtime', '--now', 'ModemManager.service'], timeout=STOP_TIMEOUT)
        if owners(port):
            raise RuntimeError('Secondary port is still owned after stopping ModemManager')
        responses = {}
        with ATReader(port) as at:
            for command in QUERIES:
                responses[command] = at.query(command)
        report.update(mode='read-only-AT-query', responses=responses,
                      usbcfg=parse_usb(responses['AT+QCFG="usbcfg"']), qpcmv_modes=parse_modes(responses['AT+QPCMV=?']))
    except Exception as exc:
        query_error = str(exc)
        report['query_error'] = query_error
    finally:
        restore_started = time.time()
        recover()
    deadline = time.monotonic() + 100
    restored = False
    while time.monotonic() < deadline:
        current = failover()
        wired_guard(current)
        if current.get('cellular_healthy') and current.get('cellular_ip') and current.get('cellular_connection_state') == 'connected':
            # Reject a stale pre-outage PPP status while the modem reconnects.
            stamp = datetime.datetime.fromisoformat(current['updated']).timestamp()
            # Wait past the controller's 20s cellular probe interval, so its
            # health flag cannot merely be a cached pre-maintenance success.
            if stamp > restore_started + 22 and Path('/sys/class/net/ppp0').exists():
                try:
                    discovered = json.loads(run(['mmcli', '-L', '-J']))['modem-list']
                    connected = len(discovered) == 1 and json.loads(run(['mmcli', '-m', discovered[0], '-J']))['modem']['generic']['state'] == 'connected'
                except (subprocess.SubprocessError, KeyError, ValueError):
                    connected = False
                if connected:
                    restored = True
                    break
        time.sleep(1)
    report['failover_after'] = failover()
    report['cellular_restored'] = restored
    report['network_hashes_unchanged'] = hashes == {p: hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in NETWORK_FILES}
    destination = Path(args.report)
    destination.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    destination.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    destination.chmod(0o600)
    if restored:
        run(['systemctl', 'stop', TIMER + '.timer'])
        STATE.unlink()
    print(json.dumps(report, ensure_ascii=False, indent=2))
    if not restored or query_error or not report['network_hashes_unchanged']:
        raise SystemExit('Probe or recovery incomplete; inspect report before further changes')


if __name__ == '__main__':
    main()
