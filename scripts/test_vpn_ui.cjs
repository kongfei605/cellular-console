// Run with Node.js 18+; no browser, network or third-party dependencies.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.resolve(__dirname, '..');
const context = vm.createContext({
 esc: v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
 clockLabel: v => v, pill: v => v,
});
vm.runInContext(fs.readFileSync(path.join(root, 'web/network.js'), 'utf8'), context);
new vm.Script(fs.readFileSync(path.join(root, 'web/app.js'), 'utf8'));
const now = Date.parse('2026-01-01T00:00:00Z');
const f = {vpns: {tun0:false}, vpn_status: {
 office: {state:'healthy',interface:'tun14',reason:'peer-reachable',latency_ms:42.1,observed_at:'2026-01-01T00:00:00+0000'},
 home: {state:'unknown',interface:'tun0',reason:'probe-command-error',observed_at:'2026-01-01T00:00:00+0000'},
}};
let entries = context.vpnEntries(f,true,now);
assert.equal(entries.length,2);
assert.equal(entries[0].id,'office');
assert.equal(entries[0].interface,'tun14');
assert.equal(entries[0].state,'healthy');
assert.equal(entries[1].state,'unknown');
assert.equal(context.vpnEntries(f,false,now)[0].state,'unknown');
assert.equal(context.vpnEntries(f,true,now+31000)[0].state,'unknown');
assert.equal(context.vpnEntries(f,true,now-6000)[0].state,'unknown');
assert.equal(context.vpnEntries({vpns:{tun0:false}},true,now)[0].state,'unknown');
assert.equal(context.vpnEntries({vpns:{tun0:true}},false,now)[0].state,'unknown');
assert.equal(context.vpnEntries({vpns:{tun0:true}},true,now)[0].state,'healthy');
let html = context.vpnDetails(entries,true);
assert.ok(html.includes('正常') && html.includes('待确认') && html.includes('42.1 ms'));
assert.ok(html.includes('探测命令异常'));
assert.ok(html.includes('检测异常不会触发'));
assert.ok(!context.vpnDetails(entries,false).includes('检测异常不会触发'));
assert.ok(context.vpnDetails([{...entries[0],id:'<script>bad</script>'}],true).includes('&lt;script&gt;'));
assert.ok(!context.vpnDetails([{...entries[0],state:'unknown'}],true).includes('42.1 ms'));
assert.ok(context.vpnDetails([{...entries[0],backend_fd_count:14,backend_fd_limit:1024}],true).includes('后台资源 14 / 1024'));
assert.ok(context.vpnRecovery('recovery-start-requested').includes('重建'));
assert.ok(context.vpnReason('session-missing-confirmed').includes('退出'));
console.log('VPN UI assertions passed; app.js and network.js parse successfully');
