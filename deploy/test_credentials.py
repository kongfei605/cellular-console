import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class CredentialInitializationTests(unittest.TestCase):
    def test_existing_account_is_never_overwritten(self):
        helper=Path(__file__).resolve().parents[1]/'scripts/init_credentials.py'
        with tempfile.TemporaryDirectory() as directory:
            output=Path(directory)/'credentials.json'
            command=[sys.executable,str(helper),'--output',str(output)]
            subprocess.run(command,check=True,stdout=subprocess.DEVNULL)
            original=output.read_bytes();value=json.loads(original)
            self.assertEqual(value['username'],'admin');self.assertGreaterEqual(len(value['password']),32)
            self.assertEqual(output.stat().st_mode & 0o777,0o600)
            result=subprocess.run(command,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            self.assertNotEqual(result.returncode,0);self.assertEqual(output.read_bytes(),original)
