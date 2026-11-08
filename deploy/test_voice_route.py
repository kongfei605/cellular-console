import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from test_host_profile import profile
s=importlib.util.spec_from_file_location('voice_route',Path(__file__).with_name('ec20-voice-route.py'))
r=importlib.util.module_from_spec(s);s.loader.exec_module(r)
class VoiceRouteTests(unittest.TestCase):
 def test_readback_requires_usb_voice(self):
  for text in ['+QPCMV: 1,2\nOK','+QPCMV: 1, 2\nOK','+QPCMV: 1\nOK']:self.assertTrue(r.route_on(text))
  for text in ['+QPCMV: 0,0','+QPCMV: 1,0','+QPCMV: 1,1','ERROR','+QPCMV: 10,2']:self.assertFalse(r.route_on(text))
 def test_runtime_cannot_reset_change_usb_or_dial(self):
  at=r.RouteAT('/not-open',lambda:None)
  for text in ['AT+CFUN=1,1','AT+QCFG="usbcfg",1','ATD+12025550123;','ATH']:
   with self.assertRaises(ValueError):at.exchange(text)
 def test_probe_uses_configured_usb_topology(self):
  with patch.object(r,'host',return_value=profile()),patch.object(r,'cmd',return_value=json.dumps({'device':{'card':2}})) as cmd:
   self.assertEqual(r.probe(),{'card':2});cmd.assert_called_once_with('/usr/local/libexec/ec20-uac-ready','-usb-path','2-3.4',timeout=5)
 def test_recovery_releases_only_its_owned_inhibit(self):
  with tempfile.TemporaryDirectory() as directory,patch.object(r,'RUN',Path(directory)),patch.object(r.subprocess,'run') as command:
   owner=Path(directory)/'owner.json';owner.write_text(json.dumps({'owned':False,'unit':r.HOLD}));r.restore();command.assert_not_called()
   owner.write_text(json.dumps({'owned':True,'unit':r.HOLD}));r.restore()
   self.assertFalse(owner.exists());self.assertEqual(command.call_args.args[0],['systemctl','stop',r.HOLD])
