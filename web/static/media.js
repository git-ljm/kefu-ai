// 素材库：上传（含视频截帧）、管理、回复时推荐与发送
const isVideo = (it) => it.media_type === 'video';

// 缩略图 HTML：图片直接显示；视频优先用关键帧拼图，没有就让浏览器显示第一帧
function mediaThumb(it, size = '') {
  const cls = `mthumb ${size}`;
  if (it.thumb_url) {
    return `<span class="${cls}" data-preview="${it.id}"><img src="${esc(it.thumb_url)}" alt="${esc(it.name)}" loading="lazy">${isVideo(it) ? '<b class="play">▶</b>' : ''}</span>`;
  }
  if (isVideo(it)) {
    return `<span class="${cls}" data-preview="${it.id}"><video src="${esc(it.url)}#t=0.5" preload="metadata" muted></video><b class="play">▶</b></span>`;
  }
  return `<span class="${cls}">📄</span>`;
}

// 所有出现过的素材，用 id 找回完整信息（预览、复制用）
const mediaCache = {};
const remember = (list) => (list || []).forEach((it) => (mediaCache[it.id] = it));

function preview(id) {
  const it = mediaCache[id] || state.mediaMap?.[id];
  if (!it) return;
  $('#previewBody').innerHTML = (isVideo(it)
    ? `<video src="${esc(it.url)}" controls autoplay></video>`
    : `<img src="${esc(it.url)}" alt="${esc(it.name)}">`) +
    `<p><b>${esc(it.name)}</b></p><p class="muted">${esc(it.description || '')}</p>`;
  $('#dlgPreview').showModal();
}
$('#dlgPreview').addEventListener('close', () => ($('#previewBody').innerHTML = ''));
document.addEventListener('click', (e) => {
  const p = e.target.closest('[data-preview]');
  if (p) { e.preventDefault(); preview(+p.dataset.preview); }
});

// ---------- 把图片复制到剪贴板（聊天软件里直接 Ctrl+V） ----------
async function copyImage(url) {
  const blob = await (await fetch(url)).blob();
  let png = blob;
  if (blob.type !== 'image/png') { // 剪贴板只接受 PNG
    const bmp = await createImageBitmap(blob);
    const c = document.createElement('canvas');
    c.width = bmp.width; c.height = bmp.height;
    c.getContext('2d').drawImage(bmp, 0, 0);
    png = await new Promise((r) => c.toBlob(r, 'image/png'));
  }
  await navigator.clipboard.write([new ClipboardItem({ 'image/png': png })]);
}

async function mediaAction(act, id, btn) {
  const it = mediaCache[id];
  if (act === 'copy') {
    await busy(btn, async () => { await copyImage(it.url); toast('图片已复制，去聊天软件里粘贴（Ctrl+V / ⌘+V）'); });
  } else if (act === 'reveal') {
    await busy(btn, async () => { await api(`/api/media/${id}/reveal`, { method: 'POST' }); toast('已在文件夹中选中，直接拖进聊天软件发送'); });
  }
}

// ---------- 回复框中的素材推荐 ----------
function mediaPickCard(it, checked) {
  mediaCache[it.id] = it;
  return `<div class="mpick">
    ${mediaThumb(it)}
    <label class="mpick-name"><input type="checkbox" data-pick="${it.id}" ${checked ? 'checked' : ''}> ${esc(it.name)}</label>
    ${it.sent ? '<span class="tag agent-edited">已发过</span>' : ''}
    <div class="mpick-btns">
      ${isVideo(it) ? '' : `<button class="btn small" data-mact="copy" data-id="${it.id}">复制图片</button>`}
      <button class="btn small" data-mact="reveal" data-id="${it.id}">打开文件夹</button>
      <a class="btn small" href="${esc(it.url)}" download="${esc(it.name)}">下载</a>
    </div>
  </div>`;
}

function renderSuggestMedia(s) {
  const picked = s.media_items || [];
  const more = s.media_more || [];
  $('#suggestMedia').classList.toggle('hidden', !picked.length && !more.length);
  $('#mediaReason').textContent = s.media_reason ? '— ' + s.media_reason : '';
  $('#mediaPicked').innerHTML = picked.map((it) => mediaPickCard(it, true)).join('') || '<span class="tip">AI 认为这次不需要发图片/视频</span>';
  $('#mediaMore').innerHTML = more.map((it) => mediaPickCard(it, false)).join('');
  $('#mediaMoreBox').classList.toggle('hidden', !more.length);
  $('#mediaMoreBox').open = !picked.length && more.length > 0;
  $('#mediaFound').innerHTML = '';
  $('#mediaFind').value = '';
}

function selectedMedia() {
  const ids = new Set();
  $$('#suggestBox [data-pick]:checked').forEach((c) => ids.add(+c.dataset.pick));
  return [...ids].map((id) => mediaCache[id]).filter(Boolean);
}

$('#suggestBox').addEventListener('click', (e) => {
  const b = e.target.closest('[data-mact]');
  if (b) mediaAction(b.dataset.mact, +b.dataset.id, b);
});
$('#btnMediaFind').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const q = $('#mediaFind').value.trim();
  if (!q) return;
  const list = await api('/api/media?q=' + encodeURIComponent(q));
  $('#mediaFound').innerHTML = list.map((it) => mediaPickCard(it, false)).join('') || '<span class="tip">没找到，换个关键词试试</span>';
}));
$('#mediaFind').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); $('#btnMediaFind').click(); } });

// ---------- 视频截帧：在浏览器里截 4 个画面拼成一张图，给 AI 看 ----------
function once(el, ev, ms) {
  return new Promise((ok, fail) => {
    const t = setTimeout(() => fail(new Error('timeout')), ms);
    el.addEventListener(ev, () => { clearTimeout(t); ok(); }, { once: true });
    el.addEventListener('error', () => { clearTimeout(t); fail(new Error('decode')); }, { once: true });
  });
}

async function videoPoster(file) {
  const url = URL.createObjectURL(file);
  const v = document.createElement('video');
  v.muted = true; v.preload = 'auto'; v.src = url;
  try {
    await once(v, 'loadeddata', 15000);
    const d = isFinite(v.duration) && v.duration > 0 ? v.duration : 1;
    const w = 480, h = Math.round((w * v.videoHeight) / v.videoWidth) || 270;
    const c = document.createElement('canvas');
    c.width = w * 2; c.height = h * 2;
    const g = c.getContext('2d');
    const points = [0.08, 0.35, 0.62, 0.9];
    for (let i = 0; i < points.length; i++) {
      v.currentTime = Math.min(d * points[i], Math.max(0, d - 0.05));
      await once(v, 'seeked', 8000);
      g.drawImage(v, (i % 2) * w, Math.floor(i / 2) * h, w, h);
    }
    return await new Promise((r) => c.toBlob(r, 'image/jpeg', 0.82));
  } catch {
    return null; // 浏览器解不了的格式，交给后端（ffmpeg）或人工描述
  } finally {
    v.removeAttribute('src'); v.load();
    URL.revokeObjectURL(url);
  }
}

async function uploadMedia(files) {
  // 没有后缀的文件浏览器认不出类型，交给后端按内容判断
  files = [...files].filter((f) => /^(image|video)\//.test(f.type) || /\.(mp4|mov|m4v|webm|avi|mkv|3gp|png|jpe?g|webp|gif|bmp)$/i.test(f.name)
    || (!f.type && !/\.[a-z0-9]{1,5}$/i.test(f.name) && !f.name.startsWith('.')));
  if (!files.length) { toast('请选择图片或视频文件', true); return; }
  const tags = $('#mediaTags').value.trim();
  const prog = $('#mediaUploadProgress');
  let done = 0, skipped = [];
  // 每批最多 10 个或 300MB
  for (let i = 0; i < files.length;) {
    const batch = [];
    let size = 0;
    while (i < files.length && batch.length < 10 && (size === 0 || size + files[i].size < 300e6)) {
      size += files[i].size; batch.push(files[i++]);
    }
    const fd = new FormData();
    fd.append('kind', 'media');
    fd.append('tags', tags);
    for (let j = 0; j < batch.length; j++) {
      const f = batch[j];
      fd.append('files', f, f.name);
      if (f.type.startsWith('video/') || /\.(mp4|mov|m4v|webm|avi|mkv|3gp)$/i.test(f.name)) {
        prog.textContent = `正在截取视频画面：${f.name}`;
        const poster = await videoPoster(f);
        if (poster) fd.append('poster_' + j, poster, 'poster.jpg');
      }
    }
    prog.textContent = `正在上传 ${done + 1}-${done + batch.length} / ${files.length} …`;
    try {
      const res = await api('/api/docs', { method: 'POST', body: fd });
      skipped.push(...res.filter((r) => r.status !== '已加入处理队列').map((r) => `${r.name}：${r.status}`));
    } catch (e) { toast(e.message, true); }
    done += batch.length;
  }
  prog.textContent = `上传完成 ${files.length} 个，AI 正在识别内容（每张图约几秒）…` + (skipped.length ? ` 跳过：${skipped.slice(0, 3).join('；')}` : '');
  loadMedia();
}

$$('#page-media input[type=file]').forEach((inp) => inp.addEventListener('change', () => { uploadMedia(inp.files); inp.value = ''; }));
{
  const z = $('#page-media .drop');
  z.addEventListener('dragover', (e) => { e.preventDefault(); z.classList.add('dragover'); });
  z.addEventListener('dragleave', () => z.classList.remove('dragover'));
  z.addEventListener('drop', (e) => { e.preventDefault(); z.classList.remove('dragover'); uploadMedia(e.dataTransfer.files); });
}

// ---------- 素材库页面 ----------
let mediaTimer = null;
let mediaQuery = '';

async function loadMedia(q = mediaQuery) {
  mediaQuery = q;
  const list = await api('/api/media' + (q ? '?q=' + encodeURIComponent(q) : ''));
  remember(list);
  $('#mediaGrid').innerHTML = list.map((d) => `
    <div class="mcard" data-id="${d.id}">
      <div class="mcard-thumb">${mediaThumb(d)}</div>
      <div class="mcard-body">
        <input class="input" data-f="name" value="${esc(d.name)}" aria-label="名称">
        <div class="st-${d.status}">${({ processing: '⏳ AI 识别中…', done: '✅ 已识别', error: '❌ 失败' })[d.status] || d.status}
          ${d.send_count ? `<span class="tip"> · 已发送 ${d.send_count} 次</span>` : ''}</div>
        ${d.error ? `<div class="err-text">${esc(d.error)}</div>` : ''}
        <label class="label">画面内容<textarea class="input" data-f="description" rows="3">${esc(d.description || '')}</textarea></label>
        <label class="label">标签<input class="input" data-f="tags" value="${esc(d.tags || '')}"></label>
        <label class="label">什么时候发<textarea class="input" data-f="usage" rows="2">${esc(d.usage || '')}</textarea></label>
        <div class="row wrap">
          <button class="btn small primary" data-act="save">保存</button>
          <button class="btn small" data-act="rescan" title="让 AI 重新看一遍">重新识别</button>
          ${isVideo(d) ? '' : '<button class="btn small" data-act="copy">复制图片</button>'}
          <button class="btn small" data-act="reveal">打开文件夹</button>
          <button class="btn small danger" data-act="del">删除</button>
        </div>
      </div>
    </div>`).join('') || `<p class="muted">${q ? '没有找到相关素材' : '还没有素材，把图片/视频拖到上面的框里'}</p>`;
  clearTimeout(mediaTimer);
  if (list.some((d) => d.status === 'processing') && $('#page-media').classList.contains('active')) {
    mediaTimer = setTimeout(() => refreshMediaStatus(), 4000);
  }
}

// 识别中时只刷新状态，避免覆盖用户正在编辑的内容
async function refreshMediaStatus() {
  const editing = document.activeElement?.closest?.('.mcard');
  if (editing) { mediaTimer = setTimeout(refreshMediaStatus, 4000); return; }
  loadMedia();
}

$('#mediaGrid').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-act]');
  if (!b) return;
  const card = b.closest('.mcard');
  const id = +card.dataset.id;
  const val = (f) => card.querySelector(`[data-f=${f}]`).value;
  switch (b.dataset.act) {
    case 'save':
      await busy(b, async () => {
        const d = await api('/api/media/' + id, { method: 'PUT', json: { name: val('name'), description: val('description'), tags: val('tags'), usage: val('usage') } });
        mediaCache[id] = d; toast('已保存，AI 推荐时会使用新的描述');
      });
      break;
    case 'rescan':
      await busy(b, async () => { await api(`/api/media/${id}/rescan`, { method: 'POST' }); loadMedia(); });
      break;
    case 'copy': case 'reveal':
      await mediaAction(b.dataset.act, id, b);
      break;
    case 'del':
      if (confirm('删除这个素材？文件也会被删除。')) { await api('/api/media/' + id, { method: 'DELETE' }); loadMedia(); }
      break;
  }
});
$('#btnMediaQ').addEventListener('click', (e) => busy(e.currentTarget, () => loadMedia($('#mediaQ').value.trim())));
$('#mediaQ').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('#btnMediaQ').click(); });
$('#btnMediaAll').addEventListener('click', () => { $('#mediaQ').value = ''; loadMedia(''); });
