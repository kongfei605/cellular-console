#!/usr/bin/env python3
"""Create new local administrator credentials without overwriting an account."""
import argparse
import json
import os
from pathlib import Path
import secrets


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--output',required=True);args=parser.parse_args()
    destination=Path(args.output);destination.parent.mkdir(parents=True,mode=0o700,exist_ok=True)
    fd=os.open(destination,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f:json.dump({'username':'admin','password':secrets.token_urlsafe(32)},f);f.write('\n')
    print('Created private credentials: '+str(destination))
