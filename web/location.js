'use strict';
let locationUI={data:null,error:'',busy:false,loading:false,received:0};
function resetLocation(){locationUI={data:null,error:'',busy:false,loading:false,received:0};document.querySelector('#location-dialog')?.close();}
async function loadLocation(){
 if(!state.logged||state.view!=='location'||locationUI.loading||locationUI.busy)return;
 const epoch=state.authEpoch,ui=locationUI;ui.loading=true;
 try{const data=await api('/api/location');if(!state.logged||epoch!==state.authEpoch||ui!==locationUI)return;ui.data=data;ui.error='';ui.received=performance.now();}
 catch(e){if(state.logged&&epoch===state.authEpoch&&ui===locationUI){ui.data=null;ui.error=e.message;}}
 finally{ui.loading=false;}
}
function locationDiagnosticsView(l,stale){
 const d=l?.diagnostics||{},cache=l?.cache||{},active=!!(l?.available&&l.gnss_on&&!stale),liveFix=active&&l.fix;
 const gsa=Array.isArray(d.gsa_fix_types)?d.gsa_fix_types.map(n=>({1:'1 · 未定位',2:'2 · 二维',3:'3 · 三维'}[n]||'未知')).join(' / '):'';
 const reportSummary=`RMC ${d.has_rmc?d.rmc_status||'状态为空':'缺失'} · GGA ${d.has_gga?(d.gga_quality??'质量字段为空'):'缺失'} · GSA ${d.has_gsa?gsa||'状态字段不可用':'缺失'}`;
 const satelliteTime={missing:'卫星日期 / UTC 缺失',invalid:'卫星日期 / UTC 无效',stale:'卫星日期 / UTC 已过期或异常',fresh:'卫星日期 / UTC 在有效时间窗内'}[d.satellite_time]||'卫星时间待检查';
 const seconds=Math.max(0,Math.floor(Number(cache.unchanged_seconds)||0));
 const duration=seconds<60?seconds+' 秒':Math.floor(seconds/60)+' 分 '+seconds%60+' 秒';
 const cacheLabel=!active?'未比较':({first:'首次读取，等待比较',unchanged:'内容未变化',changed:'内容发生变化',empty:'无有效 NMEA 缓存',unknown:'本次无法比较'}[cache.state]||'等待比较');
 const resetReason={observation_resumed:'读取间隔超过 2 分钟，已重新观察。',configuration_changed:'设备或定位源发生变化，已重新观察。'}[cache.reason]||'';
 const cacheDetail=!active?'GNSS 未开启、接口不可用或页面数据待刷新。':cache.state==='unchanged'?`已观察 ${duration} 内容未变化；${cache.samples||0} 次读取。`:cache.state==='changed'?`本次观察已记录 ${cache.content_changes||0} 次内容变化。`:resetReason||'需要后续成功读取才能判断内容是否变化。';
 const recentChange=cache.last_observed_change?'最近观察到变化 '+dateText(cache.last_observed_change):'尚未观察到内容变化';
 return `<section class="location-diagnostics" aria-label="定位诊断"><section class="panel location-diagnostic"><div class="location-diagnostic-head"><span>定位结果</span>${pill(active&&!liveFix?'未定位':liveFix?'已定位':l?.gnss_on?'待刷新':'未启用',liveFix?'green':'amber')}</div><h2>${active?(liveFix?'有效卫星定位':'尚无有效坐标'):'等待有效观测'}</h2><p>${esc(active?reportSummary:'GNSS 开关与定位成功是不同状态。')}</p><small>${esc(active?satelliteTime:'旧缓存不作为当前位置。')}</small></section>
 <section class="panel location-diagnostic"><div class="location-diagnostic-head"><span>卫星报告</span>${pill(active?(d.has_gsv?'已收到 GSV':'报告缺失'):'待检查',active&&d.has_gsv?'':'amber')}</div><h2>${active?(d.has_gsv?'GSV 已报告':'GSV 报告缺失'):'尚未判定'}</h2><p>${esc(!active?'启用并成功读取后检查卫星报告。':d.has_gsv?(l.satellites_visible===undefined?'可见卫星数量字段不可用。':'报告可见 '+l.satellites_visible+' 颗卫星。'):'没有校验通过的可见卫星数量报告；不能解读成可见卫星为 0。')}</p><small>${esc(active?(d.snr_reports?'已报告 '+d.snr_reports+' 个载噪比值，最大 '+d.max_snr_dbhz+' dB-Hz。':'载噪比报告缺失，无法评估接收强弱。'):'卫星数量与信号状态待检查。')}</small></section>
 <section class="panel location-diagnostic"><div class="location-diagnostic-head"><span>缓存内容</span>${pill(active&&cache.state==='changed'?'观察到变化':'按需比较',active&&cache.state==='changed'?'green':'')}</div><h2>${esc(cacheLabel)}</h2><p>${esc(cacheDetail)}</p><small>${esc(active?recentChange:'读取恢复后开始新的观察。')}</small></section>
 <section class="panel location-diagnostic"><div class="location-diagnostic-head"><span>天线状态</span>${pill('未知','')}</div><h2>无法远程判定</h2><p>当前接口未提供天线开路、短路或供电检测状态。</p><small>未定位或卫星报告缺失，不能区分未接天线与室内遮挡。需要现场核对 GNSS 接口、天线与接收环境。</small></section></section>
 <div class="location-cache-note">${icon('clock')}<span>最近读取${l&&!stale?' '+esc(dateText(l.observed)):'：待刷新'}。这是控制台读取时间，并非卫星数据生成时间。内容未变化不代表接收器停止输出；内容有变化也不代表定位有效。观察仅在读取时进行，间隔超过 2 分钟或服务重启后重新开始。</span></div>`;
}
function locationView(){
 const l=locationUI.data,freshNetwork=state.status.network.fresh,stale=!l||performance.now()-locationUI.received>35000,valid=!!(l?.fix&&l.position&&!stale&&freshNetwork);
 const on=l?.gnss_on,external=!!(l?.enabled_sources&16),blocked=!l?.enable_allowed||stale||!freshNetwork||locationUI.busy;
 const label=!l?'等待读取':external?'外部管理':!l.available?'接口不可用':stale?'状态待刷新':!on?'定位已停止':valid?'已获得定位':'未定位';
 const coordinate=(name,value,unit)=>`<div class="location-coordinate"><span>${name}</span><strong>${value===undefined?'—':esc(Number(value).toFixed(6))}<small>${unit}</small></strong></div>`;
 const sourceNames=mask=>[[1,'3GPP 小区'],[2,'GPS raw'],[4,'GPS NMEA'],[16,'非托管 GPS']].filter(([bit])=>mask&bit).map(([,name])=>name).join(' · ')||'无';
 return heading('卫星定位','查看 EC20 的 GNSS 数据与定位状态。','GNSS / SATELLITE POSITIONING')+
 (locationUI.error?`<div class="error-banner" role="alert">${esc(locationUI.error)}；已隐藏先前位置。请使用右上角刷新重试。</div>`:'')+
 `<section class="panel location-summary"><div class="summary-icon">${icon('location')}</div><div><div class="eyebrow">EC20 · GNSS</div><h2>${label}</h2><p>${esc(stale&&l?'定位数据未刷新，已隐藏先前位置':l?.fix_reason||'正在读取模块定位接口…')}</p></div><div class="location-actions"><button id="location-enable" class="button primary" ${on||external||blocked?'disabled':''}>${locationUI.busy?'正在设置…':'启用 GNSS'}</button><button id="location-stop" class="button secondary" ${!on||!l?.control_enabled||locationUI.busy?'disabled':''}>停止定位</button></div></section>`+
 (!on&&l?.control_reason?`<div class="notice amber">${icon('shield')}${esc(l.control_reason)}</div>`:'')+
 (l?.demo?'<div class="notice amber">演示模式：坐标为公开 NMEA 示例数据，未访问真实硬件。</div>':'')+
 locationDiagnosticsView(l,stale)+
 `<div class="location-layout"><section class="panel location-position"><div class="panel-heading"><h2>当前位置<span class="sub">WGS84</span></h2>${pill(valid?'有效定位':on?'未定位':'未启用',valid?'green':'amber',valid)}</div><div class="location-position-body ${valid?'has-fix':''}"><div class="location-radar" aria-hidden="true"><i></i><i></i><i></i><span>${icon('location')}</span></div><h3>${valid?'卫星定位已解算':on?'尚未获得有效定位':'开始接收卫星信号'}</h3><p>${valid?'仅展示校验通过且 90 秒内的定位数据。':on?'已启用不代表已定位。请保持 GNSS 天线连接，并尽量提供开阔天空视野。':'手动启用后，模块将通过专用 GNSS 端口输出定位信息。'}</p></div><div class="location-coordinates">${coordinate('纬度 LATITUDE',valid?l.position.latitude:undefined,'°')}${coordinate('经度 LONGITUDE',valid?l.position.longitude:undefined,'°')}</div><dl class="device-info"><div class="info-row"><dt>海拔</dt><dd>${valid&&l.position.altitude!==undefined?esc(l.position.altitude.toFixed(1))+' m':'—'}</dd></div><div class="info-row"><dt>卫星时间 UTC</dt><dd class="mono">${valid?esc(l.position.utc.replace('T',' ').replace('Z',' UTC')):'等待有效时间'}</dd></div></dl></section><div><section class="panel location-observation"><div class="panel-heading"><h2>接收观测</h2><span class="label">NMEA</span></div><div class="location-metrics"><div><span>参与定位</span><strong>${on&&!stale?l?.satellites_used??'—':'—'}<small>颗</small></strong></div><div><span>报告可见</span><strong>${on&&!stale?l?.satellites_visible??'—':'—'}<small>颗</small></strong></div><div><span>水平精度因子</span><strong>${on&&!stale&&l?.hdop!==undefined?esc(l.hdop.toFixed(1)):'—'}<small>HDOP</small></strong></div><div><span>校验通过语句</span><strong>${on&&!stale?l?.sentences??0:'—'}<small>条</small></strong></div></div><p class="location-footnote">参与定位数量优先读取 GGA，缺失时仅使用单条 GSA 报告；可见卫星数量来自 GSV。报告缺失时显示 —，不填造为 0。坐标需通过 RMC 状态、校验和及卫星时间核验。</p></section><section class="panel location-configuration"><div class="panel-heading"><h2>定位配置</h2>${pill(l?.reviewed_profile?'本机配置已验证':'待验证',l?.reviewed_profile?'green':'amber')}</div><dl class="device-info"><div class="info-row"><dt>支持数据源</dt><dd>${esc(sourceNames(l?.capabilities||0))}</dd></div><div class="info-row"><dt>已启用数据源</dt><dd>${esc(sourceNames(l?.enabled_sources||0))}</dd></div><div class="info-row"><dt>位置广播</dt><dd>${l?.signals?'外部已开启':'关闭 · 登录后按需读取'}</dd></div><div class="info-row"><dt>定位接口刷新配置</dt><dd>${l?esc(l.refresh_seconds)+' 秒':'—'}</dd></div></dl></section></div></div>`+
 `<section class="panel location-nmea"><div class="panel-heading"><h2>NMEA 观测<span class="sub">${l?.invalid_sentences||0} 条未通过校验</span></h2><span class="label">${l&&!stale?'最近读取 '+esc(dateText(l.observed,true)):'等待刷新'}</span></div><details id="location-raw" ${locationUI.expanded?'open':''}><summary>查看模块原始语句</summary><pre>${esc(l&&!stale?l.nmea||'尚无 NMEA 输出':'未读取或数据已过期')}</pre></details><p class="location-footnote">位置与 NMEA 只在此页按需读取，不写入本地归档或通用诊断包。未加载外部地图，也未使用浏览器定位。启动不重启模块；停止仅关闭 GPS raw / NMEA，保留其他定位源。</p></section>`;
}
async function setLocation(enabled){
 if(locationUI.busy)return;
 const ui=locationUI,epoch=state.authEpoch;ui.busy=true;render(true);
 try{await api('/api/location/control','POST',{enabled});if(!state.logged||epoch!==state.authEpoch||ui!==locationUI)return;ui.data=null;ui.error='';toast(enabled?'GNSS 已启用，等待卫星定位':'GPS raw / NMEA 已停止');}
 catch(e){if(state.logged&&epoch===state.authEpoch&&ui===locationUI){ui.data=null;ui.error=e.message;toast(e.message,true);}}
 finally{ui.busy=false;if(state.logged&&epoch===state.authEpoch&&ui===locationUI){await loadLocation();render(true);}}
}
document.addEventListener('click',e=>{
 if(e.target.closest('#location-enable')&&!locationUI.busy)document.querySelector('#location-dialog').showModal();
 if(e.target.closest('#location-cancel'))document.querySelector('#location-dialog').close();
 if(e.target.closest('#location-confirm')){document.querySelector('#location-dialog').close();setLocation(true);}
 if(e.target.closest('#location-stop'))setLocation(false);
});

document.addEventListener('toggle',e=>{if(e.target.id==='location-raw')locationUI.expanded=e.target.open;},true);
