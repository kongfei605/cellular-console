import datetime
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('uac_check', Path(__file__).with_name('check-uac.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class UACChecks(unittest.TestCase):
    def test_usb_audio_flag_and_existing_functions(self):
        parsed = probe.parse_usb('+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK')
        self.assertTrue(parsed['known'])
        self.assertTrue(parsed['uac_field_exposed'])
        self.assertFalse(parsed['uac_enabled'])
        self.assertEqual(parsed['flags']['modem'], 1)
        enabled = probe.parse_usb('+QCFG: "USBCFG",0x2C7C,0x0125,1,1,1,1,1,0,1\nOK')
        self.assertTrue(enabled['uac_enabled'])

    def test_legacy_no_audio_field_is_not_assumed_supported(self):
        parsed = probe.parse_usb('+QCFG: "usbcfg",0x2c7c,0x0125,1,1,1,1,1,0\nOK')
        self.assertTrue(parsed['known'])
        self.assertFalse(parsed['uac_field_exposed'])
        self.assertFalse(parsed['uac_enabled'])

    def test_bad_or_unrecognised_usb_layout_is_rejected(self):
        for text in ['ERROR', '+QCFG: "usbcfg",0x2c7c,0x9999,1,1,1,1,1,0,1',
                     '+QCFG: "usbcfg",0x2c7c,0x0125,1,1,1,1,1,0,2',
                     '+QCFG: "usbcfg",0x2c7c,0x0125,x,1,1,1,1,0,1',
                     '+QCFG: "other",0x2c7c,0x0125,1,1,1,1,1,0,1']:
            self.assertFalse(probe.parse_usb(text)['known'])

    def test_supported_modes_distinguish_range_and_list(self):
        self.assertEqual(probe.parse_modes('+QPCMV: (0,1),(0,2)\nOK')['modes'], [0, 2])
        self.assertEqual(probe.parse_modes('+QPCMV: (0,1),(0-2)\nOK')['modes'], [0, 1, 2])
        self.assertFalse(probe.parse_modes('+QPCMV: (0,1),(0,1)\nOK')['uac_route_supported'])
        self.assertFalse(probe.parse_modes('ERROR')['known'])

    def test_wired_guard_rejects_stale_bad_and_cellular_active(self):
        now = datetime.datetime.now(datetime.timezone.utc)
        good = dict(updated=now.isoformat(), active='wired', wired_healthy=True, wired_link=True,
                    vpns=dict(tun0=True, tun1=True, tun2=True))
        probe.wired_guard(good, now)
        bad = [dict(good, updated=(now-datetime.timedelta(seconds=20)).isoformat()),
               dict(good, updated=(now+datetime.timedelta(seconds=1)).isoformat()),
               dict(good, active='cellular'), dict(good, wired_healthy=False),
               dict(good, vpns=dict(tun0=False,tun1=True,tun2=True))]
        for state in bad:
            with self.assertRaises(RuntimeError):
                probe.wired_guard(state, now)

    def test_serial_writer_cannot_accept_configuration_or_reset(self):
        with patch.object(probe, 'failover', side_effect=AssertionError('must reject before any I/O')):
            for command in ['AT+CFUN=1,1', 'AT+QPCMV=1,2', 'AT+QCFG="usbcfg",1', 'ATH', 'ATD123;']:
                with self.assertRaises(ValueError):
                    probe.ATReader('/dev/never-opened').query(command)

    def test_owned_port_is_never_opened(self):
        with patch.object(probe, 'owners', return_value=[{'pid':123,'process':'ModemManager'}]), patch.object(probe.os, 'open') as opened:
            with self.assertRaises(RuntimeError):
                with probe.ATReader('/dev/never-opened'):
                    pass
            opened.assert_not_called()

    def test_recovery_only_unmasks_runtime_and_starts_manager(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)/'state.json'
            state.write_text('{"maintenance_owned": true}')
            with patch.object(probe,'STATE',state),patch.object(probe,'run') as run:
                probe.recover()
                self.assertEqual([c.args[0] for c in run.call_args_list], [
                    ['systemctl','unmask','--runtime','ModemManager.service'],
                    ['systemctl','start','ModemManager.service']])
            state.write_text('{"maintenance_owned": false}')
            with patch.object(probe,'STATE',state),patch.object(probe,'run') as run:
                with self.assertRaises(RuntimeError):
                    probe.recover()
                run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
