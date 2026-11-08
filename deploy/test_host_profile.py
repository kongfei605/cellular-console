import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace
from host_profile import validate, read_root_json


def profile():
    return dict(schema_version=1, profile='ec20f-r08a03-ppp', device_id='test-device-identifier', usb_path='2-3.4', wired_interface='eth9', cell_interface='ppp7', vpn_interfaces=['wg0', 'tun7'])


class HostProfileTests(unittest.TestCase):
    def test_local_identity_does_not_whitelist_new_firmware(self):
        self.assertEqual(validate(profile()), profile())
        for key, value in [('profile', 'arbitrary-modem'), ('device_id', ''), ('device_id', 'REPLACE_WITH_LOCAL_DEVICE_IDENTIFIER'), ('device_id', '0'*40), ('usb_path', '../dev'), ('vpn_interfaces', []), ('vpn_interfaces', ['wg0', 'wg0']), ('cell_interface', 'eth9'), ('schema_version', True)]:
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                validate({**profile(), key:value})

    def test_unknown_fields_are_rejected(self):
        with self.assertRaises(ValueError):validate({**profile(), 'ignore_network_guard':True})

    def test_untrusted_and_symlink_profiles_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            p=Path(directory)/'profile.json';p.write_text(json.dumps(profile()));p.chmod(0o666)
            with self.assertRaises(ValueError):read_root_json(p)
            link=p.with_name('link');link.symlink_to(p)
            with self.assertRaises(OSError):read_root_json(link)
