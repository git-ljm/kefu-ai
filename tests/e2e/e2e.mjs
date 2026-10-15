// 用 Chrome DevTools 协议做端到端复现（Node 自带 WebSocket，无需安装依赖）
// 用法：先用测试数据目录启动程序（会写入测试客户），再运行
//   go run ./cmd/kefu -no-browser -port 8698 -data /tmp/kefu-e2e
//   node tests/e2e/e2e.mjs http://127.0.0.1:8698
// 需要本机安装 Chrome，且数据里至少有一个客户
import { spawn } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const base = process.argv[2];
const chrome = spawn('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', [
  '--headless=new', '--remote-debugging-port=9333', '--no-first-run', '--no-default-browser-check',
  `--user-data-dir=${mkdtempSync(join(tmpdir(), 'kefu-e2e-'))}`, 'about:blank',
], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let ws;
for (let i = 0; i < 50 && !ws; i++) {
  try {
    const list = await (await fetch('http://127.0.0.1:9333/json/list')).json();
    const page = list.find((t) => t.type === 'page');
    if (page) ws = new WebSocket(page.webSocketDebuggerUrl);
  } catch { await sleep(200); }
}
await new Promise((r) => ws.addEventListener('open', r));
let id = 0;
const pending = new Map();
ws.addEventListener('message', (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  if (m.method === 'Runtime.exceptionThrown') console.log('!! JS 异常:', m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
  if (m.method === 'Runtime.consoleAPICalled') console.log('   console.' + m.params.type + ':', m.params.args.map((a) => a.value ?? a.description).join(' '));
  if (m.method === 'Network.responseReceived' && m.params.response.url.includes('/api/')) console.log('   ←', m.params.response.status, m.params.response.url.replace(base, ''));
});
const send = (method, params = {}) => new Promise((r) => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
const js = async (expr) => {
  const r = await send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.result?.exceptionDetails) return 'EXC: ' + r.result.exceptionDetails.exception?.description;
  return r.result?.result?.value;
};
const until = async (expr, ms = 15000) => { for (let t = 0; t < ms; t += 200) { if (await js(expr)) return true; await sleep(200); } return false; };

await send('Runtime.enable'); await send('Network.enable'); await send('Page.enable');
const report = async (label) => console.log(label, JSON.stringify(await js(`({
  welcome: !document.querySelector('#welcome').classList.contains('hidden'),
  chat: !document.querySelector('#chatArea').classList.contains('hidden'),
  list: document.querySelector('#customerList').innerText.replace(/\\n/g,' / ').slice(0,120),
  msgs: document.querySelector('#messages').innerText.replace(/\\n/g,' / ').slice(0,200),
})`)));

console.log('== 打开页面'); await send('Page.navigate', { url: base }); await until(`document.querySelector('#customerList li[data-id]')`);
await report('   状态');
console.log('== 点第一个客户'); await js(`document.querySelector('#customerList li[data-id]').click()`); await sleep(800); await report('   状态');
console.log('== 粘贴并分析');
await js(`document.querySelector('#inputText').value='How much for 10 vials?'; document.querySelector('#btnAnalyze').click()`);
console.log('   建议框出现:', await until(`!document.querySelector('#suggestBox').classList.contains('hidden')`, 30000), '内容:', await js(`document.querySelector('#replyText').value`));
console.log('== 采纳'); await js(`document.querySelector('#btnAccept').click()`); await sleep(1200); await report('   状态');
console.log('== 刷新'); await send('Page.reload'); await sleep(2000); await report('   状态');
console.log('   地址:', await js('location.href'));
console.log('== 新建同名客户'); await js(`window.confirm = (m) => { console.log('confirm弹窗: ' + m.split('\\n')[0]); return true; }; document.querySelector('#ncName').value='jack'; const d=document.querySelector('#dlgCustomer'); d.showModal(); d.close('ok')`);
await sleep(1000); await report('   状态');
console.log('== 去掉地址栏 hash 再打开（模拟重新打开浏览器）'); await send('Page.navigate', { url: base }); await sleep(2000); await report('   状态');
ws.close(); chrome.kill();
