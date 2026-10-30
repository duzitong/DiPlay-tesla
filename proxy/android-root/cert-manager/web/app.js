'use strict';
const $ = id => document.getElementById(id);
let credentials = '', termsURL = '', timer, state, polling = false;
const labels = {pending:'等待 TXT 验证',processing:'CA 正在处理',ready:'验证通过，待签发',valid:'已签发',invalid:'申请无效，请重新生成',installed:'已安装'};
function notice(text, error = false) { $('notice').textContent = text; $('notice').classList.toggle('error', error); }
function logout() {
  credentials = ''; termsURL = ''; state = undefined; clearTimeout(timer);
  $('password').value = ''; $('accept').checked = false; $('terms-panel').hidden = true;
  $('dashboard').hidden = true; $('login-card').hidden = false; $('records').replaceChildren();
  notice('已退出。页面未保存管理密码。');
}
async function api(path, body) {
  const response = await fetch('/api/' + path, {method:body === undefined ? 'GET' : 'POST', cache:'no-store', credentials:'omit', redirect:'error', headers:{Authorization:credentials, ...(body === undefined ? {} : {'Content-Type':'application/json','X-Cert-Manager':'1'})}, ...(body === undefined ? {} : {body:JSON.stringify(body)})});
  const data = await response.json();
  if (!response.ok) {
    if (response.status === 401) logout();
    throw new Error(response.status === 401 ? '管理密码不正确。' : response.status === 429 ? '登录尝试过多，请一分钟后再试。' : data.error || '请求失败');
  }
  return data;
}
function date(value) { return new Date(value).toLocaleString('zh-CN', {hour12:false}); }
function recordField(parent, title, value) {
  const label = document.createElement('label'), code = document.createElement('code'), button = document.createElement('button');
  label.textContent = title; code.textContent = value; button.textContent = '复制' + title; button.type = 'button';
  button.addEventListener('click', async () => { try { await navigator.clipboard.writeText(value); notice(title + '已复制。'); } catch { const selection = getSelection(), range = document.createRange(); range.selectNodeContents(code); selection.removeAllRanges(); selection.addRange(range); notice('浏览器未允许复制，已选中文本，请长按复制。'); } });
  parent.append(label, code, button);
}
function render(s) {
  state = s; $('domain').textContent = s.domain; $('days').textContent = s.daysRemaining;
  $('expiry').textContent = date(s.notAfter); $('health').textContent = s.busy ? '处理中' : s.renewRecommended ? '建议续期' : '有效';
  $('prepare').disabled = s.busy || !s.renewRecommended || !termsURL || !$('accept').checked;
  $('terms').disabled = s.busy; $('complete').disabled = s.busy || !s.order || ['installed','invalid'].includes(s.order.status);
  $('install').disabled = s.busy || !s.pendingCertificate;
  $('pending').textContent = s.pendingCertificate ? 'V50 已保存待安装证书；安装前会重新检查域名、私钥匹配和信任链。' : '还没有待安装证书。';
  $('order-status').textContent = s.order ? `${labels[s.order.status] || s.order.status} · 申请截止 ${date(s.order.expires)}` : '尚未生成申请。';
  const records = s.order?.records || [], signature = JSON.stringify(records);
  if ($('records').dataset.signature !== signature) {
    $('records').replaceChildren(); $('records').dataset.signature = signature;
    for (const r of records) {
      const box = document.createElement('div'); box.className = 'record';
      recordField(box, '完整名称', r.name);
      // This UI is for a host below a registrable domain; DNS consoles may use
      // different zone boundaries. Full FQDN remains the authoritative value.
      recordField(box, '主机记录', r.name.split('.').slice(0, -2).join('.'));
      recordField(box, 'TXT 值', r.value); $('records').append(box);
    }
  }
  $('no-records').hidden = !s.order || records.length !== 0 || ['installed','invalid'].includes(s.order.status);
  notice(s.message);
}
async function refresh() {
  if (!credentials || polling) return;
  polling = true; clearTimeout(timer);
  try { render(await api('status')); } catch (e) { notice(e.message, true); }
  finally { polling = false; if (credentials) timer = setTimeout(refresh, state?.busy ? 2000 : 15000); }
}
async function act(name, body) {
  for (const id of ['prepare','complete','install']) $(id).disabled = true;
  try { await api(name, body); await refresh(); } catch (e) { if (state) render(state); notice(e.message, true); }
}
$('login-form').addEventListener('submit', async e => {
  e.preventDefault(); const password = $('password').value.trim();
  if (!/^[A-Za-z0-9_-]{43}$/.test(password)) { notice('请输入首次部署保存的 43 位管理密码。', true); return; }
  credentials = 'Basic ' + btoa('admin:' + password); $('password').value = '';
  try { render(await api('status')); $('login-card').hidden = true; $('dashboard').hidden = false; await refresh(); }
  catch (e) { credentials = ''; notice(e.message, true); }
});
$('logout').addEventListener('click', logout);
$('refresh').addEventListener('click', refresh);
$('accept').addEventListener('change', () => { if (state) render(state); });
$('terms').addEventListener('click', async () => {
  $('terms').disabled = true;
  try { const data = await api('terms'); const url = new URL(data.termsURL); if (url.protocol !== 'https:') throw new Error('条款地址无效'); termsURL = url.href; $('terms-link').href = termsURL; $('terms-panel').hidden = false; $('accept').checked = false; if (state) render(state); }
  catch (e) { notice(e.message, true); } finally { $('terms').disabled = false; }
});
$('prepare').addEventListener('click', () => act('prepare', {termsURL, acceptTerms:$('accept').checked}));
$('complete').addEventListener('click', () => act('complete', {}));
$('install').addEventListener('click', () => { if (confirm('确认安装新证书并重启 HTTPS 网关？车机浏览器连接可能短暂中断，失败时会尝试恢复旧证书。')) act('install', {confirmRestart:true}); });