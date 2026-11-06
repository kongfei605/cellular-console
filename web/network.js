'use strict';

function formatBytes(value,rate=false){
 if(!Number.isFinite(value)||value<0)return '—';const units=['B','KiB','MiB','GiB','TiB'];let i=0;
 while(value>=1024&&i<units.length-1){value/=1024;i++;}
 return `${value.toFixed(i?value>=100?0:1:0)} ${units[i]}${rate?'/s':''}`;
}
function sourceName(source){return source==='wired'?'有线':source==='cellular'?'蜂窝':'未知';}
function trafficGraph(samples,role){
 if(samples.length<2)return '<div class="traffic-wait">正在采样，下一次更新后显示速率曲线</div>';
 const rx=role==='wired'?'wired_rx':'cell_rx',tx=role==='wired'?'wired_tx':'cell_tx';
 const top=Math.max(1,...samples.flatMap(s=>[s[rx]||0,s[tx]||0]));
 const points=key=>samples.map((s,i)=>`${(i/(samples.length-1)*600).toFixed(1)},${(92-Math.max(0,s[key]||0)/top*78).toFixed(1)}`).join(' ');
 return `<div class="traffic-graph"><div class="chart-scale">${formatBytes(top,true)}</div><svg viewBox="0 0 600 100" role="img" aria-label="${role==='wired'?'有线':'蜂窝'}最近速率曲线" preserveAspectRatio="none"><path d="M0 18H600M0 55H600M0 92H600" class="chart-grid"/><polyline points="${points(rx)}" class="chart-rx"/><polyline points="${points(tx)}" class="chart-tx"/></svg><div class="chart-legend"><span><i class="legend-rx"></i>接收</span><span><i class="legend-tx"></i>发送</span><small>${samples.length} 个观测点 · 最近约 ${Math.round((new Date(samples.at(-1).time)-new Date(samples[0].time))/1000)} 秒</small></div></div>`;
}
function networkView(){
 const s=state.status,n=s.network,f=s.failover,cfg=n.config,vpns=Object.entries(f.vpns||{}),up=vpns.filter(([,ok])=>ok).length;
 const countdown=n.recovery_remaining;
 const reason=n.last_transition?.reason||'暂未观测到切换';
 const status=n.fresh?f.active==='wired'?'有线网络主用':'蜂窝网络主用':'等待主备服务状态';
 const description=n.fresh?(countdown!==null&&countdown!==undefined?`有线已恢复，稳定观察还需约 ${Math.ceil(countdown)} 秒。`:!f.cellular_present?'EC20 不在，重拨已暂停；重新插入后自动连接。':f.active==='wired'?'有线故障时自动切换到 EC20，恢复稳定后自动回切。':'有线持续接受独立探测，EC20 正在承载远程连接。'):'主备服务状态不可用或已过期，暂不判断出口健康。';
 return heading('网络诊断','看清当前出口、备用链路与每一次切换。','NETWORK / DIAGNOSTICS')+
 `<section class="panel summary"><div class="summary-icon">${icon('ethernet')}</div><div><h2>${status}</h2><p>${description}</p></div><div class="summary-end"><div class="label">主备状态更新</div><div class="value">${clockLabel(f.updated)}</div></div></section>`+
 `<div class="diagnostic-metrics"><section class="panel diagnostic-metric"><span>连续失败次数</span><strong>${n.fresh?f.wired_failures:'—'}<small> / ${cfg.failure_count}</small></strong><p>两个探测目标均失败才计一次</p></section><section class="panel diagnostic-metric"><span>恢复稳定期</span><strong>${countdown!==null&&countdown!==undefined?Math.ceil(countdown):cfg.recovery_seconds}<small>秒${countdown!==null&&countdown!==undefined?'剩余':''}</small></strong><p>蜂窝消失且有线正常时立即回切</p></section><section class="panel diagnostic-metric"><span>VPN 隧道</span><strong>${up}<small> / ${vpns.length} 正常</small></strong><p>${vpns.length?vpns.map(([name,ok])=>`${esc(name)} ${ok?'✓':'—'}`).join(' · '):'等待状态'}</p></section></div>`+
 `<div class="network-interfaces">${n.interfaces.map(i=>`<section class="panel interface-card"><div class="panel-heading"><h2>${icon(i.role==='wired'?'ethernet':'radio')} ${i.role==='wired'?'有线接口':'蜂窝接口'} <span class="sub mono">${esc(i.name)}</span></h2>${pill(i.present?i.up?'接口已启用':'接口未启用':'接口不存在',i.present&&i.up?'green':'amber')}</div><div class="interface-address"><span>${esc((i.role==='wired'?f.wired_ip:f.cellular_ip)||'未获取 IPv4 地址')}</span>${n.fresh&&f.active===i.role?pill('当前主出口','blue'):''}</div><div class="interface-rates"><div><small>↓ 当前接收</small><strong>${i.rate_available?formatBytes(i.rx_bps,true):'等待采样'}</strong></div><div><small>↑ 当前发送</small><strong>${i.rate_available?formatBytes(i.tx_bps,true):'等待采样'}</strong></div></div>${trafficGraph(n.samples,i.role)}<div class="interface-totals"><span>接口累计接收 <b>${i.present?formatBytes(i.rx_bytes):'—'}</b></span><span>接口累计发送 <b>${i.present?formatBytes(i.tx_bytes):'—'}</b></span></div></section>`).join('')}</div>`+
 `<p class="network-data-note">接口累计值来自系统计数，接口重建时可能重置；速率仅保留最近 60 个采样点，均不代表运营商套餐余额。</p>`+
 `<div class="network-bottom"><section class="panel"><div class="panel-heading"><h2>自动主备策略</h2>${pill(n.config_readable?'只读配置':'配置未确认',n.config_readable?'green':'amber')}</div><dl class="device-info"><div class="info-row"><dt>检测间隔</dt><dd>${cfg.interval_seconds} 秒</dd></div><div class="info-row"><dt>有线上游探测</dt><dd class="mono">${cfg.probe_targets.map(esc).join(' / ')}</dd></div><div class="info-row"><dt>EC20 缺席时</dt><dd>暂停重拨，重新插入自动恢复</dd></div><div class="info-row"><dt>最近切换时间</dt><dd>${f.last_switch?dateText(f.last_switch):'尚未记录'}</dd></div><div class="info-row"><dt>最近切换说明</dt><dd>${esc(reason)}</dd></div></dl><div class="diagnostic-actions"><button id="copy-diagnostics" class="button secondary">${icon('copy')}复制脱敏诊断摘要</button><small>不含短信、电话号码或登录凭据</small></div></section><section class="panel"><div class="panel-heading"><h2>切换记录</h2><span class="label">PERSISTED LOCALLY</span></div>${n.history_error?`<div class="network-history-error">${esc(n.history_error)}</div>`:''}${n.events.length?`<div class="switch-history">${n.events.slice(0,12).map(e=>`<div class="switch-event"><span class="switch-dot"></span><div><strong>${sourceName(e.from)} → ${sourceName(e.to)}</strong><p>${esc(e.reason)}${e.inferred?' <span class="inferred-label">状态推断</span>':''}</p><time>${dateText(e.time)}</time></div></div>`).join('')}</div>`:empty('clock','暂无切换记录','控制台观测到切换后，会保存在本地。')}<div class="history-note">仅记录控制台已观测到的切换。原因依据采样状态推断，首次启动不会补造历史原因。</div></section></div>`;
}
function diagnosticSummary(){
 const s=state.status,n=s.network,f=s.failover;
 return JSON.stringify({version:s.version,observed_at:s.updated,modem_last_success:s.last_modem_success,
  device:{model:s.radio.model,present:f.cellular_present,operator:s.radio.operator,technology:s.radio.technology,signal:s.radio.signal},
  network:{active:f.active,wired_healthy:f.wired_healthy,wired_ip:f.wired_ip,cellular_healthy:f.cellular_healthy,cellular_ip:f.cellular_ip,vpns:f.vpns,state_updated:f.updated},
  config:n.config,interfaces:n.interfaces,last_switches:n.events.slice(0,10)},null,2);
}
