#!/usr/bin/env python3
"""Privacy and packaging checks for tracked files and reachable Git history.

This complements Gitleaks; it is not a complete security audit.
Only rule names and paths are printed, never matching values.
"""
import argparse
import json
from pathlib import Path
import re
import subprocess

ROOT=Path(__file__).resolve().parents[1]
RULES={
    'private-key':re.compile(rb'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----'),
    'provider-token':re.compile(rb'(?:AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-(?:proj-)?[A-Za-z0-9_-]{30,})'),
    'personal-home-path':re.compile(rb'/(?:Users|home)/[A-Za-z0-9_-]+/'),
    'personal-email':re.compile(rb'[A-Za-z0-9_.+-]+@(?:gmail|qq|163)\.com'),
}


def findings(data):
    found=[name for name,pattern in RULES.items() if pattern.search(data)]
    # Existing all-zero demonstration numbers exercise +86 normalization.
    # Every other mobile-looking number needs to be replaced by reserved data.
    for match in re.finditer(rb'(?<!\d)(?:\+?86)?(1[3-9]\d{9})(?!\d)',data):
        if match[1] not in (b'13800000000',b'13900000000'):
            found.append('non-synthetic-phone');break
    return found


def check(history=False):
    failures=[];count=0
    files=subprocess.check_output(['git','ls-files','-z'],cwd=ROOT).decode().split('\0')
    for name in filter(None,files):
        path=ROOT/name
        if not path.is_file():continue
        count+=1
        failures.extend({'rule':rule,'path':name} for rule in findings(path.read_bytes()))
        if re.search(r'(^|/)(credentials\.json|host\.json|inbox\.json|outbox\.json|workspace\.json)$|\.(pem|key|p12|pfx|bundle|zip)$',name):
            failures.append({'rule':'private-runtime-file','path':name})
    objects=0
    if history:
        rows=subprocess.check_output(['git','rev-list','--objects','--all'],cwd=ROOT,text=True).splitlines()
        names={row.split(' ',1)[0]:row.split(' ',1)[1] if ' ' in row else '(commit metadata)' for row in rows}
        process=subprocess.Popen(['git','cat-file','--batch'],cwd=ROOT,stdin=subprocess.PIPE,stdout=subprocess.PIPE)
        raw,_=process.communicate(('\n'.join(names)+'\n').encode());offset=0
        if process.returncode:raise RuntimeError('Git object inspection failed')
        for oid,name in names.items():
            end=raw.index(b'\n',offset);header=raw[offset:end].split();size=int(header[2]);data=raw[end+1:end+1+size];offset=end+size+2
            if header[1] not in (b'blob',b'commit',b'tag'):continue
            objects+=1
            failures.extend({'rule':rule,'path':'history:'+name} for rule in findings(data))
    # Deduplicate without exposing any credential or telephone number.
    unique={json.dumps(item,sort_keys=True) for item in failures}
    return {'tracked_files':count,'history_objects':objects,'passed':not unique,'findings':[json.loads(item) for item in sorted(unique)]}


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--history',action='store_true');a=p.parse_args()
    report=check(a.history);print(json.dumps(report,indent=2));raise SystemExit(0 if report['passed'] else 1)
