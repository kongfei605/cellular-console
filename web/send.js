'use strict';
const gsmCharacters="@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà";
const gsmExtra="^{}\\[~]|€";
let smsDraft={id:'',number:'',text:''},smsSubmitting=false;
function smsLabel(status){return ({pending:'等待处理',creating:'准备短信',ready:'准备发送',submitting:'正在提交',submitted:'设备已接受 · 待确认',sent:'已提交运营商',failed:'未发送',unknown:'结果待核实 · 勿重复发送'})[status]||'状态待确认';}
function smsEstimate(text){
 let units=0,encoding='GSM-7';
 if(!text.trim())return {error:'请输入短信内容'};
 for(const c of text){const code=c.codePointAt(0);if(code>0xffff||(code<32&&c!=='\n'&&c!=='\r')||(code>=127&&code<=159))return {error:'暂不支持表情、BMP以外字符或控制字符'};if(gsmCharacters.includes(c))units++;else if(gsmExtra.includes(c))units+=2;else encoding='UCS-2';}
 if(encoding==='UCS-2')units=[...text].length;
 const single=encoding==='GSM-7'?160:70,multi=encoding==='GSM-7'?153:67;
 let segments=1;
 if(units>single){if(encoding==='GSM-7'){let used=0;for(const c of text){const n=gsmExtra.includes(c)?2:1;if(used+n>multi){segments++;used=0;}used+=n;}}else segments=Math.ceil(units/multi);}
 return {units,encoding,segments,error:segments>3?'每次最多发送3段，请缩短内容':''};
}
function smsNewID(){if(!window.crypto?.getRandomValues)throw new Error('浏览器不支持安全请求标识');return [...crypto.getRandomValues(new Uint8Array(16))].map(b=>b.toString(16).padStart(2,'0')).join('');}
function resetSMSComposer(){
 smsDraft={id:'',number:'',text:''};smsSubmitting=false;
 for(const id of ['sms-dialog','sms-confirm-dialog']){const dialog=document.getElementById(id);if(dialog?.open)dialog.close();}
 for(const id of ['sms-number','sms-text']){const field=document.getElementById(id);if(field)field.value='';}
 for(const id of ['sms-summary-text','sms-summary-number','sms-compose-error']){const el=document.getElementById(id);if(el)el.textContent='';}
}
function openSMS(number=''){
 if(!state.status?.sms_send_allowed){toast(state.status?.sms_block_reason||'设备状态尚未就绪',true);return;}
 try{smsDraft={id:smsNewID(),number,text:''};}catch(e){toast(e.message,true);return;}
 $('#sms-number').value=number;$('#sms-text').value='';$('#sms-compose-error').textContent='';smsUpdateEstimate();$('#sms-dialog').showModal();(number?$('#sms-text'):$('#sms-number')).focus();
}
function smsUpdateEstimate(){
 const text=$('#sms-text').value.replace(/\r\n?/g,'\n'),info=smsEstimate(text);
 $('#sms-estimate').textContent=info.error||`${info.encoding} · ${info.units} 编码单位 · 预计 ${info.segments} 段`;
 $('#sms-next').disabled=!!info.error||smsSubmitting||!state.status?.sms_send_allowed;
}
document.addEventListener('click',e=>{
 const button=e.target.closest('[data-compose-sms]');if(button)openSMS(button.dataset.composeSms||'');
});
document.addEventListener('input',e=>{if(e.target.id==='sms-text'||e.target.id==='sms-number')smsUpdateEstimate();});
document.addEventListener('DOMContentLoaded',()=>{
 $('#sms-cancel').addEventListener('click',()=>$('#sms-dialog').close());
 $('#sms-edit').addEventListener('click',()=>{$('#sms-confirm-dialog').close();$('#sms-dialog').showModal();});
 $('#sms-form').addEventListener('submit',e=>{
  e.preventDefault();const number=$('#sms-number').value.trim().replace(/[ ()-]/g,''),text=$('#sms-text').value.replace(/\r\n?/g,'\n'),info=smsEstimate(text);
  if(!/^\+?[0-9]{3,20}$/.test(number)){$('#sms-compose-error').textContent='请输入3–20位单个收件号码，可使用+国际区号';$('#sms-number').focus();return;}
  if(info.error){$('#sms-compose-error').textContent=info.error;return;}
  smsDraft.number=number;smsDraft.text=text;$('#sms-summary-number').textContent=number;$('#sms-summary-text').textContent=text;
  $('#sms-summary-info').textContent=`${info.encoding} · 预计 ${info.segments} 段，实际计费以运营商为准。`;
  $('#sms-confirm-title').textContent=state.status.demo?'确认模拟发送短信':'确认发送短信';
  $('#sms-confirm-note').textContent=state.status.demo?'演示设备不会向任何真实号码发送短信。':'确认后将使用69上EC20的SIM发送，可能产生短信费用。';
  $('#sms-dialog').close();$('#sms-confirm-dialog').showModal();
 });
 $('#sms-send').addEventListener('click',async()=>{
  if(smsSubmitting)return;
  smsSubmitting=true;$('#sms-send').disabled=true;const epoch=state.authEpoch,submitted={...smsDraft};$('#sms-confirm-dialog').close();
  try{
   const result=await api('/api/sms','POST',{request_id:submitted.id,number:submitted.number,text:submitted.text});
   if(!state.logged||epoch!==state.authEpoch)return;
   state.selected=result.message.conversation_id;state.search='';state.unread=false;workspaceUI.trash=false;
   if(state.view!=='inbox')location.hash='inbox';
   toast(result.duplicate?'已找到同一请求的发送记录，未重复提交':'发送请求已保存，请在会话中查看状态');
   await loadConversations(false);render();
  }catch(e){if(state.logged&&epoch===state.authEpoch)toast(`${e.message}。连接异常时请先查看会话，勿直接重复发送。`,true);}
  finally{smsSubmitting=false;$('#sms-send').disabled=false;}
 });
});
