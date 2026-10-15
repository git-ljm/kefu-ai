// 自定义话术界面测试：新增、编辑、从聊天记录“存为话术”
// 用法：node tests/e2e/snippet.mjs http://127.0.0.1:8695   （数据里要有客户 1，且有一条我方回复）
import { spawn } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const base = process.argv[2];
const chrome = spawn('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', [
  '--headless=new', '--remote-debugging-port=9335', '--no-first-run',
  `--user-data-dir=${mkdtempSync(join(tmpdir(), 'kefu-sn-'))}`, 'about:blank',
], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws;
for (let i = 0; i < 50 && !ws; i++) {
  try { const p = (await (await fetch('http://127.0.0.1:9335/json/list')).json()).find((t) => t.type === 'page'); if (p) ws = new WebSocket(p.webSocketDebuggerUrl); } catch { await sleep(200); }
}
await new Promise((r) => ws.addEventListener('open', r));
let id = 0; const pending = new Map();
ws.addEventListener('message', (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  if (m.method === 'Runtime.exceptionThrown') console.log('!! JS 异常:', m.params.exceptionDetails.exception?.description);
});
const send = (method, params = {}) => new Promise((r) => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
const js = async (e) => (await send('Runtime.evaluate', { expression: e, awaitPromise: true, returnByValue: true })).result?.result?.value;
let pass = true;
const check = (name, ok) => { console.log(ok ? '  ✅' : '  ❌', name); pass &&= ok; };
const rows = () => js(`document.querySelector('#snippetTable').innerText.replace(/\\s+/g,' ').slice(0,200)`);

await send('Runtime.enable'); await send('Page.enable');
await send('Page.navigate', { url: base }); await sleep(1500);

console.log('== 资料库页 → 新增话术');
await js(`document.querySelector('.tab[data-page=docs]').click()`); await sleep(500);
await js(`document.querySelector('#btnNewSnippet').click()`); await sleep(200);
check('对话框打开', await js(`document.querySelector('#dlgSnippet').open`));
await js(`document.querySelector('#snCategory').value='物流'; document.querySelector('#snQuestion').value='When will it ship?'; document.querySelector('#snAnswer').value='We ship within 2 business days.'; document.querySelector('#snippetForm button[value=ok]').click()`);
await sleep(800);
check('保存后对话框关闭', !(await js(`document.querySelector('#dlgSnippet').open`)));
let r = await rows(); console.log('   ', r);
check('列表出现新话术（没写标题时自动用问法）', r.includes('When will it ship') && r.includes('物流'));

console.log('== 空回复不能保存');
await js(`document.querySelector('#btnNewSnippet').click()`); await sleep(200);
await js(`document.querySelector('#snAnswer').value='   '; document.querySelector('#snippetForm button[value=ok]').click()`); await sleep(500);
check('对话框保持打开', await js(`document.querySelector('#dlgSnippet').open`));
await js(`document.querySelector('#snippetForm button[value=cancel]').click()`); await sleep(200);

console.log('== 编辑');
await js(`document.querySelector('[data-sn-edit]').click()`); await sleep(200);
await js(`document.querySelector('#snTitle').value='问发货时间'; document.querySelector('#snippetForm button[value=ok]').click()`); await sleep(800);
r = await rows(); console.log('   ', r);
check('标题已更新', r.includes('问发货时间'));

console.log('== 聊天记录里「⭐ 存为话术」');
await js(`document.querySelector('.tab[data-page=work]').click()`); await sleep(200);
await js(`document.querySelector('#customerList li[data-id]').click()`); await sleep(800);
const has = await js(`!!document.querySelector('[data-save-snippet]')`);
check('我方消息上有「存为话术」按钮', has);
if (has) {
  await js(`document.querySelector('[data-save-snippet]').click()`); await sleep(300);
  const f = await js(`JSON.stringify({q: document.querySelector('#snQuestion').value, a: document.querySelector('#snAnswer').value, c: document.querySelector('#snCategory').value})`);
  console.log('   预填:', f);
  const v = JSON.parse(f);
  check('预填了客户问法和我方回复', v.q.length > 0 && v.a.length > 0);
  await js(`document.querySelector('#snippetForm button[value=ok]').click()`); await sleep(800);
  await js(`document.querySelector('.tab[data-page=docs]').click()`); await sleep(600);
  r = await rows(); console.log('   ', r);
  check('资料库里能看到这条话术', r.includes(v.a.slice(0, 15)));
}
console.log(pass ? '\n全部通过' : '\n有失败项');
ws.close(); chrome.kill();
