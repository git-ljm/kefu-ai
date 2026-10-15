// 自定义话术：手动新增 / 编辑 / 删除，以及从聊天记录一键保存
let snippets = [];
let editingSnippet = 0; // 0 表示新增

async function loadSnippets() {
  const q = $('#snippetQ').value.trim();
  snippets = await api('/api/snippets' + (q ? '?q=' + encodeURIComponent(q) : ''));
  $('#snippetCount').textContent = q ? `（搜索到 ${snippets.length} 条）` : `（共 ${snippets.length} 条）`;
  const cut = (s, n) => (s.length > n ? s.slice(0, n) + '…' : s);
  $('#snippetTable').innerHTML = snippets.map((s) => `<tr>
    <td>${s.category ? `<span class="tag">${esc(s.category)}</span>` : ''}</td>
    <td><b>${esc(s.title)}</b>${s.warning ? `<div class="err-text" title="${esc(s.warning)}">⚠️ 向量未生成，仅关键词匹配</div>` : ''}</td>
    <td class="pre">${esc(cut(s.question, 120))}</td>
    <td class="pre">${esc(cut(s.answer, 200))}</td>
    <td><button class="btn small" data-sn-edit="${s.id}">编辑</button> <button class="btn small danger" data-sn-del="${s.id}">删除</button></td>
  </tr>`).join('') || `<tr><td colspan="5" class="muted">${q ? '没有匹配的话术' : '还没有自定义话术，点右上角「＋ 新增话术」'}</td></tr>`;
  // 分类下拉：已有分类
  const cats = [...new Set(snippets.map((s) => s.category).filter(Boolean).flatMap((c) => c.split(',')))];
  $('#snippetCats').innerHTML = cats.map((c) => `<option value="${esc(c)}">`).join('');
}

function openSnippetDialog(s = {}) {
  editingSnippet = s.id || 0;
  $('#snippetDlgTitle').textContent = editingSnippet ? '编辑话术' : '新增话术';
  $('#snTitle').value = s.title || '';
  $('#snCategory').value = s.category || '';
  $('#snQuestion').value = s.question || '';
  $('#snAnswer').value = s.answer || '';
  $('#dlgSnippet').showModal();
  (s.answer ? $('#snTitle') : $('#snAnswer')).focus();
}

$('#btnNewSnippet').addEventListener('click', () => openSnippetDialog());
$('#snippetQ').addEventListener('input', () => { clearTimeout(loadSnippets._t); loadSnippets._t = setTimeout(loadSnippets, 250); });
$('#snippetTable').addEventListener('click', async (e) => {
  const edit = e.target.dataset.snEdit, del = e.target.dataset.snDel;
  if (edit) openSnippetDialog(snippets.find((s) => s.id === +edit));
  if (del && confirm('删除这条话术？')) {
    try { await api('/api/snippets/' + del, { method: 'DELETE' }); toast('已删除'); loadSnippets(); } catch (err) { toast(err.message, true); }
  }
});

// 保存：拦截 dialog 的提交，失败时不关闭对话框
$('#snippetForm').addEventListener('submit', async (e) => {
  if (e.submitter?.value !== 'ok') return; // 取消
  e.preventDefault();
  const body = { title: $('#snTitle').value, category: $('#snCategory').value, question: $('#snQuestion').value, answer: $('#snAnswer').value };
  if (!body.answer.trim()) { toast('请填写推荐回复', true); return; }
  await busy(e.submitter, async () => {
    await api('/api/snippets' + (editingSnippet ? '/' + editingSnippet : ''), { method: editingSnippet ? 'PUT' : 'POST', json: body });
    $('#dlgSnippet').close();
    toast(editingSnippet ? '话术已更新' : '话术已保存，AI 回复时会优先参考');
    if ($('#page-docs').classList.contains('active')) loadSnippets();
  });
});

$('#btnSnTranslate').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const text = $('#snAnswer').value.trim();
  if (!text) { toast('先写回复内容', true); return; }
  const r = await api('/api/transform', { method: 'POST', json: { text, mode: 'to_customer', lang: 'English' } });
  if (r.text) $('#snAnswer').value = r.text + '\n\n（中文：' + text + '）';
}));

// 聊天记录里“我方”的消息：⭐ 存为话术（问法 = 它回复的那条客户消息）
$('#messages').addEventListener('click', (e) => {
  const id = e.target.dataset.saveSnippet;
  if (!id) return;
  const m = state.messages.find((x) => x.id === +id);
  if (!m) return;
  const q = state.messages.find((x) => x.id === m.reply_to) ||
    [...state.messages].reverse().find((x) => x.role === 'customer' && x.id < m.id);
  let intent = '';
  try { intent = q?.analysis ? JSON.parse(q.analysis).intent || '' : ''; } catch {}
  openSnippetDialog({ title: '', category: intent, question: q?.content || '', answer: m.content });
});

// 打开资料库页时加载
document.querySelector('.tab[data-page=docs]').addEventListener('click', () => loadSnippets().catch((e) => toast(e.message, true)));
