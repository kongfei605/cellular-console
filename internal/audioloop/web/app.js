'use strict';
const $ = id => document.getElementById(id);
let csrf = '', pc = null, context = null, input = null, oscillator = null, localAnalyser = null, returnAnalyser = null;
let peerID = '', tick = null, deadline = null, starting = false, mode = '', startedAt = 0, goodSamples = 0, updating = false;
let generation = 0;
let last = {mode: '', sent: 0, received: 0, server_received: 0, server_reflected: 0, frequency_hz: 0, return_rms: 0, verified: false};
const remote = $('remote');
remote.volume = Number($('volume').value);

async function api(path, method = 'GET', body) {
  const response = await fetch(path, {method, credentials: 'same-origin', headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf}, body: body === undefined ? undefined : JSON.stringify(body)});
  const data = await response.json();
  if (!response.ok) {
    if (response.status === 401 && path !== '/api/login') { $('lab').hidden = true; $('loginPanel').hidden = false; $('logout').hidden = true; }
    throw new Error(data.error || `请求未完成 (${response.status})`);
  }
  return data;
}
function showLab(session) {
  csrf = session.csrf;
  $('loginPanel').hidden = true; $('lab').hidden = false; $('logout').hidden = false;
  $('secure').textContent = window.isSecureContext ? 'HTTPS · 安全上下文' : '需要安全上下文';
  if (!window.isSecureContext || !window.RTCPeerConnection || !window.AudioContext) {
    $('message').textContent = '此浏览器需要受信任的 HTTPS、WebRTC 与 Web Audio 支持。';
    $('mic').disabled = $('tone').disabled = true;
  }
}
$('loginForm').addEventListener('submit', async event => {
  event.preventDefault(); $('loginError').textContent = '';
  const button = event.submitter; button.disabled = true;
  try { showLab(await api('/api/login', 'POST', {username: $('username').value, password: $('password').value})); $('password').value = ''; await restorePeer(); }
  catch (e) { $('loginError').textContent = e.message; }
  finally { button.disabled = false; }
});
$('logout').addEventListener('click', async () => {
  await stop('已停止回环');
  try { await api('/api/logout', 'POST', {}); } catch (e) { $('message').textContent = e.message; }
  csrf = ''; $('lab').hidden = true; $('loginPanel').hidden = false; $('logout').hidden = true;
});
async function restorePeer() {
  const state = await api('/api/loopback');
  if (state.active && state.owned) {
    peerID = state.id; $('stop').disabled = false; $('mic').disabled = $('tone').disabled = true;
    $('state').textContent = '上次会话待释放'; $('message').textContent = '检测到本浏览器的未结束会话，点击停止后可重新开始。';
  } else if (state.active) { $('message').textContent = '另一个浏览器正在使用回环，结束后可开始。'; }
}
function analyser(stream) {
  const source = context.createMediaStreamSource(stream), node = context.createAnalyser();
  node.fftSize = 4096; source.connect(node); return node;
}
function signal(node) {
  if (!node || !context) return {rms: 0, frequency: 0};
  const data = new Float32Array(node.fftSize); node.getFloatTimeDomainData(data);
  const rms = Math.sqrt(data.reduce((sum, x) => sum + x*x, 0) / data.length);
  const bins = new Float32Array(node.frequencyBinCount); node.getFloatFrequencyData(bins);
  const scale = context.sampleRate / node.fftSize;
  let peak = 0, amplitude = -Infinity;
  for (let i = Math.ceil(100/scale); i < Math.min(bins.length, Math.ceil(2000/scale)); i++) if (bins[i] > amplitude) { amplitude = bins[i]; peak = i; }
  return {rms, frequency: rms > .005 ? Math.round(peak*scale) : 0};
}
function level(rms) { return rms > .0001 ? `${Math.round(20*Math.log10(rms))} dBFS` : '静音'; }
function waitForICE(connection) {
  if (connection.iceGatheringState === 'complete') return Promise.resolve();
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => { connection.removeEventListener('icegatheringstatechange', changed); reject(new Error('本地网络候选收集超时')); }, 8000);
    function changed() { if (connection.iceGatheringState === 'complete') { clearTimeout(timeout); connection.removeEventListener('icegatheringstatechange', changed); resolve(); } }
    connection.addEventListener('icegatheringstatechange', changed);
  });
}
async function start(sourceMode) {
  if (starting || pc) return;
  const ticket = ++generation;
  starting = true; $('mic').disabled = $('tone').disabled = true;
  $('stop').disabled = false;
  $('state').textContent = '正在建立回环'; $('message').textContent = sourceMode === 'mic' ? '请允许本站使用麦克风。' : '正在生成测试音，并通过 69 回传。';
  $('result').className = 'result'; $('result').textContent = '验证中'; goodSamples = 0;
  last = {mode: sourceMode, sent: 0, received: 0, server_received: 0, server_reflected: 0, frequency_hz: 0, return_rms: 0, verified: false};
  try {
    context = new AudioContext(); await context.resume();
    if (ticket !== generation) return;
    mode = sourceMode;
    if (mode === 'mic') {
      if (!navigator.mediaDevices?.getUserMedia) throw new Error('麦克风接口不可用，请检查 HTTPS 与浏览器权限');
      const acquired = await navigator.mediaDevices.getUserMedia({video: false, audio: {echoCancellation: true, noiseSuppression: true, autoGainControl: true}});
      if (ticket !== generation) { acquired.getTracks().forEach(track => track.stop()); return; }
      input = acquired;
    } else {
      const destination = context.createMediaStreamDestination(), gain = context.createGain();
      oscillator = context.createOscillator(); oscillator.frequency.value = 440; gain.gain.value = .18;
      oscillator.connect(gain); gain.connect(destination); oscillator.start(); input = destination.stream;
    }
    localAnalyser = analyser(input);
    const connection = new RTCPeerConnection({iceServers: [], bundlePolicy: 'max-bundle'}); pc = connection;
    connection.ontrack = event => {
      const stream = event.streams[0] || new MediaStream([event.track]);
      remote.srcObject = stream; returnAnalyser = analyser(stream);
      remote.play().catch(() => { $('message').textContent = '返回音频已到达，但浏览器暂停了播放。点击音量控制后重试，或允许本站自动播放。'; });
      $('playLabel').textContent = '接收来自 69 的音频';
    };
    connection.onconnectionstatechange = () => {
      $('connection').textContent = connection.connectionState;
      if (connection.connectionState === 'connected') $('state').textContent = '回环运行中';
      if (pc === connection && ['failed', 'disconnected'].includes(connection.connectionState)) void stop('连接中断，回环已停止');
    };
    const sender = connection.addTrack(input.getAudioTracks()[0], input);
    const transceiver = connection.getTransceivers().find(t => t.sender === sender);
    const codecs = RTCRtpSender.getCapabilities('audio').codecs.filter(c => c.mimeType.toLowerCase() === 'audio/pcmu' && c.clockRate === 8000);
    if (!codecs.length || !transceiver.setCodecPreferences) throw new Error('当前浏览器无法协商 PCMU/8000 音频');
    transceiver.setCodecPreferences(codecs);
    await connection.setLocalDescription(await connection.createOffer()); await waitForICE(connection);
    if (ticket !== generation) return;
    const result = await api('/api/loopback', 'POST', {type: connection.localDescription.type, sdp: connection.localDescription.sdp});
    if (ticket !== generation) { await api('/api/loopback/stop', 'POST', {id: result.id}); return; }
    peerID = result.id;
    await connection.setRemoteDescription(result.answer);
    if (ticket !== generation) return;
    startedAt = Date.now(); $('stop').disabled = false;
    $('sourceLabel').textContent = mode === 'mic' ? '麦克风 · 浏览器降噪' : '440Hz 正弦测试音';
    $('message').textContent = mode === 'mic' ? '请对麦克风说话，确认耳机中能听到返回声音。' : '测试会检查返回信号的频率与双向数据包，20 秒后自动停止。';
    tick = setInterval(() => void update(), 700);
    deadline = setTimeout(() => void stop('已达到本次测试时限，回环已停止'), mode === 'tone' ? 20000 : 175000);
    await update();
  } catch (e) {
    if (ticket !== generation) return;
    const detail = e.name === 'NotAllowedError' ? '麦克风权限未获允许，请在浏览器中允许后重试。' : e.name === 'NotFoundError' ? '未发现可用麦克风，可先运行测试音。' : e.message;
    await stop(detail); $('result').textContent = '本次测试未完成';
  } finally { if (ticket === generation) starting = false; if (!starting && !pc && !peerID) $('mic').disabled = $('tone').disabled = false; }
}
async function update() {
  if (!pc || updating) return; updating = true; const connection = pc;
  try {
    const stats = await connection.getStats();
    let sent = 0, received = 0, rtt = null;
    stats.forEach(s => {
      if (s.type === 'outbound-rtp' && s.kind === 'audio') sent += s.packetsSent || 0;
      if (s.type === 'inbound-rtp' && s.kind === 'audio') received += s.packetsReceived || 0;
      if (s.type === 'candidate-pair' && s.state === 'succeeded' && s.nominated && s.currentRoundTripTime !== undefined) rtt = s.currentRoundTripTime;
    });
    if (pc !== connection) return;
    const send = signal(localAnalyser), back = signal(returnAnalyser), server = await api('/api/loopback');
    if (pc !== connection) return;
    if (!server.active || !server.owned || server.id !== peerID) { await stop('服务器已释放回环，请重新开始'); return; }
    last = {...last, sent, received, server_received: server.received, server_reflected: server.reflected, frequency_hz: back.frequency, return_rms: Number(back.rms.toFixed(5))};
    $('inputLevel').textContent = level(send.rms); $('returnLevel').textContent = level(back.rms);
    $('inputMeter').value = Math.min(1, send.rms*5); $('returnMeter').value = Math.min(1, back.rms*5);
    $('packets').textContent = `${sent} / ${received}`; $('serverPackets').textContent = `${server.received} / ${server.reflected}`;
    $('rtt').textContent = rtt === null ? '—' : `${Math.round(rtt*1000)} ms`;
    $('signal').textContent = back.frequency ? `${back.frequency} Hz · ${level(back.rms)}` : mode === 'mic' && received > 10 ? `当前音量较低 · ${level(back.rms)}` : '尚无有效返回信号';
    $('remaining').textContent = `剩余 ${Math.max(0, (mode === 'tone' ? 20 : 175) - Math.floor((Date.now()-startedAt)/1000))} 秒`;
    if (mode === 'tone' && sent > 10 && received > 10 && server.reflected > 10 && back.frequency > 400 && back.frequency < 480 && back.rms > .015) goodSamples++;
    else if (mode === 'tone') goodSamples = 0;
    if (goodSamples >= 3) {
      last.verified = true; $('result').className = 'result pass'; $('result').textContent = '回环验证通过 · 440Hz 测试音已经过 69 返回浏览器';
    } else if (mode === 'mic' && received > 10 && back.rms > .005) {
      $('result').className = 'result pass'; $('result').textContent = '双向音频数据已连通 · 已检测到返回声音，请佩戴耳机确认听感';
    }
  } catch (e) { if (pc === connection) await stop(`检查未完成：${e.message}`); }
  finally { updating = false; }
}
async function stop(message = '回环已停止') {
  generation++; starting = false;
  clearInterval(tick); clearTimeout(deadline); tick = deadline = null;
  const id = peerID; peerID = '';
  const connection = pc; pc = null; if (connection) { connection.onconnectionstatechange = null; connection.close(); }
  input?.getTracks().forEach(track => track.stop()); input = null;
  if (oscillator) { try { oscillator.stop(); } catch {} oscillator = null; }
  if (context) { const ctx = context; context = null; await ctx.close(); }
  localAnalyser = returnAnalyser = null; remote.srcObject = null;
  $('inputMeter').value = $('returnMeter').value = 0;
  $('inputLevel').textContent = $('returnLevel').textContent = '已停止';
  $('state').textContent = '回环已停止'; $('connection').textContent = '已关闭'; $('remaining').textContent = '媒体资源已释放';
  $('playLabel').textContent = '播放已停止'; $('message').textContent = message; $('stop').disabled = true;
  if ($('result').textContent === '验证中') $('result').textContent = '本次测试已停止，尚未完成验证';
  if (id) { try { await api('/api/loopback/stop', 'POST', {id}); } catch { $('message').textContent += '；服务端将在超时后释放会话。'; } }
  if (!starting) $('mic').disabled = $('tone').disabled = false;
}
$('mic').addEventListener('click', () => void start('mic'));
$('tone').addEventListener('click', () => void start('tone'));
$('stop').addEventListener('click', () => void stop());
$('muted').addEventListener('change', () => { remote.muted = $('muted').checked; });
$('volume').addEventListener('input', () => { remote.volume = Number($('volume').value); if (remote.srcObject) void remote.play().catch(() => {}); });
$('copy').addEventListener('click', async () => {
  const report = {version: '0.1.0', mode: 'software-loopback', secure_context: window.isSecureContext, codec: 'PCMU/8000/1', ...last, checked_at: new Date().toISOString()};
  try { await navigator.clipboard.writeText(JSON.stringify(report, null, 2)); $('message').textContent = '诊断已复制，不含账号、密码或音频内容。'; } catch { $('message').textContent = '浏览器未允许复制，请查看页面中的验证结果。'; }
});
window.addEventListener('pagehide', () => {
  input?.getTracks().forEach(track => track.stop()); pc?.close();
  if (peerID) void fetch('/api/loopback/stop', {method: 'POST', credentials: 'same-origin', keepalive: true, headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf}, body: JSON.stringify({id: peerID})});
});
(async () => { try { showLab(await api('/api/session')); await restorePeer(); } catch {} })();
