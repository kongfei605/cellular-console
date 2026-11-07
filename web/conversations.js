'use strict';

function highlightMessage(text,query){
 if(!query.trim())return esc(text);
 const needle=query.trim().toLowerCase(),lower=String(text).toLowerCase();let pos=0,result='',at;
 while((at=lower.indexOf(needle,pos))!==-1){result+=esc(text.slice(pos,at))+'<mark>'+esc(text.slice(at,at+needle.length))+'</mark>';pos=at+needle.length;}
 return result+esc(text.slice(pos));
}
function dayLabel(value){
 const d=parseTimestamp(value);if(!Number.isFinite(d.getTime()))return '时间未知';
 return new Intl.DateTimeFormat('zh-CN',{timeZone:'Asia/Shanghai',year:'numeric',month:'long',day:'numeric'}).format(d);
}
function clockLabel(value){
 const d=parseTimestamp(value);if(!Number.isFinite(d.getTime()))return '—';
 return new Intl.DateTimeFormat('zh-CN',{timeZone:'Asia/Shanghai',hour:'2-digit',minute:'2-digit'}).format(d);
}
function conversationView(){
 const threads=state.conversations,detail=state.detail,chosen=detail?.conversation;
 let previousDay='';
 const bubbles=(detail?.messages||[]).map(m=>{
  const date=m.timestamp||m.first_seen,day=dayLabel(date),separator=day!==previousDay?`<div class="conversation-day"><span>${day}</span></div>`:'';previousDay=day;
  const outgoing=m.direction==='outgoing';
  return separator+`<article class="message-bubble ${outgoing?'outgoing':''}"><div class="message-bubble-text">${highlightMessage(m.text,state.search)}</div><div class="message-bubble-meta"><time>${clockLabel(date)}</time>${outgoing?`<span class="sms-send-state ${m.send_status==='failed'||m.send_status==='unknown'?'attention':''}" title="${esc(m.send_error||'已提交运营商不代表对方已收到')}">${esc(smsLabel(m.send_status))}</span>`:''}${m.sender!==chosen.sender?`<span title="原始发送方号码">${esc(m.sender)}</span>`:''}<button class="bubble-copy icon-button" data-copy-message="${m.id}" aria-label="复制这条短信" title="复制这条短信">${icon('copy')}</button></div>${outgoing&&m.send_error&&['failed','unknown'].includes(m.send_status)?`<p class="sms-send-error">${esc(m.send_error)}</p>`:''}</article>`;
 }).join('');
 return heading('短信会话','收发短信按号码归档，历史记录始终保存在 69 本地。','MESSAGING / CONVERSATIONS')+
 `<section class="panel conversation-panel"><div class="inbox-toolbar"><label class="search-box" for="search">${icon('search')}<input id="search" type="search" aria-label="搜索号码或短信内容" placeholder="搜索号码或短信内容" value="${esc(state.search)}" autocomplete="off"></label><button class="filter-toggle ${!state.unread?'active':''}" data-filter="all">全部</button><button class="filter-toggle ${state.unread?'active':''}" data-filter="unread">未读会话</button><button class="filter-toggle" id="sms-trash">${workspaceUI.trash?'返回会话':'回收站'}</button><button class="button primary sms-new" data-compose-sms="" ${!state.status.sms_send_allowed?'disabled':''}>新建短信</button><span class="inbox-count">${threads.length} 个会话 · ${state.status.message_count} 条短信</span></div><div class="conversation-layout"><div class="conversation-list">${threads.length?threads.map(c=>`<button class="conversation-item ${state.selected===c.id?'selected':''}" data-conversation="${c.id}" aria-label="${esc(c.sender)}，${c.count} 条短信，${c.unread} 条未读"><span class="sender-avatar">${esc(c.sender.slice(-2)||'信')}</span><span class="conversation-summary"><span class="conversation-row"><strong>${esc(personName(c.sender))}</strong><time>${dateText(c.last_at,true)}</time></span><span class="conversation-preview">${highlightMessage(c.preview,state.search)}</span><span class="conversation-row conversation-bottom"><small>${c.count} 条消息${state.search?` · ${c.matches} 条匹配`:''}</small>${c.unread?`<span class="unread-count">${c.unread}</span>`:''}</span></span></button>`).join(''):empty('mail',state.search?'没有匹配的会话':state.unread?'没有未读会话':'收件箱还是空的',state.search?'换一个号码或关键词试试。':'新短信到达后会自动归档。')}</div><section class="conversation-detail">${chosen?`<header class="conversation-header"><div class="conversation-person"><span class="sender-avatar">${icon('mail')}</span><div><h2>${esc(personName(chosen.sender))}</h2><p>${esc(chosen.sender)} · ${chosen.count} 条消息 · ${chosen.unread?`${chosen.unread} 条未读`:'全部已读'}</p></div></div><div class="conversation-tools"><button class="text-action" id="archive-thread">${workspaceUI.trash?'恢复':'移入回收站'}</button>${/^\+?[0-9]{3,20}$/.test(chosen.sender)&&!contactFor(chosen.sender)?`<button class="text-action" data-save-person="${esc(chosen.sender)}">存为联系人</button>`:''}${chosen.unread?'<button id="read-conversation" class="button secondary">全部标为已读</button>':''}${/^\+?[0-9]{3,20}$/.test(chosen.sender)?`<button class="button secondary" data-compose-sms="${esc(chosen.sender)}" ${!state.status.sms_send_allowed?'disabled':''}>回复短信</button>`:pill('本地归档','green')}</div></header><div class="conversation-messages" data-thread-id="${chosen.id}">${bubbles}</div><div class="conversation-footer">${icon('shield')}${esc(state.status.sms_send_allowed?'有线主用 · 可发送短信，最多3段；已提交不代表送达':state.status.sms_block_reason||'暂时不能发送短信')}</div>`:empty('mail',threads.length?'选择一个会话':'暂无会话内容','消息到达后会自动显示在这里。')}</section></div></section>`;
}

let conversationRequest=0;
async function loadConversations(autoRead=false){
 const requestID=++conversationRequest,query=state.search,unread=state.unread,epoch=state.authEpoch;
 const params=new URLSearchParams();if(query)params.set('q',query);if(unread)params.set('unread','1');if(workspaceUI.trash)params.set('trash','1');
 const conversations=await api('/api/conversations'+(params.size?'?'+params.toString():''));
 if(!state.logged||requestID!==conversationRequest||epoch!==state.authEpoch)return;
 state.conversations=conversations;
 if(!conversations.some(c=>c.id===state.selected))state.selected=conversations[0]?.id||null;
 const selected=state.selected;
 if(!selected){state.detail=null;return;}
 const detail=await api('/api/conversations/'+selected);
 if(!state.logged||requestID!==conversationRequest||epoch!==state.authEpoch||selected!==state.selected)return;
 state.detail=detail;
 if(autoRead&&state.view==='inbox'&&detail.conversation.unread>0)await readConversation(selected);
}
async function readConversation(id){
 const detail=state.detail;if(!detail||detail.conversation.id!==id||!detail.conversation.unread)return;
 const epoch=state.authEpoch;
 const result=await api('/api/conversations/'+id+'/read','POST');
 if(epoch!==state.authEpoch||!state.logged)return;
 if(state.detail?.conversation.id===id){state.detail.messages.forEach(m=>m.read=true);state.detail.conversation.unread=0;}
 const thread=state.conversations.find(c=>c.id===id);if(thread)thread.unread=0;
 if(state.status)state.status.unread_count=result.unread_count;
}
