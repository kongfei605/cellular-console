#!/usr/bin/env python3
"""Generate notices from exactly the modules pinned by go.mod/go.sum."""
import argparse
import json
import os
from pathlib import Path
import subprocess

ROOT=Path(__file__).resolve().parents[1]


def generate(go=None):
    go=go or os.environ.get('GO','go')
    modules={};decoder=json.JSONDecoder()
    # Only linked packages belong in distributed binary notices. Walking `all`
    # modules would also fetch upstream projects' unused test-only dependencies.
    for target in [('linux','amd64'),('darwin','arm64')]:
        environment={**os.environ,'GOOS':target[0],'GOARCH':target[1]}
        stream=subprocess.check_output([go,'list','-deps','-json','./...'],cwd=ROOT,text=True,env=environment)
        while stream.strip():
            value,at=decoder.raw_decode(stream.lstrip());stream=stream.lstrip()[at:]
            module=value.get('Module')
            if module:modules[module['Path']]=module
    goroot=Path(subprocess.check_output([go,'env','GOROOT'],text=True).strip())
    sections=['Third-party notices\n===================\n\nGenerated from pinned Go modules; regenerate with python3 scripts/update_notices.py.\nThe project MIT license does not replace these upstream licenses.\n',
              'Go standard library and runtime\n'+(goroot/'LICENSE').read_text()]
    for module in sorted(modules.values(),key=lambda item:item['Path']):
        if module.get('Main'):continue
        if module.get('Replace'):raise RuntimeError('review replaced module licenses before distribution')
        directory=module.get('Dir')
        if not directory:raise RuntimeError('run go mod download: '+module['Path'])
        files=[p for name in ['LICENSE','LICENSE.txt','LICENSE.md','COPYING','NOTICE'] if (p:=Path(directory)/name).is_file()]
        if not files:raise RuntimeError('license missing: '+module['Path'])
        sections.append(module['Path']+' '+module['Version']+'\n'+'\n'.join(p.name+'\n'+p.read_text() for p in files))
    sections.append('External runtime programs\n-------------------------\nALSA arecord/aplay, ModemManager, NetworkManager, systemd, curl and ping are system dependencies; their binaries are not bundled.\nGitleaks is a separate release/CI tool, not linked into application binaries.\nThe FFmpeg-generated G.711 integer reference vectors are synthetic test data, not recordings; FFmpeg binaries/source are not bundled.\n')
    text=('\n'+'='*72+'\n').join(sections)
    return '\n'.join(line.rstrip() for line in text.splitlines())+'\n'


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--check',action='store_true');a=p.parse_args()
    content=generate();destination=ROOT/'THIRD_PARTY_NOTICES.txt'
    if a.check:
        if destination.read_text()!=content:raise SystemExit('third-party notices are stale; regenerate')
    else:destination.write_text(content)
