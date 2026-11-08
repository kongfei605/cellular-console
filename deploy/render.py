#!/usr/bin/env python3
"""Render a reviewable Linux deployment. Never installs, restarts or dials."""
import argparse
import ipaddress
import json
from pathlib import Path
import re
import shutil
from host_profile import validate

SOURCE = Path(__file__).resolve().parent


def render(profile, public_host, output, *, gnss=False, call_audio=False, recovery=False):
    validate(profile)
    # Literal IPs prevent systemd specifier/argument injection in rendered units.
    address=ipaddress.IPv4Address(public_host)
    if address.is_unspecified or address.is_multicast or public_host=='255.255.255.255':
        raise ValueError('literal unicast or loopback host address required')
    output=Path(output)
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    substitutions={'PUBLIC_HOST':public_host, 'USB_PATH':profile['usb_path'],
                   'CONSOLE_FEATURES':' '.join(flag for enabled,flag in [(gnss,'-gnss-control'),(call_audio,'-ec20-voice')] if enabled),
                   'AUDIO_FEATURES':'-ec20-voice' if call_audio else ''}
    def copy(name, destination):
        target=output/destination;target.parent.mkdir(parents=True,exist_ok=True)
        content=(SOURCE/name).read_text()
        for key,value in substitutions.items():content=content.replace('@'+key+'@',value)
        if re.search(r'@[A-Z_]+@',content):raise ValueError('unresolved unit template')
        target.write_text(content);target.chmod(0o755 if target.suffix=='.py' else 0o644)
    for name in ['ec20-console.service','ec20-audio-loopback.service']:
        copy(name,Path('etc/systemd/system')/name)
    copy('49-ec20-console.rules','etc/polkit-1/rules.d/49-ec20-console.rules')
    if gnss:
        for name in ['ec20-gnss-broker.service','ec20-gnss-broker.socket']:copy(name,Path('etc/systemd/system')/name)
    if call_audio or recovery:
        copy('host_profile.py','usr/local/libexec/host_profile.py')
    if call_audio:
        for name in ['ec20-voice-route.service','ec20-voice-route.timer']:copy(name,Path('etc/systemd/system')/name)
        copy('ec20-voice-route.py','usr/local/libexec/ec20-voice-route.py')
        copy('99-ec20-call-audio.rules','etc/udev/rules.d/99-ec20-call-audio.rules')
        copy('ec20-call-audio.override.conf','etc/systemd/system/ec20-audio-loopback.service.d/40-uac.conf')
        (output/'etc/ec20-call-audio.enabled').write_text('enabled\n')
    if recovery:
        copy('ec20-modem-recovery.service','etc/systemd/system/ec20-modem-recovery.service')
        copy('ec20-modem-recovery.py','usr/local/libexec/ec20-modem-recovery.py')
        copy('ec20-modem-recovery.json','etc/ec20-modem-recovery.json')
    destination=output/'etc/ec20-toolbox/host.json';destination.parent.mkdir(exist_ok=True)
    destination.write_text(json.dumps(profile,indent=2)+'\n');destination.chmod(0o644)
    plan=dict(render_only=True,network_services_changed=False,gnss=gnss,call_audio=call_audio,recovery_files=recovery,
              recovery_enabled=False,public_origin='https://'+public_host+':8788',instructions='docs/deployment.md')
    (output/'plan.json').write_text(json.dumps(plan,indent=2)+'\n')
    return plan


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profile',required=True);parser.add_argument('--public-host',default='127.0.0.1')
    parser.add_argument('--output',required=True)
    parser.add_argument('--enable-gnss',action='store_true');parser.add_argument('--enable-call-audio',action='store_true')
    parser.add_argument('--include-recovery',action='store_true',help='stage recovery files; enabled remains false')
    a=parser.parse_args()
    print(json.dumps(render(json.loads(Path(a.profile).read_text()),a.public_host,a.output,
                            gnss=a.enable_gnss,call_audio=a.enable_call_audio,recovery=a.include_recovery),indent=2))
