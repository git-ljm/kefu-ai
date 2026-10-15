// 「问一下 AI」和「联网搜索设置」界面测试
// 用法：node tests/e2e/ask.mjs http://127.0.0.1:8692
import { spawn } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const base = process.argv[2];
const chrome = spawn('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', [
  '--headless=new', '--remote-debugging-port=9336', '--no-first-run',
  `--user-data-dir=${mkdtempSync(join(tmpdir(), 'kefu-ask-'))}`, 'about:blank',
], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws;
for (let i = 0; i < 50 && !ws; i++) {
  try { const p = (await (await fetch('http://127.0.0.1:9336/json/list')).json()).find((t) => t.type === 'page'); if (p) ws = new WebSocket(p.webSocketDebuggerUrl); } catch { await sleep(200); }
}
await new Promise((r) => ws.addEventListener('open', r));
let id = 0; const pending = new Map(); let jsErrors = 0;
ws.addEventListener('message', (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  if (m.method === 'Runtime.exceptionThrown') { jsErrors++; console.log('!! JS 异常:', m.params.exceptionDetails.exception?.description); }
});
const send = (method, params = {}) => new Promise((r) => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
const js = async (e) => (await send('Runtime.evaluate', { expression: e, awaitPromise: true, returnByValue: true })).result?.result?.value;
let pass = true;
const check = (name, ok) => { console.log(ok ? '  ✅' : '  ❌', name); pass &&= ok; };
const askOnce = async (q, web) => {
  await js(`document.querySelector('#searchQ').value=${JSON.stringify(q)}; document.querySelector('#askWeb').value='${web}'; document.querySelector('#btnSearch').click()`);
  for (let t = 0; t < 60 && await js(`document.querySelector('#btnSearch').disabled`); t++) await sleep(250);
  return js(`document.querySelector('#searchResult').innerText`);
};

await send('Runtime.enable'); await send('Page.enable');
await send('Page.navigate', { url: base }); await sleep(1500);
await js(`document.querySelector('.tab[data-page=docs]').click()`); await sleep(600);

console.log('== 资料库能回答');
let t = await askOnce('What is your return policy', 'auto');
console.log('   ' + t.replace(/\n+/g, ' / ').slice(0, 220));
check('显示 AI 回答 + 来自店铺资料 + 可发给客户 + 资料命中', t.includes('KB_ANSWER') && t.includes('来自店铺资料') && t.includes('可以发给客户') && t.includes('资料库命中（1）'));

console.log('== 资料库没有 → 联网');
t = await askOnce('peptide storage', 'auto');
console.log('   ' + t.replace(/\n+/g, ' / ').slice(0, 260));
check('显示网络回答、核实提示和网络结果链接', t.includes('WEB_ANSWER') && t.includes('含网络信息') && t.includes('网络结果') && t.includes('[W1]'));

console.log('== 素材打通');
t = await askOnce('black one real photo', 'off');
console.log('   ' + t.replace(/\n+/g, ' / ').slice(0, 200));
check('显示相关素材和复制/打开按钮', t.includes('相关素材（1）') && t.includes('复制图片'));

console.log('== 设置页：联网搜索');
await js(`document.querySelector('.tab[data-page=settings]').click()`); await sleep(1200);
const st = JSON.parse(await js(`JSON.stringify({p: document.querySelector('#s_web_provider').value, keyHidden: document.querySelector('#webKeyRow').classList.contains('hidden'), searxShown: !document.querySelector('#webSearxRow').classList.contains('hidden')})`));
console.log('   ', JSON.stringify(st));
check('读出已保存的服务（SearXNG），显示地址栏、隐藏 Key 栏', st.p === 'searxng' && st.keyHidden && st.searxShown);
await js(`{ const s=document.querySelector('#s_web_provider'); s.value='bocha'; s.dispatchEvent(new Event('change')) }`);
check('切到博查后显示 Key 栏', !(await js(`document.querySelector('#webKeyRow').classList.contains('hidden')`)));
await js(`{ const s=document.querySelector('#s_web_provider'); s.value='searxng'; s.dispatchEvent(new Event('change')); document.querySelector('#btnWebTest').click() }`);
for (let i = 0; i < 40 && !(await js(`document.querySelector('#webTestResult').innerText`)); i++) await sleep(250);
t = await js(`document.querySelector('#webTestResult').innerText`);
console.log('   ' + t.replace(/\n+/g, ' / ').slice(0, 120));
check('测试联网搜索成功', t.includes('联网搜索可用'));
check('没有 JS 异常', jsErrors === 0);
console.log(pass ? '\n全部通过' : '\n有失败项');
ws.close(); chrome.kill();
