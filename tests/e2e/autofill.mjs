// 复现“浏览器把保存的用户名自动填进客户搜索框，导致点客户后列表变空”
// 用法：node tests/e2e/autofill.mjs http://127.0.0.1:8696   （数据里至少要有 2 个客户）
import { spawn } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const base = process.argv[2];
const chrome = spawn('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', [
  '--headless=new', '--remote-debugging-port=9334', '--no-first-run',
  `--user-data-dir=${mkdtempSync(join(tmpdir(), 'kefu-af-'))}`, 'about:blank',
], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws;
for (let i = 0; i < 50 && !ws; i++) {
  try { const p = (await (await fetch('http://127.0.0.1:9334/json/list')).json()).find((t) => t.type === 'page'); if (p) ws = new WebSocket(p.webSocketDebuggerUrl); } catch { await sleep(200); }
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
const state = () => js(`JSON.stringify({search: document.querySelector('#customerSearch').value, list: document.querySelector('#customerList').innerText.replace(/\\n/g,' / ').slice(0,90), chat: !document.querySelector('#chatArea').classList.contains('hidden') ? document.querySelector('#chatName').textContent : '(未打开)'})`);
// 真实鼠标点击（会触发 pointerdown）
const click = async (sel) => {
  const r = await js(`(() => { const b = document.querySelector('${sel}').getBoundingClientRect(); return [b.x + b.width / 2, b.y + b.height / 2]; })()`);
  for (const type of ['mousePressed', 'mouseReleased']) await send('Input.dispatchMouseEvent', { type, x: r[0], y: r[1], button: 'left', clickCount: 1 });
};
let pass = true;
const check = (name, ok) => { console.log(ok ? '  ✅' : '  ❌', name); pass &&= ok; };

await send('Runtime.enable'); await send('Page.enable');
await send('Page.navigate', { url: base }); await sleep(1500);
console.log('初始:', await state());

console.log('\n场景1：浏览器自动填充（直接改值 + 普通 input 事件）');
await js(`const el = document.querySelector('#customerSearch'); el.value = 'admin'; el.dispatchEvent(new Event('input', { bubbles: true }))`);
await sleep(200); let s = JSON.parse(await state()); console.log('  ', JSON.stringify(s));
check('自动填充的内容被清掉，客户列表完整', s.search === '' && !s.list.includes('没有找到'));

console.log('\n场景2：Chrome 隐藏式自动填充（值已在框里，用户第一次点击页面才生效），然后点客户');
await js(`document.querySelector('#customerSearch').value = 'admin'`);
await click('#customerList li[data-id]'); await sleep(800);
s = JSON.parse(await state()); console.log('  ', JSON.stringify(s));
check('点击客户后列表不为空，并打开了客户', s.search === '' && !s.list.includes('没有找到') && s.chat !== '(未打开)');

console.log('\n场景3：用户亲手输入搜索');
await click('#customerSearch');
await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'J', text: 'J' });
await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'J' });
await sleep(200); s = JSON.parse(await state()); console.log('  ', JSON.stringify(s));
check('手动输入的搜索词保留并正常过滤', s.search === 'J');
await send('Input.insertText', { text: 'zzz' }); await sleep(200);
s = JSON.parse(await state()); console.log('  ', JSON.stringify(s));
check('搜不到时提示里带搜索词和清除按钮', s.list.includes('Jzzz') && s.list.includes('清除搜索'));
await click('[data-clear-search]'); await sleep(200);
s = JSON.parse(await state()); console.log('  ', JSON.stringify(s));
check('点「清除搜索」后恢复全部客户', s.search === '' && !s.list.includes('没有找到'));

console.log('\n场景4：密码框的 autocomplete 设置');
const ac = await js(`[...document.querySelectorAll('input[type=password]')].map((e) => e.id + '=' + e.autocomplete).join(', ')`);
console.log('  ', ac);
check('所有密码框都是 new-password（浏览器不会保存/自动填充）', !ac.includes('=off') && !ac.includes('=,'));

console.log(pass ? '\n全部通过' : '\n有失败项');
ws.close(); chrome.kill();
