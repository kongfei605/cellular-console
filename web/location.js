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
function locationView(){
 const l=locationUI.data,freshNetwork=state.status.network.fresh,stale=!l||performance.now()-locationUI.received>35000,valid=!!(l?.fix&&l.position&&!stale&&freshNetwork);
 const on=l?.gnss_on,external=!!(l?.enabled_sources&16),blocked=!l?.enable_allowed||stale||!freshNetwork||locationUI.busy;
 const label=!l?'等待读取':external?'外部管理':!l.available?'接口不可用':!on?'定位已停止':valid?'已获得定位':'正在搜星';
 const coordinate=(name,value,unit)=>`<div class="location-coordinate"><span>${name}</span><strong>${value===undefined?'—':esc(Number(value).toFixed(6))}<small>${unit}</small></strong></div>`;
 const sourceNames=mask=>[[1,'3GPP 小区'],[2,'GPS raw'],[4,'GPS NMEA'],[16,'非托管 GPS']].filter(([bit])=>mask&bit).map(([,name])=>name).join(' · ')||'无';
 return heading('卫星定位','查看 EC20 的 GNSS 数据与定位状态。','GNSS / SATELLITE POSITIONING')+
 (locationUI.error?`<div class="error-banner" role="alert">${esc(locationUI.error)}；已隐藏先前位置。请使用右上角刷新重试。</div>`:'')+
 `<section class="panel location-summary"><div class="summary-icon">${icon('location')}</div><div><div class="eyebrow">EC20 · GNSS</div><h2>${label}</h2><p>${esc(stale&&l?'定位数据未刷新，已隐藏先前位置':l?.fix_reason||'正在读取模块定位接口…')}</p></div><div class="location-actions"><button id="location-enable" class="button primary" ${on||external||blocked?'disabled':''}>${locationUI.busy?'正在设置…':'启用 GNSS'}</button><button id="location-stop" class="button secondary" ${!on||!l?.control_enabled||locationUI.busy?'disabled':''}>停止定位</button></div></section>`+
 (!on&&l?.control_reason?`<div class="notice amber">${icon('shield')}${esc(l.control_reason)}</div>`:'')+
 (l?.demo?'<div class="notice amber">演示模式：坐标为公开 NMEA 示例数据，未访问真实硬件。</div>':'')+
 `<div class="location-layout"><section class="panel location-position"><div class="panel-heading"><h2>当前位置<span class="sub">WGS84</span></h2>${pill(valid?'有效定位':on?'等待定位':'未启用',valid?'green':'amber',valid)}</div><div class="location-position-body ${valid?'has-fix':''}"><div class="location-radar" aria-hidden="true"><i></i><i></i><i></i><span>${icon('location')}</span></div><h3>${valid?'卫星定位已解算':on?'等待卫星解算':'开始接收卫星信号'}</h3><p>${valid?'仅展示校验通过且 90 秒内的定位数据。':on?'已启用不代表已定位。请保持 GNSS 天线连接，并尽量提供开阔天空视野。':'手动启用后，模块将通过专用 GNSS 端口输出定位信息。'}</p></div><div class="location-coordinates">${coordinate('纬度 LATITUDE',valid?l.position.latitude:undefined,'°')}${coordinate('经度 LONGITUDE',valid?l.position.longitude:undefined,'°')}</div><dl class="device-info"><div class="info-row"><dt>海拔</dt><dd>${valid&&l.position.altitude!==undefined?esc(l.position.altitude.toFixed(1))+' m':'—'}</dd></div><div class="info-row"><dt>卫星时间 UTC</dt><dd class="mono">${valid?esc(l.position.utc.replace('T',' ').replace('Z',' UTC')):'等待有效时间'}</dd></div></dl></section><div><section class="panel location-observation"><div class="panel-heading"><h2>接收观测</h2><span class="label">NMEA</span></div><div class="location-metrics"><div><span>参与定位</span><strong>${on&&!stale?l?.satellites_used??'—':'—'}<small>颗</small></strong></div><div><span>报告可见</span><strong>${on&&!stale?l?.satellites_visible??'—':'—'}<small>颗</small></strong></div><div><span>水平精度因子</span><strong>${on&&!stale&&l?.hdop!==undefined?esc(l.hdop.toFixed(1)):'—'}<small>HDOP</small></strong></div><div><span>校验通过语句</span><strong>${on&&!stale?l?.sentences??0:'—'}<small>条</small></strong></div></div><p class="location-footnote">卫星数量来自模块缓存的 GGA / GSV 报告，不能单独证明已定位。坐标需通过 RMC 状态、校验和及卫星时间核验。</p></section><section class="panel location-configuration"><div class="panel-heading"><h2>定位配置</h2>${pill(l?.reviewed_profile?'本机配置已验证':'待验证',l?.reviewed_profile?'green':'amber')}</div><dl class="device-info"><div class="info-row"><dt>支持数据源</dt><dd>${esc(sourceNames(l?.capabilities||0))}</dd></div><div class="info-row"><dt>已启用数据源</dt><dd>${esc(sourceNames(l?.enabled_sources||0))}</dd></div><div class="info-row"><dt>位置广播</dt><dd>${l?.signals?'外部已开启':'关闭 · 登录后按需读取'}</dd></div><div class="info-row"><dt>模块缓存刷新</dt><dd>${l?esc(l.refresh_seconds)+' 秒':'—'}</dd></div></dl></section></div></div>`+
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
