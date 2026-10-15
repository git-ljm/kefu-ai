// 客服 AI 助手前端（原生 JS，无需构建）
const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path, opts = {}) {
  const o = { ...opts };
  if (o.json !== undefined) {
    o.body = JSON.stringify(o.json);
    o.headers = { 'Content-Type': 'application/json' };
    delete o.json;
  }
  const res = await fetch(path, o);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `请求失败 (${res.status})`);
  return data;
}

function toast(msg, isErr = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.className = 'toast show' + (isErr ? ' error' : '');
  clearTimeout(t._h);
  t._h = setTimeout(() => (t.className = 'toast'), isErr ? 5000 : 2200);
}

async function busy(btn, fn) {
  const old = btn.innerHTML;
  btn.disabled = true;
  btn.classList.add('loading');
  try { return await fn(); }
  catch (e) { toast(e.message, true); }
  finally { btn.disabled = false; btn.classList.remove('loading'); btn.innerHTML = old; }
}

const fmtTime = (ts) => {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
};

// ================= 页面切换 =================
$$('.tab').forEach((t) => t.addEventListener('click', () => {
  $$('.tab').forEach((x) => x.classList.toggle('active', x === t));
  $$('.page').forEach((p) => p.classList.toggle('active', p.id === 'page-' + t.dataset.page));
  ({ docs: loadDocs, media: () => loadMedia(), stats: loadStats, settings: loadSettings })[t.dataset.page]?.();
}));

// ================= AI 状态 =================
async function checkStatus() {
  const el = $('#aiStatus');
  try {
    const s = await api('/api/status');
    const w = $('#dataWarn');
    if (s.other_data_dir && localStorage.getItem('ignoreOtherDir') !== s.other_data_dir) {
      w.innerHTML = `⚠️ 发现电脑上还有另一份数据：<code class="path">${esc(s.other_data_dir)}</code>
        （现在使用的是 <code class="path">${esc(s.data_dir)}</code>）。如果找不到以前的客户，多半在那份数据里。
        处理方法：关闭本程序 → 用那份数据旁边的程序打开，或把那个 kefu-data 文件夹复制过来替换。
        <button class="btn small" id="btnIgnoreDir">知道了，不再提示</button>`;
      w.classList.remove('hidden');
      $('#btnIgnoreDir').onclick = () => { localStorage.setItem('ignoreOtherDir', s.other_data_dir); w.classList.add('hidden'); };
    }
    const c = s.checks;
    if (c.chat && c.vision && c.embed) { el.textContent = '● AI 已就绪'; el.className = 'ai-status ok'; }
    else if (c.chat) { el.textContent = '● AI 可用（部分模型未安装，见设置）'; el.className = 'ai-status'; }
    else { el.textContent = '● AI 未连接，请到设置检查'; el.className = 'ai-status bad'; }
  } catch { el.textContent = '● AI 未连接'; el.className = 'ai-status bad'; }
}

// ================= 工作台 =================
const state = { customers: [], current: null, messages: [], images: [], pendingMsg: null, suggestion: null };
const showZh = () => $('#toggleZh').checked;
$('#toggleZh').checked = localStorage.getItem('showZh') !== '0';
$('#toggleZh').addEventListener('change', () => {
  localStorage.setItem('showZh', showZh() ? '1' : '0');
  renderMessages();
  renderReplyZh();
});

async function loadCustomers() {
  state.customers = await api('/api/customers');
  renderCustomers();
}

function renderCustomers() {
  const q = $('#customerSearch').value.trim().toLowerCase();
  const list = state.customers.filter((c) => !q || (c.name + c.platform + c.tags + c.notes).toLowerCase().includes(q));
  $('#customerList').innerHTML = list.map((c) => `
    <li data-id="${c.id}" class="${state.current?.id === c.id ? 'active' : ''}">
      <div class="c-name"><span>${esc(c.name)}${c.last_role === 'customer' ? '<span class="pending">待回复</span>' : ''}</span><span class="badge">${esc(c.platform)}</span></div>
      <div class="c-last">${esc(c.last_message || '暂无消息')}</div>
    </li>`).join('') || (q
      ? `<li class="muted">没有找到匹配「${esc($('#customerSearch').value.trim())}」的客户<br><button class="btn small" data-clear-search>清除搜索，显示全部客户</button></li>`
      : '<li class="muted">还没有客户，点上面按钮新建</li>');
  $('#btnClearSearch').classList.toggle('hidden', !q);
  $('#welcome').classList.toggle('hidden', !!state.current);
  // 已有客户时，欢迎页提示从左侧选择，避免误以为数据丢了而重复新建
  $('#welcomeHasCustomers').classList.toggle('hidden', !state.customers.length);
  $('#welcomeGuide').classList.toggle('hidden', state.customers.length > 0);
}
// 搜索框只接受“用户亲手输入”的内容：浏览器自动填充（密码管理器把保存的用户名填进来）一律清掉
let searchTyped = false;
function isAutofilled(el) {
  for (const sel of [':autofill', ':-webkit-autofill']) {
    try { if (el.matches(sel)) return true; } catch { /* 旧浏览器不认识这个选择器 */ }
  }
  return false;
}
$('#customerSearch').addEventListener('keydown', () => { searchTyped = true; });
$('#customerSearch').addEventListener('paste', () => { searchTyped = true; });
$('#customerSearch').addEventListener('input', (e) => {
  const el = e.target;
  if (!searchTyped || isAutofilled(el) || e.inputType === undefined || e.inputType === 'insertReplacementText') {
    if (el.value) { el.value = ''; }
  }
  if (!el.value) searchTyped = false;
  renderCustomers();
});
// Chrome 在用户第一次点击页面后才把自动填充的值交给页面，这里再兜底检查一次
['pointerdown', 'focusin'].forEach((ev) => document.addEventListener(ev, () => setTimeout(() => {
  const el = $('#customerSearch');
  if (el.value && !searchTyped) { el.value = ''; renderCustomers(); }
}, 50), true));
function clearSearch() { $('#customerSearch').value = ''; searchTyped = false; renderCustomers(); }
$('#btnClearSearch').addEventListener('click', clearSearch);
$('#customerList').addEventListener('click', (e) => {
  if (e.target.closest('[data-clear-search]')) { clearSearch(); return; }
  const li = e.target.closest('li[data-id]');
  if (li) openCustomer(+li.dataset.id);
});

$('#btnNewCustomer').addEventListener('click', () => {
  $('#ncName').value = ''; $('#ncPlatform').value = localStorage.getItem('lastPlatform') || ''; $('#ncNotes').value = '';
  $('#dlgCustomer').showModal();
  $('#ncName').focus();
});
$('#dlgCustomer').addEventListener('close', async () => {
  if ($('#dlgCustomer').returnValue !== 'ok') return;
  const name = $('#ncName').value.trim();
  const same = state.customers.find((c) => c.name.trim().toLowerCase() === name.toLowerCase());
  if (same && confirm(`已经有一个叫「${same.name}」的客户（${same.last_message ? '有聊天记录' : '暂无消息'}）。\n\n点「确定」打开已有客户，点「取消」仍然新建一个。`)) {
    openCustomer(same.id);
    return;
  }
  try {
    const c = await api('/api/customers', { method: 'POST', json: { name: $('#ncName').value, platform: $('#ncPlatform').value, notes: $('#ncNotes').value } });
    localStorage.setItem('lastPlatform', $('#ncPlatform').value);
    await loadCustomers();
    openCustomer(c.id);
  } catch (e) { toast(e.message, true); }
});

async function openCustomer(id) {
  let data;
  try {
    data = await api('/api/customers/' + id);
  } catch (e) {
    toast('打开客户失败：' + e.message, true);
    if (localStorage.getItem('currentCustomer') === String(id)) localStorage.removeItem('currentCustomer');
    return;
  }
  // 记住当前客户：刷新页面后自动回到这里
  localStorage.setItem('currentCustomer', String(id));
  if (location.hash !== '#c' + id) history.replaceState(null, '', '#c' + id);
  state.current = data.customer;
  state.messages = data.messages;
  state.mediaMap = data.media || {};
  hideSuggest();
  clearComposer();
  $('#chatArea').classList.remove('hidden');
  $('#profilePanel').classList.remove('hidden');
  $('#chatName').textContent = state.current.name;
  $('#chatPlatform').textContent = state.current.platform;
  renderCustomers();
  renderMessages();
  renderProfile();
  $('#inputText').focus();
}

const actionLabel = { accepted: '采纳 AI', edited: '修改过', manual: '手写' };

function renderMessages() {
  const box = $('#messages');
  if (!state.messages.length) {
    box.innerHTML = '<p class="muted" style="text-align:center;margin-top:40px">把客户的第一条消息粘贴到下面 👇</p>';
    return;
  }
  box.innerHTML = state.messages.map((m) => {
    const imgs = m.image_path ? m.image_path.split(',').map((p) => `<img src="/files/${esc(p)}" alt="客户截图" onclick="window.open(this.src)">`).join('') : '';
    let intent = '';
    if (m.role === 'customer' && m.analysis) {
      try { const a = JSON.parse(m.analysis); intent = a.intent ? `<span class="tag">${esc(a.intent)}</span>` : ''; } catch {}
    }
    const zh = m.role === 'customer' && showZh() && m.translation ? `<div class="zh">🇨🇳 ${esc(m.translation)}</div>` : '';
    const sentMedia = (m.media_ids || '').split(',').filter(Boolean).map((id) => state.mediaMap?.[id]).filter(Boolean)
      .map((it) => `<span class="sent-media" title="${esc(it.name)}">${mediaThumb(it, 'small')}</span>`).join('');
    const regen = m.role === 'customer' ? `<button class="link" data-regen="${m.id}">重新生成回复</button>`
      : `<button class="link" data-save-snippet="${m.id}" title="把这条回复保存为自定义话术">⭐ 存为话术</button>`;
    const act = m.role === 'agent' ? `<span class="tag agent-${m.action}">${actionLabel[m.action] || ''}</span>` : '';
    return `<div class="msg ${m.role}">
      <div class="bubble">${esc(m.content)}${imgs}${sentMedia ? `<div class="sent-row">📎 ${sentMedia}</div>` : ''}</div>${zh}
      <div class="meta">${m.role === 'customer' ? '客户' : '我方'} · ${fmtTime(m.created_at)} ${intent}${act}${regen}
        <button class="del" data-del="${m.id}" title="删除这条记录">✕</button></div>
    </div>`;
  }).join('');
  box.scrollTop = box.scrollHeight;
}

$('#messages').addEventListener('click', async (e) => {
  const del = e.target.dataset.del;
  const regen = e.target.dataset.regen;
  if (del && confirm('删除这条记录？')) {
    await api('/api/messages/' + del, { method: 'DELETE' });
    state.messages = state.messages.filter((m) => m.id !== +del);
    renderMessages();
  }
  if (regen) {
    state.pendingMsg = state.messages.find((m) => m.id === +regen);
    regenerate('', e.target);
  }
});

// ---------- 粘贴 / 拖拽截图 ----------
function addImages(files) {
  for (const f of files) {
    if (!f.type.startsWith('image/')) continue;
    state.images.push(f);
  }
  renderImages();
}
function renderImages() {
  $('#imagePreview').innerHTML = state.images.map((f, i) =>
    `<div class="thumb"><img src="${URL.createObjectURL(f)}" alt="待识别截图"><button data-rm="${i}" aria-label="移除">×</button></div>`).join('');
}
$('#imagePreview').addEventListener('click', (e) => {
  if (e.target.dataset.rm !== undefined) { state.images.splice(+e.target.dataset.rm, 1); renderImages(); }
});
$('#imageInput').addEventListener('change', (e) => { addImages(e.target.files); e.target.value = ''; });
document.addEventListener('paste', (e) => {
  if (!$('#page-work').classList.contains('active') || !state.current) return;
  const files = [...(e.clipboardData?.files || [])].filter((f) => f.type.startsWith('image/'));
  if (files.length) { e.preventDefault(); addImages(files); toast(`已添加 ${files.length} 张截图`); }
});
const composer = $('#composer');
composer.addEventListener('dragover', (e) => { e.preventDefault(); composer.classList.add('dragover'); });
composer.addEventListener('dragleave', () => composer.classList.remove('dragover'));
composer.addEventListener('drop', (e) => { e.preventDefault(); composer.classList.remove('dragover'); addImages(e.dataTransfer.files); });
$('#inputText').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); $('#btnAnalyze').click(); }
});

function clearComposer() { $('#inputText').value = ''; state.images = []; renderImages(); }

// ---------- AI 分析 ----------
$('#btnAnalyze').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const text = $('#inputText').value.trim();
  if (!text && !state.images.length) { toast('请先粘贴客户的消息或截图', true); return; }
  const role = $('input[name=role]:checked').value;
  const fd = new FormData();
  fd.append('text', text);
  fd.append('role', role);
  state.images.forEach((f, i) => fd.append('images', f, f.name || `paste${i}.png`));
  $('#btnAnalyze').textContent = state.images.length ? '正在识别截图…' : role === 'agent' ? '正在记录…' : 'AI 正在思考…';
  const res = await api(`/api/customers/${state.current.id}/analyze`, { method: 'POST', body: fd });
  clearComposer();
  state.messages.push(res.message);
  renderMessages();
  loadCustomers();
  if (role === 'agent') {
    $('input[name=role][value=customer]').checked = true;
    setTimeout(updateAnalyzeBtn); // 等 busy() 恢复按钮后再更新文字
    toast('已记录我方消息');
    return;
  }
  state.pendingMsg = res.message;
  if (res.error) {
    toast(res.error, true);
    // AI 不可用时仍然可以手写回复并记录
    showSuggest({ reply: '', intent: '', confidence: 0, embed_warning: 'AI 暂不可用，请手动输入回复' });
    state.suggestion.reply = '';
    return;
  }
  showSuggest(res.suggestion);
}));

function updateAnalyzeBtn() {
  const agent = $('input[name=role]:checked').value === 'agent';
  $('#btnAnalyze').textContent = agent ? '② 📝 记录这条我方消息' : '② 🤖 AI 分析并写回复';
}
$$('input[name=role]').forEach((r) => r.addEventListener('change', updateAnalyzeBtn));

function showSuggest(s) {
  state.suggestion = s;
  if (state.pendingMsg && s.customer_message_zh) {
    state.pendingMsg.translation = s.customer_message_zh;
    renderMessages();
  }
  const conf = s.confidence ?? 0;
  const confColor = conf >= 75 ? '🟢' : conf >= 50 ? '🟡' : '🔴';
  $('#suggestTags').innerHTML = [
    s.intent && `<span class="tag">📌 ${esc(s.intent)}</span>`,
    s.sentiment && `<span class="tag">情绪：${esc(s.sentiment)}</span>`,
    s.customer_language && `<span class="tag">🌐 ${esc(s.customer_language)}</span>`,
    s.intent && `<span class="tag">${confColor} 把握度 ${conf}%</span>`,
    s.embed_warning && `<span class="tag">⚠️ ${esc(s.embed_warning)}</span>`,
    (s.web_results?.length || s.web_used) && `<span class="tag agent-edited">🌐 用到了网络信息，发送前请核实</span>`,
    s.web_error && `<span class="tag" title="${esc(s.web_error)}">🌐 联网失败</span>`,
  ].filter(Boolean).join('');
  $('#suggestRisk').classList.toggle('hidden', !s.risk);
  $('#suggestRisk').textContent = '⚠️ 风险提醒：' + (s.risk || '');
  $('#suggestMissing').classList.toggle('hidden', !s.missing_info);
  $('#suggestMissing').textContent = '🔎 需要你确认：' + (s.missing_info || '');
  $('#replyText').value = s.reply || '';
  state.replyZh = s.reply_zh || '';
  renderReplyZh();
  updateAcceptBtn();
  const src = s.source_hits || [];
  const web = s.web_results || [];
  $('#suggestSources').classList.toggle('hidden', !src.length && !web.length);
  $('#suggestSources div').innerHTML = src.map((h) => `<div class="src">${esc(h.content)}</div>`).join('') +
    web.map((w, i) => `<div class="src">🌐 <a href="${esc(w.url)}" target="_blank" rel="noopener noreferrer">[W${i + 1}] ${esc(w.title || w.url)}</a>\n${esc(w.snippet)}</div>`).join('');
  $('#suggestSources').open = web.length > 0;
  $('#zhWriter').classList.add('hidden');
  renderSuggestMedia(s);
  $('#suggestBox').classList.remove('hidden');
  $('#replyText').focus();
}
function renderReplyZh() { $('#replyZh').textContent = showZh() && state.replyZh ? '🇨🇳 ' + state.replyZh : ''; }
function hideSuggest() { $('#suggestBox').classList.add('hidden'); state.suggestion = null; state.pendingMsg = null; }
$('#btnCloseSuggest').addEventListener('click', hideSuggest);

function isEdited() { return state.suggestion && $('#replyText').value.trim() !== (state.suggestion.reply || '').trim(); }
function updateAcceptBtn() { $('#btnAccept').textContent = isEdited() ? '💾 保存我的修改并复制' : '✅ 采纳并复制'; }
$('#replyText').addEventListener('input', () => { updateAcceptBtn(); if (isEdited()) { state.replyZh = ''; renderReplyZh(); } });

async function copyText(t) {
  try { await navigator.clipboard.writeText(t); return true; }
  catch {
    const ta = document.createElement('textarea'); ta.value = t; document.body.appendChild(ta); ta.select();
    const ok = document.execCommand('copy'); ta.remove(); return ok;
  }
}

$('#btnAccept').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const content = $('#replyText').value.trim();
  const media = selectedMedia();
  if (!content && !media.length) { toast('回复不能为空', true); return; }
  const m = await api(`/api/customers/${state.current.id}/reply`, {
    method: 'POST', json: { reply_to: state.pendingMsg?.id || 0, content, ai_reply: state.suggestion?.reply || '', media_ids: media.map((x) => x.id) },
  });
  media.forEach((it) => (state.mediaMap[it.id] = it));
  state.messages.push(m);
  renderMessages();
  const copied = content ? await copyText(content) : true;
  let tip = copied ? '已复制文字，去聊天软件里粘贴发送吧' : '已记录（复制失败，请手动复制）';
  if (m.action === 'edited') tip += '（你的修改已被 AI 学习）';
  if (media.length) tip += `。别忘了发送勾选的 ${media.length} 个图片/视频`;
  toast(tip);
  hideSuggest();
  loadCustomers();
  $('#inputText').focus();
}));

async function regenerate(hint, btn) {
  if (!state.pendingMsg) { toast('找不到要回复的客户消息', true); return; }
  await busy(btn, async () => {
    const s = await api(`/api/messages/${state.pendingMsg.id}/suggest`, { method: 'POST', json: { hint } });
    showSuggest(s);
  });
}
$('#btnRegen').addEventListener('click', (e) => regenerate('换一种说法', e.currentTarget));
$$('[data-hint]').forEach((b) => b.addEventListener('click', (e) => regenerate(b.dataset.hint, e.currentTarget)));

$('#btnWriteZh').addEventListener('click', () => { $('#zhWriter').classList.toggle('hidden'); $('#zhInput').focus(); });
$('#btnZhTranslate').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const text = $('#zhInput').value.trim();
  if (!text) { toast('先写点中文', true); return; }
  const lang = state.suggestion?.customer_language || 'English';
  const r = await api('/api/transform', { method: 'POST', json: { text, mode: 'to_customer', lang } });
  $('#replyText').value = r.text || '';
  state.replyZh = r.zh || text;
  renderReplyZh();
  updateAcceptBtn();
}));

// ---------- 客户资料 / 画像 ----------
const profileLabels = {
  summary: '一句话概括', stage: '所处阶段', purchase_intent: '购买意向', language: '语言', region: '地区',
  interests: '感兴趣', concerns: '顾虑', personality: '沟通风格', preferences: '偏好', next_action: '💡 下一步建议',
};
function renderProfile() {
  const c = state.current;
  $('#pfName').value = c.name; $('#pfPlatform').value = c.platform; $('#pfTags').value = c.tags; $('#pfNotes').value = c.notes;
  if (!c.profile) {
    $('#profileView').innerHTML = '<p class="muted">聊几句后点「更新」，AI 会总结这个客户的特点（每 3 条消息也会自动更新）</p>';
    return;
  }
  let p = {};
  try { p = JSON.parse(c.profile); } catch {}
  $('#profileView').innerHTML = '<dl>' + Object.entries(profileLabels).filter(([k]) => p[k] && String(p[k]).length)
    .map(([k, l]) => `<dt>${l}</dt><dd>${esc(Array.isArray(p[k]) ? p[k].join('、') : p[k])}</dd>`).join('') + '</dl>';
}
$('#btnSaveCustomer').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  state.current = { ...state.current, ...(await api('/api/customers/' + state.current.id, {
    method: 'PUT', json: { name: $('#pfName').value, platform: $('#pfPlatform').value, tags: $('#pfTags').value, notes: $('#pfNotes').value },
  })) };
  $('#chatName').textContent = state.current.name; $('#chatPlatform').textContent = state.current.platform;
  toast('已保存'); loadCustomers();
}));
$('#btnDeleteCustomer').addEventListener('click', async () => {
  if (!confirm(`确定删除客户「${state.current.name}」和全部聊天记录？此操作不能恢复。`)) return;
  await api('/api/customers/' + state.current.id, { method: 'DELETE' });
  state.current = null;
  localStorage.removeItem('currentCustomer');
  history.replaceState(null, '', location.pathname);
  $('#chatArea').classList.add('hidden'); $('#profilePanel').classList.add('hidden');
  loadCustomers();
});
$('#btnProfile').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const r = await api(`/api/customers/${state.current.id}/profile`, { method: 'POST' });
  state.current.profile = r.profile; renderProfile(); toast('画像已更新');
}));

// ================= 资料库 =================
let docTimer = null;
const kindLabel = { doc: '📁 资料', history: '💬 历史聊天', example: '⭐ 示范回复', media: '🖼️ 素材', snippet: '✍️ 自定义话术' };
const statusLabel = { processing: '⏳ 处理中', done: '✅ 完成', error: '❌ 失败' };

async function loadDocs() {
  const docs = await api('/api/docs');
  $('#docTable').innerHTML = docs.map((d) => `<tr>
    <td>${esc(d.name)}</td><td>${kindLabel[d.kind] || d.kind}</td>
    <td class="st-${d.status}">${statusLabel[d.status] || d.status}${d.error ? `<div class="err-text">${esc(d.error)}</div>` : ''}</td>
    <td>${d.chunks}</td>
    <td>${d.status === 'error' && d.error.includes('密码') ? `<button class="btn small primary" data-pw="${d.id}" data-name="${esc(d.name)}">🔑 输入密码</button>` : ''}
        <button class="btn small" data-view="${d.id}" data-name="${esc(d.name)}">查看</button>
        <button class="btn small danger" data-deldoc="${d.id}">删除</button></td></tr>`).join('')
    || '<tr><td colspan="5" class="muted">还没有资料，把文件拖到上面的框里</td></tr>';
  clearTimeout(docTimer);
  if (docs.some((d) => d.status === 'processing') && $('#page-docs').classList.contains('active')) docTimer = setTimeout(loadDocs, 3000);
}

async function uploadFiles(files, kind) {
  files = [...files];
  if (!files.length) return;
  toast(`正在上传 ${files.length} 个文件…`);
  // 分批上传，避免一次太大
  for (let i = 0; i < files.length; i += 20) {
    const fd = new FormData();
    fd.append('kind', kind);
    if ($('#uploadPassword').value) fd.append('password', $('#uploadPassword').value);
    files.slice(i, i + 20).forEach((f) => fd.append('files', f, f.webkitRelativePath || f.name));
    try {
      const res = await api('/api/docs', { method: 'POST', body: fd });
      const skipped = res.filter((r) => r.status !== '已加入处理队列');
      if (skipped.length) toast(skipped.map((r) => `${r.name}：${r.status}`).slice(0, 5).join('；'));
    } catch (e) { toast(e.message, true); }
  }
  loadDocs();
}
$$('#page-docs input[type=file]').forEach((inp) => inp.addEventListener('change', () => { uploadFiles(inp.files, inp.dataset.kind); inp.value = ''; }));
$$('#page-docs .drop').forEach((z) => {
  z.addEventListener('dragover', (e) => { e.preventDefault(); z.classList.add('dragover'); });
  z.addEventListener('dragleave', () => z.classList.remove('dragover'));
  z.addEventListener('drop', (e) => { e.preventDefault(); z.classList.remove('dragover'); uploadFiles(e.dataTransfer.files, z.dataset.kind); });
});
$('#docTable').addEventListener('click', async (e) => {
  const { view, deldoc, name, pw } = e.target.dataset;
  if (pw) {
    $('#pwFileName').textContent = name;
    $('#pwInput').value = '';
    $('#dlgPassword').dataset.id = pw;
    $('#dlgPassword').showModal();
    $('#pwInput').focus();
  }
  if (deldoc && confirm('删除这份资料？AI 将不再参考它。')) { await api('/api/docs/' + deldoc, { method: 'DELETE' }); loadDocs(); }
  if (view) {
    const chunks = await api(`/api/docs/${view}/chunks`);
    $('#chunkTitle').textContent = `「${name}」AI 读到的内容（前 200 段）`;
    $('#chunkList').innerHTML = chunks.map((c) => `<div class="hit">${esc(c)}</div>`).join('') || '<p class="muted">没有内容</p>';
    $('#dlgChunks').showModal();
  }
});
$('#dlgPassword').addEventListener('close', async () => {
  const dlg = $('#dlgPassword');
  const password = $('#pwInput').value;
  $('#pwInput').value = '';
  if (dlg.returnValue !== 'ok' || !password) return;
  try {
    await api(`/api/docs/${dlg.dataset.id}/password`, { method: 'POST', json: { password } });
    toast('正在用密码重新导入…');
    loadDocs();
  } catch (e) { toast(e.message, true); }
});
// 「问一下 AI」：资料库 + 话术 + 素材库 + 可选联网
$('#askWeb').value = localStorage.getItem('askWeb') || 'auto';
$('#askWeb').addEventListener('change', () => { localStorage.setItem('askWeb', $('#askWeb').value); updateAskWebHint(); });
let webConfigured = false;
function updateAskWebHint() { $('#askWebHint').classList.toggle('hidden', webConfigured || $('#askWeb').value === 'off'); }
async function refreshWebConfigured() {
  try { const s = await api('/api/settings'); webConfigured = !!s.web_provider; } catch {}
  updateAskWebHint();
}
document.querySelector('.tab[data-page=docs]').addEventListener('click', refreshWebConfigured);

$('#btnSearch').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const q = $('#searchQ').value.trim();
  if (!q) { toast('请输入问题', true); return; }
  const web = $('#askWeb').value;
  $('#btnSearch').textContent = web === 'always' ? '查资料 + 联网中…' : 'AI 查找中…';
  $('#searchResult').innerHTML = '<p class="muted loading">正在查资料库和素材库</p>';
  const r = await api('/api/ask', { method: 'POST', json: { q, web } });
  remember(r.media); // 素材预览/复制需要
  const sec = (title, body) => `<div class="ask-sec"><div class="ask-title">${title}</div>${body}</div>`;
  let html = '';
  if (r.embed_warning) html += `<div class="alert warn">⚠️ ${esc(r.embed_warning)}</div>`;
  if (r.answer) {
    const badge = r.used_web ? '<span class="tag agent-edited">🌐 含网络信息，请核实</span>'
      : r.covered ? '<span class="tag agent-accepted">📚 来自店铺资料</span>' : '<span class="tag agent-edited">资料库没有完整答案</span>';
    html += sec(`🤖 AI 回答 ${badge}`, `<div class="ask-answer">${esc(r.answer)}</div>` +
      (r.reply_en ? `<div class="ask-reply"><b>可以发给客户：</b>${esc(r.reply_en)} <button class="btn small" data-copy-reply>复制</button></div>` : ''));
  }
  if (r.error) html += `<div class="alert danger">${esc(r.error)}</div>`;
  if (r.web_error) html += `<div class="alert warn">🌐 ${esc(r.web_error)}</div>`;
  if (r.media?.length) {
    html += sec(`🖼️ 相关素材（${r.media.length}）`, `<div class="media-strip">${r.media.map((it) => `<div class="mpick">${mediaThumb(it)}
      <div class="mpick-name">${esc(it.name)}</div>
      <div class="mpick-btns">${isVideo(it) ? '' : `<button class="btn small" data-mact="copy" data-id="${it.id}">复制图片</button>`}
      <button class="btn small" data-mact="reveal" data-id="${it.id}">打开文件夹</button></div></div>`).join('')}</div>`);
  }
  r.kb_hits ||= [];
  html += sec(`📚 资料库命中（${r.kb_hits.length}）`, r.kb_hits.map((h) => `<details class="hit"><summary><b>${kindLabel[h.kind] || h.kind}</b> · 相关度 ${h.score.toFixed(2)} · ${esc(h.content.slice(0, 60).replace(/\n/g, ' '))}…</summary>${esc(h.content)}</details>`).join('')
    || '<p class="muted">资料库里没有找到相关内容</p>');
  if (r.web_results?.length) {
    html += sec(`🌐 网络结果（搜索词：${esc(r.web_query)}）`, r.web_results.map((w, i) => `<div class="web-hit">
      <a href="${esc(w.url)}" target="_blank" rel="noopener noreferrer">[W${i + 1}] ${esc(w.title || w.url)}</a> <span class="tip">${esc(w.site || '')}</span>
      <div class="tip">${esc(w.snippet)}</div></div>`).join(''));
  }
  $('#searchResult').innerHTML = html;
  $('#searchResult').dataset.reply = r.reply_en || '';
}));
$('#searchResult').addEventListener('click', async (e) => {
  if (e.target.closest('[data-copy-reply]')) { await copyText($('#searchResult').dataset.reply); toast('已复制'); }
  const b = e.target.closest('[data-mact]');
  if (b) mediaAction(b.dataset.mact, +b.dataset.id, b);
});
$('#searchQ').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('#btnSearch').click(); });
// 向量状态：缺多少、后台是否在重建（每 2 秒刷新，直到完成）
let reembedTimer = null;
async function refreshReembed() {
  clearTimeout(reembedTimer);
  let p;
  try { p = await api('/api/docs/reembed'); } catch { return; }
  const st = p.state;
  const el = $('#reembedInfo');
  if (st.running) {
    el.textContent = `⏳ 正在用 ${st.model} 生成向量：${st.done} / ${st.total || '…'}（期间只能关键词检索）`;
    reembedTimer = setTimeout(refreshReembed, 2000);
  } else if (st.error) {
    el.innerHTML = `<span class="st-error">❌ 生成向量失败：${esc(st.error.slice(0, 120))}</span>`;
  } else if (p.missing > 0) {
    el.textContent = `⚠️ ${p.missing} / ${p.chunks} 个片段还没有向量（${p.model}），点右边补全`;
  } else {
    el.textContent = p.chunks ? `✅ ${p.chunks} 个片段的向量都已生成（${p.model}）` : '';
  }
  $('#btnReembed').disabled = st.running;
}
document.querySelector('.tab[data-page=docs]').addEventListener('click', refreshReembed);
$('#btnReembed').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  await api('/api/docs/reembed', { method: 'POST' });
  await refreshReembed();
  loadDocs();
}));

// ================= 统计 =================
async function loadStats() {
  const s = await api('/api/stats');
  const rate = s.replies ? Math.round((s.accepted / s.replies) * 100) : 0;
  $('#statCards').innerHTML = [
    ['客户数', s.customers], ['资料文件', s.documents], ['已回复', s.replies], ['AI 直接采纳率', rate + '%'],
    ['修改后发送', s.edited], ['手写回复', s.manual], ['知识片段', s.chunks],
  ].map(([l, n]) => `<div class="stat"><div class="num">${n}</div><div class="lbl">${l}</div></div>`).join('');
  const entries = Object.entries(s.intents).sort((a, b) => b[1] - a[1]);
  const max = Math.max(1, ...entries.map((e) => e[1]));
  $('#intentBars').innerHTML = entries.map(([k, n]) =>
    `<div class="bar"><span class="name">${esc(k)}</span><span class="fill" style="width:${(n / max) * 60}%"></span><span>${n}</span></div>`).join('')
    || '<p class="muted">还没有数据</p>';
}

// ================= 设置 =================
const settingKeys = ['ollama_url', 'chat_model', 'vision_model', 'embed_model', 'openai_base', 'openai_key', 'openai_model', 'openai_vision', 'persona',
  'web_provider', 'web_key', 'web_searxng', 'web_proxy', 'embed_base', 'embed_key', 'embed_cloud', 'embed_dim'];
const embedCloud = () => $('input[name=embed_provider]:checked')?.value === 'openai';
function updateEmbedRows() { $('#embedCloudRows').classList.toggle('hidden', !embedCloud()); }
$$('input[name=embed_provider]').forEach((r) => r.addEventListener('change', updateEmbedRows));
$('#btnEmbedTest').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  try {
    const r = await api('/api/embed/test', { method: 'POST' });
    $('#embedTestResult').innerHTML = ` <span class="st-done">✅ ${esc(r.model)} 可用，向量维度 ${r.dim}</span>`;
  } catch (err) {
    $('#embedTestResult').innerHTML = ` <span class="st-error">❌ ${esc(err.message)}</span>`;
  }
}));
function updateWebRows() {
  const p = $('#s_web_provider').value;
  $('#webKeyRow').classList.toggle('hidden', !['bocha', 'ollama', 'tavily'].includes(p));
  $('#webSearxRow').classList.toggle('hidden', p !== 'searxng');
  $('#webModelTip').classList.toggle('hidden', p !== 'model');
}
$('#s_web_provider').addEventListener('change', updateWebRows);
$('#btnWebTest').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  $('#webTestResult').innerHTML = '';
  try {
    const rs = await api('/api/websearch/test', { method: 'POST', json: {} });
    $('#webTestResult').innerHTML = `<div class="alert">✅ 联网搜索可用，测试搜到 ${rs.length} 条：` +
      rs.slice(0, 3).map((w) => `<div class="tip">· ${esc(w.title)}（${esc(w.site || '')}）</div>`).join('') + '</div>';
  } catch (err) {
    $('#webTestResult').innerHTML = `<div class="alert danger">❌ ${esc(err.message)}</div>`;
  }
}));
async function loadSettings() {
  const s = await api('/api/settings');
  settingKeys.forEach((k) => ($('#s_' + k).value = s[k] || ''));
  $$('input[name=provider]').forEach((r) => (r.checked = r.value === s.provider));
  $$('input[name=reply_language]').forEach((r) => (r.checked = r.value === s.reply_language));
  $$('input[name=web_reply]').forEach((r) => (r.checked = r.value === (s.web_reply || 'off')));
  $$('input[name=embed_provider]').forEach((r) => (r.checked = r.value === (s.embed_provider || 'ollama')));
  updateWebRows();
  updateEmbedRows();
  testConnection();
}
async function testConnection() {
  const url = $('#s_ollama_url').value;
  const s = await api('/api/status?ollama_url=' + encodeURIComponent(url)).catch((e) => ({ ollama_ok: false, ollama_error: e.message }));
  $('#modelList').innerHTML = (s.models || []).map((m) => `<option value="${esc(m)}">`).join('');
  if (s.data_dir) { $('#dataDir').textContent = s.data_dir; $('#appVersion').textContent = s.version; }
  if (!s.ollama_ok) {
    $('#testResult').innerHTML = `<div class="alert danger">❌ 连接不上 Ollama：${esc(s.ollama_error || '')}<br>
      请确认已安装并运行 Ollama（<a href="https://ollama.com/download" target="_blank" rel="noopener">下载地址</a>）。
      如果 Ollama 在另一台电脑上，那台电脑需要设置环境变量 <code>OLLAMA_HOST=0.0.0.0</code> 后重启 Ollama。</div>`;
    return;
  }
  const has = (n) => (s.models || []).some((m) => m === n || m === n + ':latest' || m.replace(/:latest$/, '') === n);
  const rows = [['对话模型', $('#s_chat_model').value], ['看图模型', $('#s_vision_model').value],
    ...(embedCloud() ? [] : [['向量模型', $('#s_embed_model').value]])]
    .map(([l, m]) => has(m) ? `<div class="check">✅ ${l} <b>${esc(m)}</b> 已安装</div>`
      : `<div class="check">❌ ${l} <b>${esc(m)}</b> 未安装，在显卡电脑的终端运行：<code>ollama pull ${esc(m)}</code></div>`);
  $('#testResult').innerHTML = `<div class="alert ${rows.some((r) => r.includes('❌')) ? 'warn' : ''}">✅ 已连接 Ollama，共 ${s.models.length} 个模型${rows.join('')}</div>`;
}
$('#btnTest').addEventListener('click', (e) => busy(e.currentTarget, testConnection));
$('#btnOpenData').addEventListener('click', (e) => busy(e.currentTarget, () => api('/api/datadir/open', { method: 'POST' })));
$('#btnSaveSettings').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const body = {};
  settingKeys.forEach((k) => (body[k] = $('#s_' + k).value));
  body.provider = $('input[name=provider]:checked')?.value || 'ollama';
  body.reply_language = $('input[name=reply_language]:checked')?.value || 'en';
  body.web_reply = $('input[name=web_reply]:checked')?.value || 'off';
  body.embed_provider = embedCloud() ? 'openai' : 'ollama';
  const r = await api('/api/settings', { method: 'PUT', json: body });
  toast(r.reembedding ? '设置已保存。向量模型已更换，正在后台重建全部向量（可在资料库页看进度）' : '设置已保存');
  testConnection();
  checkStatus();
}));

// AI 正在处理时刷新/关闭页面，先提醒（客户消息已保存，但这次的建议回复会丢失）
window.addEventListener('beforeunload', (e) => {
  if ($('#btnAnalyze').disabled || $('#btnAccept').disabled) { e.preventDefault(); e.returnValue = ''; }
});

// ================= 启动 =================
(async () => {
  try {
    await loadCustomers();
  } catch (e) {
    $('#customerList').innerHTML = `<li class="muted">⚠️ 客户列表加载失败：${esc(e.message)}<br><button class="btn small" onclick="location.reload()">重试</button></li>`;
    return;
  }
  // 刷新后回到上次打开的客户（地址栏 #c123 优先）
  const fromHash = /^#c(\d+)$/.exec(location.hash)?.[1];
  const id = +(fromHash || localStorage.getItem('currentCustomer') || 0);
  if (id && state.customers.some((c) => c.id === id)) openCustomer(id);
})();
checkStatus();
setInterval(checkStatus, 60000);
