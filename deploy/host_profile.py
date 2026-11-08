"""Shared validation for Linux helpers; config cannot whitelist new firmware."""
import json
import os
from pathlib import Path
import re
import stat

DEFAULT_PATH = '/etc/ec20-toolbox/host.json'
PROFILE = 'ec20f-r08a03-ppp'
FIRMWARE = 'EC20CEFAGR08A03M4G'
FIELDS = {'schema_version', 'profile', 'device_id', 'usb_path', 'wired_interface', 'cell_interface', 'vpn_interfaces'}


def validate(value):
    if not isinstance(value, dict) or set(value) != FIELDS or type(value['schema_version']) is not int or value['schema_version'] != 1 or value['profile'] != PROFILE:
        raise ValueError('unsupported hardware profile or unknown fields')
    identity = value['device_id']
    if not isinstance(identity, str) or not re.fullmatch(r'[A-Za-z0-9_.:-]{8,128}', identity) or 'REPLACE' in identity.upper() or identity.lower().startswith('example') or not identity.strip('0'):
        raise ValueError('explicit local ModemManager device_id required')
    if not isinstance(value['usb_path'], str) or not re.fullmatch(r'[0-9]+-[0-9]+(?:\.[0-9]+)*', value['usb_path']):
        raise ValueError('invalid pinned USB topology')
    vpns = value['vpn_interfaces']
    if not isinstance(vpns, list) or not 1 <= len(vpns) <= 8:
        raise ValueError('one to eight monitored VPN interfaces required')
    names = [value['wired_interface'], value['cell_interface'], *vpns]
    if any(not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9_.:-]{1,15}', name) for name in names) or len(set(names)) != len(names):
        raise ValueError('invalid or duplicate interfaces')
    return value


def read_root_json(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd) as source:
        st = os.fstat(source.fileno())
        if st.st_uid != 0 or not stat.S_ISREG(st.st_mode) or st.st_mode & 0o022:
            raise ValueError('configuration must be root-owned, regular and not group/world writable')
        raw = source.read(8193)
        if len(raw) > 8192:
            raise ValueError('configuration too large')
        return json.loads(raw)


def load(path=None):
    return validate(read_root_json(path or DEFAULT_PATH))
