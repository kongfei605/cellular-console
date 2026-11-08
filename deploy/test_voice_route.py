import importlib.util,unittest
from pathlib import Path
def load(name,file):
 s=importlib.util.spec_from_file_location(name,Path(__file__).with_name(file));m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
r=load('voice_route','ec20-voice-route.py');u=load('uac_enable','enable-uac.py')
class VoiceRouteTests(unittest.TestCase):
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
if __name__=='__main__':unittest.main()
