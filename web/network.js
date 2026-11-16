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
// Detailed VPN identity is separate from conservative hardware guard slots.
function vpnEntries(f,fresh,now=Date.now()){
 const details=f.vpn_status;
 if(details&&Object.keys(details).length)return Object.entries(details).map(([id,v])=>{
  const age=now-Date.parse(v.observed_at),valid=fresh&&Number.isFinite(age)&&age>=-5000&&age<=30000;
  const status=valid&&['healthy','unhealthy','unknown'].includes(v.state)?v.state:'unknown';
  return {...v,id,state:status,reason:valid?v.reason:'observation-stale'};
 });
 return Object.entries(f.vpns||{}).map(([id,ok])=>({id,interface:id,state:fresh&&ok===true?'healthy':'unknown',reason:fresh?'legacy-observation':'observation-stale'}));
}
function vpnReason(reason){return ({
 'session-missing-confirmed':'已确认 VPN 会话退出',
 'backend-fd-pressure':'VPN 后台资源接近上限',
 'backend-reconnect-exhausted':'VPN 持续重连超时',
 'backend-present-or-starting':'后台仍存在或正在启动，暂不新建',
 'peer-tunnel-present':'内网路由仍指向隧道，暂不新建',
 'service-start-in-progress':'VPN 服务正在启动',
 'session-remote-unresolved':'会话远端待确认',
 'peer-reachable-unmanaged':'内网可达，后台与远端已交叉核对',
 'unmanaged-peer-not-confirmed':'未托管会话连通性待确认',
 'unmanaged-backend-ambiguous':'未托管后台身份待确认',
 'unmanaged-query-failed':'未托管后台查询失败',
 'peer-reachable':'内网可达','peer-no-reply':'内网探测无响应',
 'probe-command-error':'探测命令异常','probe-execution-failed':'探测执行失败',
 'session-query-failed':'会话查询失败','session-unresolved':'未能定位已有会话',
 'session-changed-or-observation-stale':'检测期间会话变化或结果过期',
 'multiple-sessions':'存在多个同身份会话','session-remote-mismatch':'会话远端不匹配',
 'interface-unresolved':'接口未确认','interface-address-unavailable':'接口地址未确认',
 'peer-route-mismatch':'内网路由与隧道不匹配','interface-route-query-failed':'接口或路由查询失败',
 'shared-session-interface':'多个会话共用接口，身份待确认',
 'session-transition-or-authentication':'会话正在连接、暂停或等待认证',
 'legacy-observation':'旧版监控未提供检测详情','observation-stale':'检测结果已过期或不可用'
 })[reason]||'检测结果待确认';}
function vpnRecovery(reason){return ({
 'recovery-start-requested':'正在重建已退出的 VPN 会话',
 'recovery-rebuild-requested':'正在回收故障会话，随后受控重建',
 'recovery-containment-requested':'已请求资源保护，重建仍受次数限制',
 'recovery-waiting-uplink':'等待出口恢复后重建',
 'another-recovery-in-flight':'等待其他恢复任务结束',
 'recovery-maintenance':'自动恢复已暂停',
 'manual-session-ownership':'只读确认，未托管会话不自动重连',
 'recovery-requested':'已请求恢复','recovery-in-flight':'恢复任务进行中',
 'recovery-backoff':'恢复退避中','recovery-budget-exhausted':'已达恢复次数上限',
 'clock-moved-backward':'系统时间回退，恢复已暂停',
 'recovery-revalidation-failed':'恢复前复核未通过',
 'recovery-check-or-command-failed':'恢复检查或命令失败'
 })[reason]||'';}
function vpnDetails(vpns,safeRecovery=false){
 return `<section class="panel vpn-panel"><div class="panel-heading"><h2>VPN 实际通信</h2></div>${vpns.length?vpns.map(v=>`<div class="vpn-observation"><div><strong>${esc(v.id)}</strong><small>${esc(v.interface||'接口待确认')} · ${esc(vpnReason(v.reason))}</small><small>${v.observed_at?`检测时间 ${esc(clockLabel(v.observed_at))}`:'等待详细检测'}${vpnRecovery(v.recovery)?` · ${esc(vpnRecovery(v.recovery))}`:''}</small>${Number.isFinite(v.backend_fd_count)&&Number.isFinite(v.backend_fd_limit)?`<small>后台资源 ${v.backend_fd_count} / ${v.backend_fd_limit}</small>`:''}</div><div>${pill(v.state==='healthy'?'正常':v.state==='unhealthy'?'异常':'待确认',v.state==='healthy'?'green':'amber')}${v.state==='healthy'&&Number.isFinite(v.latency_ms)?`<small>${v.latency_ms.toFixed(1)} ms</small>`:''}</div></div>`).join(''):'<p>等待 VPN 状态</p>'}<p class="muted small">${safeRecovery?'检测异常不会触发 VPN 重连；恢复任务受连续故障复核、退避与次数上限保护。':'当前监控未声明受限恢复策略，无法确认其重连保护。'}</p></section>`;
}
function networkView(){
 const s=state.status,n=s.network,f=s.failover,cfg=n.config,vpns=vpnEntries(f,n.fresh),up=vpns.filter(v=>v.state==='healthy').length;
 if(f.source==='macos-local')return localNetworkView(n,f);
 const countdown=n.recovery_remaining;
 const reason=n.last_transition?.reason||'暂未观测到切换';
 const status=n.fresh?f.active==='wired'?'有线网络主用':'蜂窝网络主用':'等待主备服务状态';
 const description=n.fresh?(countdown!==null&&countdown!==undefined?`有线已恢复，稳定观察还需约 ${Math.ceil(countdown)} 秒。`:!f.cellular_present?'EC20 不在，重拨已暂停；重新插入后自动连接。':f.active==='wired'?'有线故障时自动切换到 EC20，恢复稳定后自动回切。':'有线持续接受独立探测，EC20 正在承载远程连接。'):'主备服务状态不可用或已过期，暂不判断出口健康。';
 return heading('网络诊断','看清当前出口、备用链路与每一次切换。','NETWORK / DIAGNOSTICS')+
 `<section class="panel summary"><div class="summary-icon">${icon('ethernet')}</div><div><h2>${status}</h2><p>${description}</p></div><div class="summary-end"><div class="label">主备状态更新</div><div class="value">${clockLabel(f.updated)}</div></div></section>`+
 `<div class="diagnostic-metrics"><section class="panel diagnostic-metric"><span>连续失败次数</span><strong>${n.fresh?f.wired_failures:'—'}<small> / ${cfg.failure_count}</small></strong><p>检测执行异常不计入连续失败</p></section><section class="panel diagnostic-metric"><span>恢复稳定期</span><strong>${countdown!==null&&countdown!==undefined?Math.ceil(countdown):cfg.recovery_seconds}<small>秒${countdown!==null&&countdown!==undefined?'剩余':''}</small></strong><p>蜂窝消失且有线正常时立即回切</p></section><section class="panel diagnostic-metric"><span>VPN 隧道</span><strong>${up}<small> / ${vpns.length} 正常</small></strong><p>${vpns.length?vpns.map(v=>`${esc(v.id)} ${v.state==='healthy'?'✓':v.state==='unhealthy'?'异常':'待确认'}`).join(' · '):'等待状态'}</p></section></div>`+
 vpnDetails(vpns,['identity-bounded-v1','identity-bounded-v2'].includes(f.vpn_recovery_policy))+
 `<div class="network-interfaces">${n.interfaces.map(i=>`<section class="panel interface-card"><div class="panel-heading"><h2>${icon(i.role==='wired'?'ethernet':'radio')} ${i.role==='wired'?'有线接口':'蜂窝接口'} <span class="sub mono">${esc(i.name)}</span></h2>${pill(i.present?i.up?'接口已启用':'接口未启用':'接口不存在',i.present&&i.up?'green':'amber')}</div><div class="interface-address"><span>${esc((i.role==='wired'?f.wired_ip:f.cellular_ip)||'未获取 IPv4 地址')}</span>${n.fresh&&f.active===i.role?pill('当前主出口','blue'):''}</div><div class="interface-rates"><div><small>↓ 当前接收</small><strong>${i.rate_available?formatBytes(i.rx_bps,true):'等待采样'}</strong></div><div><small>↑ 当前发送</small><strong>${i.rate_available?formatBytes(i.tx_bps,true):'等待采样'}</strong></div></div>${trafficGraph(n.samples,i.role)}<div class="interface-totals"><span>接口累计接收 <b>${i.present?formatBytes(i.rx_bytes):'—'}</b></span><span>接口累计发送 <b>${i.present?formatBytes(i.tx_bytes):'—'}</b></span></div></section>`).join('')}</div>`+
 `<p class="network-data-note">接口累计值来自系统计数，接口重建时可能重置；速率仅保留最近 60 个采样点，均不代表运营商套餐余额。</p>`+
 `<div class="network-bottom"><section class="panel"><div class="panel-heading"><h2>自动主备策略</h2>${pill(n.config_readable?'只读配置':'配置未确认',n.config_readable?'green':'amber')}</div><dl class="device-info"><div class="info-row"><dt>检测间隔</dt><dd>${cfg.interval_seconds} 秒</dd></div><div class="info-row"><dt>有线上游探测</dt><dd class="mono">${cfg.probe_targets.map(esc).join(' / ')}</dd></div><div class="info-row"><dt>EC20 缺席时</dt><dd>暂停重拨，重新插入自动恢复</dd></div><div class="info-row"><dt>最近切换时间</dt><dd>${f.last_switch?dateText(f.last_switch):'尚未记录'}</dd></div><div class="info-row"><dt>最近切换说明</dt><dd>${esc(reason)}</dd></div></dl><div class="diagnostic-actions"><button id="copy-diagnostics" class="button secondary">${icon('copy')}复制脱敏诊断摘要</button><small>不含短信、电话号码或登录凭据</small></div></section><section class="panel"><div class="panel-heading"><h2>切换记录</h2><span class="label">PERSISTED LOCALLY</span></div>${n.history_error?`<div class="network-history-error">${esc(n.history_error)}</div>`:''}${n.events.length?`<div class="switch-history">${n.events.slice(0,12).map(e=>`<div class="switch-event"><span class="switch-dot"></span><div><strong>${sourceName(e.from)} → ${sourceName(e.to)}</strong><p>${esc(e.reason)}${e.inferred?' <span class="inferred-label">状态推断</span>':''}</p><time>${dateText(e.time)}</time></div></div>`).join('')}</div>`:empty('clock','暂无切换记录','控制台观测到切换后，会保存在本地。')}<div class="history-note">仅记录控制台已观测到的切换。原因依据采样状态推断，首次启动不会补造历史原因。</div></section></div>`;
}
function localNetworkView(n,f){
 const primary=f.active==='wired'?'主网络':f.active==='cellular'?'EC20 蜂窝':'出口待确认';
 return heading('Mac 网络诊断','查看当前出口、EC20 ECM 连通性与实际流量。','MAC / NETWORK')+
 `<section class="panel summary"><div class="summary-icon">${icon('ethernet')}</div><div><h2>${n.fresh?primary+'主用':'等待本机网络探测'}</h2><p>控制台只观测默认路由，并分别通过主网络和 EC20 探测 HTTPS；当前模式不执行自动切换。</p></div><div class="summary-end"><div class="label">最近探测</div><div class="value">${clockLabel(f.updated)}</div></div></section>`+
 `<div class="network-interfaces">${n.interfaces.map(i=>`<section class="panel interface-card"><div class="panel-heading"><h2>${icon(i.role==='wired'?'ethernet':'radio')} ${i.role==='wired'?'主网络':'EC20 ECM'} <span class="sub mono">${esc(i.name)}</span></h2>${pill(n.fresh&&(i.role==='wired'?f.wired_healthy:f.cellular_healthy)?'HTTPS 可达':'连通性待确认',n.fresh&&(i.role==='wired'?f.wired_healthy:f.cellular_healthy)?'green':'amber')}</div><div class="interface-address">${esc((i.role==='wired'?f.wired_ip:f.cellular_ip)||'未获取 IPv4 地址')}</div><div class="interface-rates"><div><small>↓ 当前接收</small><strong>${i.rate_available?formatBytes(i.rx_bps,true):'等待采样'}</strong></div><div><small>↑ 当前发送</small><strong>${i.rate_available?formatBytes(i.tx_bps,true):'等待采样'}</strong></div></div>${trafficGraph(n.samples,i.role)}<div class="interface-totals"><span>累计接收 <b>${i.present?formatBytes(i.rx_bytes):'—'}</b></span><span>累计发送 <b>${i.present?formatBytes(i.tx_bytes):'—'}</b></span></div></section>`).join('')}</div>`+
 `<section class="panel"><div class="panel-heading"><h2>本机操作保护</h2></div><p>主网络主用且两条链路探测通过后，才开放短信、拨号和 GNSS 启用。路由或 USB 设备变化会锁定操作，等待重新探测。</p><p>未配置 VPN 健康监控或自动主备服务。流量来自系统接口计数，不代表运营商套餐余额。</p><div class="diagnostic-actions"><button id="copy-diagnostics" class="button secondary">${icon('copy')}复制脱敏诊断摘要</button></div></section>`;
}
function diagnosticSummary(){
 const s=state.status,n=s.network,f=s.failover;
 return JSON.stringify({version:s.version,observed_at:s.updated,modem_last_success:s.last_modem_success,
  device:{model:s.radio.model,present:f.cellular_present,operator:s.radio.operator,technology:s.radio.technology,signal:s.radio.signal},
  network:{active:f.active,wired_healthy:f.wired_healthy,wired_ip:f.wired_ip,cellular_healthy:f.cellular_healthy,cellular_ip:f.cellular_ip,vpns:f.vpns,vpn_status:f.vpn_status,vpn_recovery_policy:f.vpn_recovery_policy,wired_probe:f.wired_probe,cellular_probe:f.cellular_probe,state_updated:f.updated},
  config:n.config,interfaces:n.interfaces,last_switches:n.events.slice(0,10)},null,2);
}
