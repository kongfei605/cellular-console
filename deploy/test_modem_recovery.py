import copy
import datetime
import importlib.util
import json
import os
import pty
from pathlib import Path
import tempfile
import threading
import select
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from test_host_profile import profile

spec=importlib.util.spec_from_file_location('recovery',Path(__file__).with_name('ec20-modem-recovery.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)


class Clock:
    def __init__(self):self.value=2000000000
    def time(self):return self.value
    def monotonic(self):return self.value
    def sleep(self,seconds):self.value+=seconds


class RecoveryConfigurationTests(unittest.TestCase):
    def test_identity_is_from_the_reviewed_host_profile_not_recovery_options(self):
        with patch.object(m,'read_root_json',return_value={'enabled':False}),patch.object(m,'load_host',return_value=profile()):
            cfg=m.load_config();self.assertEqual(cfg['device_id'],profile()['device_id']);self.assertEqual(cfg['usb_path'],'2-3.4')
        with patch.object(m,'read_root_json',return_value={'device_id':'other-device'}),patch.object(m,'load_host',return_value=profile()):
            with self.assertRaises(ValueError):m.load_config()

    def test_enabled_recovery_requires_an_explicit_https_target(self):
        for target in ['', 'file:///etc/passwd', 'http://example.com/', 'https://user:secret@example.com/']:
            with patch.object(m,'read_root_json',return_value={'enabled':True,'verify_url':target}),patch.object(m,'load_host',return_value=profile()):
                with self.assertRaises(ValueError):m.load_config()
        with patch.object(m,'read_root_json',return_value={'enabled':True,'verify_url':'https://example.com/'}),patch.object(m,'load_host',return_value=profile()):
            self.assertTrue(m.load_config()['enabled'])


def fault():
    return dict(fresh=True,wired=True,vpn=True,usb='usb:42',signature=('wired-ip','nm-pid','failover-pid'),
                paused=False,other_modems=False,mm_known=True,mm_active=True,mm_masked=False,target=False,
                ppp=False,ppp_running=False,connected=False,cell_healthy=False,sms_busy=False,ip_matches=False)


class Ops:
    def __init__(self,clock,heals='rediscover'):
        self.clock=clock;self.s=fault();self.events=[];self.heals=heals;self.probes=True;self.https=True
        self.failure=None;self.on_scan=None
    def snapshot(self):return copy.deepcopy(self.s)
    def confirm_wired(self):return self.probes
    def diagnostics(self,at):self.events.append('diagnostics')
    def heal(self):self.s.update(target=True,connected=True,ppp=True,ppp_running=True,cell_healthy=True,ip_matches=True)
    def scan(self):
        self.events.append('scan')
        if self.on_scan:self.on_scan()
        if self.heals=='scan':self.heal()
    def rediscover(self):
        self.events.append('rediscover')
        if self.heals=='rediscover':self.heal()
        if self.failure:raise RuntimeError(self.failure)
    def reset(self,guard):
        guard();self.events.append('reset')
        if self.heals=='reset':self.heal()
        return {'reset_command_sent':True,'acknowledged':True}
    def verify_data(self):self.events.append('https');return self.https


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name);self.clock=Clock();self.cfg={**m.DEFAULTS,'enabled':True}
        self.ops=Ops(self.clock);self.ledger=m.Ledger(self.root/'ledger.json')
        self.r=m.Recovery(self.cfg,self.ops,self.ledger,self.clock,self.root/'status.json')
    def trigger(self):
        self.r.tick();self.clock.sleep(self.cfg['fault_seconds']);self.r.tick()

    def test_delay_and_successful_rediscovery_no_reset(self):
        self.r.tick();self.assertEqual(self.ops.events,[])
        self.clock.sleep(89);self.r.tick();self.assertEqual(self.ops.events,[])
        self.clock.sleep(1);self.r.tick()
        self.assertEqual(self.ops.events,['diagnostics','scan','rediscover','https'])
        self.assertTrue(self.r.last_result['success']);self.assertEqual(self.ledger.data['resets'],[])

    def test_scan_success_does_not_restart_mm_or_modem(self):
        self.ops.heals='scan';self.trigger()
        self.assertEqual(self.ops.events,['diagnostics','scan','https'])

    def test_reset_only_after_rescan_and_mm_restart_failure(self):
        self.ops.heals='reset';self.trigger()
        self.assertEqual(self.ops.events,['diagnostics','scan','rediscover','reset','https'])
        self.assertTrue(self.r.last_result['success']);self.assertEqual(len(self.ledger.data['resets']),1)

    def test_every_ineligible_guard_prevents_all_actions(self):
        for key,value in [('fresh',False),('wired',False),('vpn',False),('usb',''),('paused',True),
                          ('other_modems',True),('mm_known',False),('mm_active',False),('mm_masked',True),
                          ('target',True),('ppp',True),('ppp_running',True),('sms_busy',True),('cell_healthy',True)]:
            with self.subTest(key=key):
                self.ops.s=fault();self.ops.s[key]=value;self.r.fault_since=None;self.ops.events=[]
                self.trigger();self.assertEqual(self.ops.events,[])

    def test_fault_stability_resets_on_usb_change_and_wired_failure(self):
        self.r.tick();self.clock.sleep(90);self.ops.s['usb']='usb:43';self.r.tick();self.assertEqual(self.ops.events,[])
        self.ops.s['wired']=False;self.r.tick();self.ops.s['wired']=True;self.r.tick()
        self.clock.sleep(89);self.r.tick();self.assertEqual(self.ops.events,[])

    def test_wired_loss_during_scan_aborts_all_escalation(self):
        self.ops.on_scan=lambda:self.ops.s.update(wired=False);self.trigger()
        self.assertEqual(self.ops.events,['diagnostics','scan']);self.assertFalse(self.r.last_result['success'])

    def test_vpn_or_signature_change_aborts(self):
        for key,value in [('vpn',False),('signature',('new-ip','nm','failover'))]:
            self.setUp()
            self.ops.on_scan=lambda:self.ops.s.update({key:value});self.trigger()
            self.assertNotIn('rediscover',self.ops.events);self.assertNotIn('reset',self.ops.events)

    def test_independent_ping_failure_prevents_mutations(self):
        self.r.tick();self.clock.sleep(90);self.ops.probes=False
        with self.assertRaises(RuntimeError):self.r.tick()
        self.assertEqual(self.ops.events,[]);self.assertEqual(self.ledger.data['cycles'],[])

    def test_recognized_but_no_data_never_forces_reboot(self):
        self.ops.on_scan=lambda:self.ops.s.update(target=True);self.trigger()
        self.assertNotIn('rediscover',self.ops.events);self.assertNotIn('reset',self.ops.events)

    def test_reboot_ok_without_data_is_not_success(self):
        self.ops.heals=None;self.trigger()
        self.assertIn('reset',self.ops.events);self.assertFalse(self.r.last_result['success'])

    def test_https_failure_is_not_success(self):
        self.ops.https=False;self.trigger();self.assertFalse(self.r.last_result['success'])
        self.assertNotIn('reset',self.ops.events)

    def test_limits_survive_daemon_restart_and_clock_rollback(self):
        self.ledger.reserve('cycle',self.clock.time(),self.cfg)
        other=m.Ledger(self.ledger.path)
        self.assertTrue(other.allowed('cycle',self.clock.time()+60,self.cfg))
        self.assertTrue(other.allowed('reset',self.clock.time()-60,self.cfg))
        self.assertFalse(other.allowed('cycle',self.clock.time()+900,self.cfg))
        other.reserve('reset',self.clock.time(),self.cfg)
        self.assertTrue(m.Ledger(self.ledger.path).allowed('reset',self.clock.time()+3600,self.cfg))

    def test_quota_and_failure_breaker(self):
        for i in range(3):self.ledger.reserve('cycle',self.clock.time()+900*i,self.cfg)
        self.assertTrue(self.ledger.allowed('cycle',self.clock.time()+2700,self.cfg))
        for _ in range(3):self.ledger.finish(False,self.clock.time()+3000,self.cfg)
        self.assertTrue(self.ledger.allowed('cycle',self.clock.time()+4000,self.cfg))
        self.assertFalse(self.ledger.allowed('cycle',self.clock.time()+7000,self.cfg))
        # Reboot daily quota is independent from success/failure and cycle retention.
        self.ledger.reserve('reset',self.clock.time()+7000,self.cfg)
        self.ledger.reserve('reset',self.clock.time()+7000+21600,self.cfg)
        self.assertTrue(self.ledger.allowed('reset',self.clock.time()+7000+43200,self.cfg))

    def test_storage_failure_prevents_scan(self):
        self.r.tick();self.clock.sleep(90)
        original=m.atomic_json
        def fail(path,*args):
            if path==self.ledger.path:raise OSError('disk full')
            return original(path,*args)
        with patch.object(m,'atomic_json',side_effect=fail):
            with self.assertRaises(OSError):self.r.tick()
        self.assertEqual(self.ops.events,[])

    def test_corrupt_ledger_is_fail_closed(self):
        for text in ['{}','null','{"cycles": []}']:
            self.ledger.path.write_text(text)
            with self.assertRaises((ValueError,TypeError)):m.Ledger(self.ledger.path)

    def test_at_rejects_configuration_and_call_commands(self):
        at=m.FixedAT('/not-open')
        for text in ['AT+QCFG="usbcfg",1','ATD+12025550123;','ATH','AT+QPCMV=1,2']:
            with self.assertRaises(ValueError):at.exchange(text,1)

    def test_disabled_has_no_hardware_operations(self):
        self.cfg['enabled']=False;self.trigger();self.assertEqual(self.ops.events,[])

    def test_maintenance_restore_never_unmasks_without_owned_marker(self):
        with patch.object(m,'RUNTIME',self.root),patch.object(m,'command') as run:
            m.restore_owned_mm();run.assert_not_called()

    def test_owned_restore_is_narrow_and_idempotent(self):
        boot=self.root/'boot';boot.write_text('test-boot')
        (self.root/'owner.json').write_text(json.dumps(dict(owned=True,boot='test-boot')))
        real_path=Path
        def path(value):
            if str(value)=='/proc/sys/kernel/random/boot_id':return boot
            if str(value)=='/etc/systemd/system/ModemManager.service':return self.root/'no-override'
            return real_path(value)
        with patch.object(m,'RUNTIME',self.root),patch.object(m,'Path',side_effect=path),patch.object(m,'command') as run:
            m.restore_owned_mm();m.restore_owned_mm()
            self.assertEqual([x.args[0] for x in run.call_args_list],
                [['systemctl','unmask','--runtime','ModemManager.service'],['systemctl','start','--no-block','ModemManager.service']])
        self.assertFalse((self.root/'owner.json').exists())

    def test_reset_stop_failure_always_restores_mm(self):
        ops=m.LinuxOps.__new__(m.LinuxOps);ops.cfg=self.cfg
        boot=self.root/'boot';boot.write_text('test-boot');real_path=Path
        def path(value):
            if str(value)=='/proc/sys/kernel/random/boot_id':return boot
            if str(value)=='/run/systemd/system/ModemManager.service':return self.root/'no-mask'
            return real_path(value)
        def run(args,*_):
            if args[:2]==['systemctl','stop']:raise RuntimeError('stop timeout')
            return ''
        with patch.object(m,'RUNTIME',self.root),patch.object(m,'Path',side_effect=path),patch.object(m,'command',side_effect=run),\
             patch.object(m,'restore_owned_mm') as restore,patch.object(m.subprocess,'run'):
            with self.assertRaisesRegex(RuntimeError,'stop timeout'):ops.reset(lambda:None)
            restore.assert_called_once()

    def test_fixed_serial_exchange_with_pseudo_terminal(self):
        master,slave=pty.openpty();name=os.ttyname(slave);received=[]
        def modem():
            data=b''
            for _ in range(3):
                while b'\r' not in data:
                    if not select.select([master],[],[],2)[0]:return
                    data+=os.read(master,1024)
                text,data=data.split(b'\r',1);received.append(text.decode())
                reply=(self.cfg['firmware']+'\r\nOK\r\n') if text==b'AT+CGMR' else '\r\nOK\r\n'
                os.write(master,reply.encode())
        worker=threading.Thread(target=modem,daemon=True)
        try:
            with m.FixedAT(name) as at:
                worker.start();self.assertIn('OK',at.exchange('AT',1))
                self.assertIn(self.cfg['firmware'],at.exchange('AT+CGMR',1))
                self.assertIn('OK',at.exchange('AT+CFUN=1,1',1))
            worker.join(2);self.assertEqual(received,['AT','AT+CGMR','AT+CFUN=1,1'])
        finally:os.close(master);os.close(slave)

    def test_ppp_removed_between_exists_and_address_query_is_safe(self):
        ops=m.LinuxOps.__new__(m.LinuxOps);ops.cfg=self.cfg
        ops.net={'vpns':[{'interface':'tun0'},{'interface':'tun1'},{'interface':'tun2'}]}
        ops.usb_identity=lambda:'usb:42'
        state={'updated':datetime.datetime.now(datetime.timezone.utc).isoformat(), 'active':'wired',
               'wired_healthy':True,'wired_link':True,'wired_ip':'192.0.2.20',
               'cellular_healthy':False,'vpns':{'tun0':True,'tun1':True,'tun2':True}}
        calls=[]
        class FakePath:
            def __init__(self,value):self.value=str(value)
            def exists(self):
                if self.value=='/sys/class/net/ppp0':calls.append(1);return len(calls)==1
                return False
            def read_text(self):
                if self.value.endswith('status.json'):return json.dumps(state)
                if self.value.endswith('/carrier'):return '1'
                raise AssertionError(self.value)
        def run(args,*_):
            if args[:2]==['ip','-4']:
                if args[-1]=='ppp0':raise subprocess.CalledProcessError(1,args)
                return json.dumps([{'addr_info':[{'scope':'global','local':'192.0.2.20'}]}])
            if args[:2]==['mmcli','-L']:return '{"modem-list":[]}'
            if 'ActiveState' in args:return 'active'
            if 'UnitFileState' in args:return 'enabled'
            return '123'
        with patch.object(m,'Path',side_effect=FakePath),patch.object(m,'command',side_effect=run),\
             patch.object(m.subprocess,'run',return_value=SimpleNamespace(returncode=1)):
            s=ops.snapshot()
        self.assertFalse(s['ppp']);self.assertFalse(s['ip_matches']);self.assertTrue(m.missing_fault(s))


if __name__=='__main__':unittest.main()
