import json
from pathlib import Path
import tempfile
import unittest
from render import render
from test_host_profile import profile


class DeploymentRenderTests(unittest.TestCase):
    def test_default_stages_no_privileged_hardware_maintenance(self):
        with tempfile.TemporaryDirectory() as directory:
            out=Path(directory)/'result';plan=render(profile(),'192.0.2.10',out)
            unit=(out/'etc/systemd/system/ec20-console.service').read_text()
            self.assertNotIn('-ec20-voice',unit);self.assertNotIn('-gnss-control',unit)
            self.assertIn('-tls-cert %d/tls.crt',unit);self.assertIn('-data-dir /var/lib/ec20-console',unit)
            self.assertNotIn('@PUBLIC_HOST@',unit)
            self.assertFalse((out/'usr/local/libexec/ec20-modem-recovery.py').exists())
            self.assertFalse(plan['network_services_changed'])

    def test_opt_in_keeps_recovery_disabled_and_pins_sound_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            out=Path(directory)/'result';render(profile(),'192.0.2.10',out,gnss=True,call_audio=True,recovery=True)
            self.assertIn('KERNELS=="2-3.4"',(out/'etc/udev/rules.d/99-ec20-call-audio.rules').read_text())
            self.assertFalse(json.loads((out/'etc/ec20-modem-recovery.json').read_text())['enabled'])
            self.assertIn('-ec20-voice',(out/'etc/systemd/system/ec20-console.service').read_text())
            self.assertTrue((out/'usr/local/libexec/host_profile.py').exists())

    def test_bad_identity_or_systemd_injection_creates_no_output(self):
        with tempfile.TemporaryDirectory() as directory:
            out=Path(directory)/'result'
            for p,bind in [({**profile(),'device_id':'REPLACE_WITH_LOCAL_DEVICE_IDENTIFIER'},'127.0.0.1'),(profile(),'127.0.0.1 -evil'),(profile(),'%h'),(profile(),'0.0.0.0')]:
                with self.assertRaises(ValueError):render(p,bind,out)
                self.assertFalse(out.exists())
