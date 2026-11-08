import importlib.util,unittest,json
from unittest.mock import patch
from types import SimpleNamespace
from pathlib import Path
def load(name,file):
 s=importlib.util.spec_from_file_location(name,Path(__file__).with_name(file));m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
r=load('voice_route','ec20-voice-route.py');u=load('uac_enable','enable-uac.py')
class VoiceRouteTests(unittest.TestCase):
 def test_console_override_preserves_credentials_and_persistent_data(self):
  unit=Path(__file__).with_name('ec20-console-voice.override.conf').read_text()
  for arg in ['-credentials /etc/ec20-console/credentials.json','-data-dir /var/lib/ec20-console','-tls-cert %d/tls.crt','-tls-key %d/tls.key','-audio-gateway-key %d/audio.key']:
   self.assertIn(arg,unit)
  self.assertNotIn('-tls-cert /etc/',unit)
 def test_readback_requires_usb_voice(self):
  for text in ['+QPCMV: 1,2\nOK','+QPCMV: 1, 2\nOK','+QPCMV: 1\nOK']:self.assertTrue(r.route_on(text))
  for text in ['+QPCMV: 0,0','+QPCMV: 1,0','+QPCMV: 1,1','ERROR','+QPCMV: 10,2']:self.assertFalse(r.route_on(text))
 def test_runtime_cannot_reset_or_change_usb(self):
  at=r.RouteAT('/not-open',lambda:None)
  for text in ['AT+CFUN=1,1','AT+QCFG="usbcfg",'+u.NEW,'ATD+12025550123;','ATH']:
   with self.assertRaises(ValueError):at.exchange(text)
 def test_migration_only_changes_one_verified_function_bit(self):
  for tuple in [u.OLD,u.NEW]:self.assertEqual(u.tuple_from('+QCFG: "usbcfg",'+tuple+'\nOK'),tuple)
  for tuple in ['0x2C7C,0x0125,1,1,1,1,1,1,1','0x2C7C,0x9999,1,1,1,1,1,0,1','0x2C7C,0x0125,1,0,1,1,1,0,1','0x2C7C,0x0125,1,1,1,1,1,0,2']:
   with self.assertRaises((AssertionError,ValueError)):u.tuple_from('+QCFG: "usbcfg",'+tuple+'\nOK')
 def test_migration_rejects_arbitrary_control(self):
  at=u.AT()
  for text in ['AT+QDAI=1','AT+QCFG="usbnet",1','AT+QPOWD','ATD+12025550123;']:
   with self.assertRaises(AssertionError):at.query(text)
 def test_short_first_enumeration_is_not_treated_as_ready(self):
  clock=[0]
  def sleep(seconds):clock[0]+=seconds
  def probe(*args,**kwargs):
   if 6<=clock[0]<14:return SimpleNamespace(returncode=2,stdout='')
   fp='temporary' if clock[0]<6 else 'final'
   return SimpleNamespace(returncode=0,stdout=json.dumps({'device':{'fingerprint':fp}}))
  with patch.object(u,'wired'),patch.object(u.time,'monotonic',side_effect=lambda:clock[0]),patch.object(u.time,'sleep',side_effect=sleep),patch.object(u.subprocess,'run',side_effect=probe),patch.object(u,'Path',return_value=SimpleNamespace(exists=lambda:True)):
   device=u.wait_usb()
  self.assertEqual(device['fingerprint'],'final');self.assertGreaterEqual(clock[0],26)
if __name__=='__main__':unittest.main()
