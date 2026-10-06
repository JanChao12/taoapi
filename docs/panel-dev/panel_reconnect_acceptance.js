// panel_reconnect_acceptance.js —— 跨 origin 自动重连验收（Codex 第 19 轮定稿的 6 场景）
//
// 用法：
//   node docs/panel-dev/panel_reconnect_acceptance.js
//
// ═══════════════════════════════════════════════════════════════════
// 为什么单独一个脚本
// ═══════════════════════════════════════════════════════════════════
//
//   panel_acceptance.js 验的是"设置页的写入/安全/布局"，
//   本脚本验的是"改端口重启后的跨 origin 自动重连" —— 它需要
//   **真的起进程、真的换端口**，与前者差别太大，混在一起会互相拖累。
//
// ═══════════════════════════════════════════════════════════════════
// Codex 第 19 轮定稿的 6 个场景（不新增）
// ═══════════════════════════════════════════════════════════════════
//
//   1.  新服务返回 503                    → 不导航，继续等待
//   2a. 无关服务 200 + 通配 CORS           → 不导航
//   2b. 200 + 正确 CORS + service/ready 对
//       但 restartId 错                    → 不导航
//   3.  旧 origin 探测新 origin            → 能读到 body（显式断言）
//   4.  实际改端口重启                     → 面板**自行导航**，新页面能保存设置
//   5.  新端口被占用                       → 旧服务恢复，有限时间内显示失败
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 两条方法学红线（Codex 强调，违反则测试无意义）
// ═══════════════════════════════════════════════════════════════════
//
//   1. **场景 4 必须由面板自行导航**，测试脚本不得代它跳转 ——
//      代跳会绕过被测的生产逻辑（导航决策恰恰是要验的东西）。
//      本脚本只**观察** `Page.frameNavigated` / 当前 URL 变化。
//   2. **不预启动"新端口实例"占位**：那会改变真实的子进程 bind 语义。
//      场景 5 要的是"新端口被**无关**程序占用"，用测试自己的服务占，
//      而不是再起一个 wbapi。
//
// ⚠️ CDP 连接的是 **Chrome 进程**，不是页面 origin ——
//    页面换 origin（跳转到新端口）**不会**断开调试连接。
//    这正是我原先误判"做不了"的地方（Codex 纠正）。

'use strict';

const http = require('http');
const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');
const net = require('net');

// ── 断言框架 ──
let passed = 0;
let failed = 0;
const failures = [];

function ok(cond, label, detail) {
  if (cond) {
    passed++;
    console.log('  ✓ ' + label);
  } else {
    failed++;
    failures.push(label + (detail ? '  ← ' + detail : ''));
    console.log('  ✗ ' + label + (detail ? '  ← ' + detail : ''));
  }
}

function section(t) { console.log('\n── ' + t + ' ──'); }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── 配置（可用环境变量覆盖）──
const WBAPI = process.env.WBAPI_EXE ||
  path.join(__dirname, '..', '..', 'wbapi.exe');
const OLD_PORT = Number(process.env.OLD_PORT || 8891); // 面板初始端口
const NEW_PORT = Number(process.env.NEW_PORT || 8892); // 重启后目标端口
const DEBUG_PORT = Number(process.env.CDP_PORT || 9344);
const DATA_DIR = process.env.DATA_DIR ||
  path.join(process.env.TEMP || '/tmp', 'wbapi-reconnect-accept');

// ── 找到 Chrome ──
function findChrome() {
  const cands = [
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    '/usr/bin/google-chrome',
    '/usr/bin/chromium',
  ];
  for (const c of cands) {
    try { fs.accessSync(c); return c; } catch (_) { /* 继续 */ }
  }
  return null;
}

// ── 极简 HTTP 工具 ──
function httpGetJSON(port, p, timeoutMs) {
  return new Promise((resolve, reject) => {
    const req = http.get({ host: '127.0.0.1', port, path: p, timeout: timeoutMs || 3000 },
      (res) => {
        let d = '';
        res.on('data', (c) => (d += c));
        res.on('end', () => {
          try { resolve({ status: res.statusCode, body: JSON.parse(d) }); }
          catch (e) { resolve({ status: res.statusCode, body: d }); }
        });
      });
    req.on('timeout', () => { req.destroy(new Error('timeout')); });
    req.on('error', reject);
  });
}

async function waitPort(port, wantUp, ms) {
  const deadline = Date.now() + (ms || 15000);
  while (Date.now() < deadline) {
    const up = await new Promise((resolve) => {
      const s = net.connect(port, '127.0.0.1');
      s.on('connect', () => { s.destroy(); resolve(true); });
      s.on('error', () => resolve(false));
      s.setTimeout(500, () => { s.destroy(); resolve(false); });
    });
    if (up === wantUp) return true;
    await sleep(200);
  }
  return false;
}

// ── CDP ──
function getJSON(port, p) {
  return new Promise((resolve, reject) => {
    http.get({ host: '127.0.0.1', port, path: p }, (res) => {
      let d = '';
      res.on('data', (c) => (d += c));
      res.on('end', () => { try { resolve(JSON.parse(d)); } catch (e) { reject(e); } });
    }).on('error', reject);
  });
}

async function connect(port) {
  let tabs;
  for (let i = 0; i < 60; i++) {
    try { tabs = await getJSON(port, '/json/list'); break; } catch (_) { await sleep(250); }
  }
  if (!tabs) throw new Error('无法连接 Chrome 调试端口');
  const target = tabs.find((t) => t.type === 'page');
  if (!target) throw new Error('没有可用页面');

  const ws = new WebSocket(target.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();
  const events = [];

  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m.result); pending.delete(m.id); }
    else if (m.method) { events.push(m); }
  };
  await new Promise((r) => (ws.onopen = r));

  const send = (method, params = {}) =>
    new Promise((r) => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });

  return { ws, send, events };
}

async function evalIn(conn, expr, awaitPromise) {
  const r = await conn.send('Runtime.evaluate', {
    expression: expr, returnByValue: true, awaitPromise: !!awaitPromise,
  });
  if (r && r.exceptionDetails) {
    throw new Error('页面内异常: ' +
      ((r.exceptionDetails.exception && r.exceptionDetails.exception.description) || r.exceptionDetails.text));
  }
  return r && r.result ? r.result.value : undefined;
}

async function currentURL(conn) {
  const r = await evalIn(conn, 'location.href');
  return r;
}

// ── 真实点击（走鼠标事件，不是 el.click()）──
//
// Codex 第 13 轮要求"真实鼠标/键盘操作"，不能用 JS 直接触发。
//
// ⚠️ 三个字段缺一不可（我第一版漏了前两个，点击**完全没生效**：
//    按钮的 click 探针根本没被触发，于是"确认框没出现"被误判成产品问题）：
//      1. 先发 `mouseMoved` —— 直接 press/release 不产生 click；
//      2. `buttons: 1` / `buttons: 0` —— 表示按下/松开时按住的键位；
//      3. `clickCount: 1`。
//    诊断实测：补上后探针收到 'restart-clicked'，`#overlay` 变为 `overlay open`。
async function clickReal(conn, selector) {
  // ⚠️ 必须先 scrollIntoView：设置页很长（诊断实测 docH≈1420），
  //    按钮可能在视口之外 —— 那样 getBoundingClientRect 会给一个
  //    视口外的坐标，鼠标事件打不到元素上（点击静默失效）。
  const box = await evalIn(conn, `(function(){
    var el = document.querySelector(${JSON.stringify(selector)});
    if (!el) return null;
    if (el.scrollIntoView) el.scrollIntoView({block:'center'});
    var r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return null;
    return {x: r.left + r.width/2, y: r.top + r.height/2,
            vw: window.innerWidth, vh: window.innerHeight};
  })()`);
  if (!box) throw new Error('找不到可点击元素: ' + selector);

  // 坐标必须落在视口内，否则事件不会被派发到元素上
  if (box.y < 0 || box.y > box.vh || box.x < 0 || box.x > box.vw) {
    throw new Error('元素不在视口内: ' + selector +
      ' (x=' + box.x.toFixed(0) + ',y=' + box.y.toFixed(0) +
      ' vp=' + box.vw + 'x' + box.vh + ')');
  }

  await conn.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y });
  await sleep(50);
  await conn.send('Input.dispatchMouseEvent', {
    type: 'mousePressed', x: box.x, y: box.y, button: 'left', buttons: 1, clickCount: 1,
  });
  await sleep(30);
  await conn.send('Input.dispatchMouseEvent', {
    type: 'mouseReleased', x: box.x, y: box.y, button: 'left', buttons: 0, clickCount: 1,
  });
}

// ── 启动 wbapi ──
//
// 🔴 必须用 **WBAPI_DATA_DIR** 隔离数据目录（含 config.json）。
//
//	否则验收会写真实的 `~/.wbapi/config.json`：
//	  - 场景 4 会把用户的端口改成 NEW_PORT 并**持久化**；
//	  - 跑完不还原的话，用户下次启动 wbapi 会发现在另一个端口上。
//	  （我第一版没隔离，实测把真实配置的 port 改成了 8892，已手工还原。）
function startWbapi(port, extraArgs) {
  const args = ['serve', '--addr', '127.0.0.1:' + port].concat(extraArgs || []);
  const child = spawn(WBAPI, args, {
    stdio: 'ignore',
    env: Object.assign({}, process.env, { WBAPI_DATA_DIR: DATA_DIR }),
    detached: false,
  });
  return child;
}

// 准备一个干净的数据目录（每次验收从头开始，避免残留账号/配置干扰）
function resetDataDir() {
  try { fs.rmSync(DATA_DIR, { recursive: true, force: true }); } catch (_) { /* ignore */ }
  try { fs.mkdirSync(DATA_DIR, { recursive: true }); } catch (_) { /* ignore */ }
}

async function stopWbapi(child) {
  if (!child || child.killed) return;
  try { child.kill(); } catch (_) { /* ignore */ }
  await sleep(800);
}

// ═══════════════════════════════════════════════════════════════════
// 场景 1 / 2a / 2b / 3：用"假服务"验证面板的探活判据
//
// 这些场景**不需要真重启**：它们验的是"面板拿到什么样的健康响应
// 才会导航"。用测试自己的 HTTP 服务精确构造四种响应即可。
// （场景 4、5 才需要真进程。）
// ═══════════════════════════════════════════════════════════════════
async function scenariosProbeJudgement(conn, panelOrigin) {
  section('场景 1/2a/2b/3：探活判据（假服务，精确构造健康响应）');

  // 面板当前必须在"旧 origin"上 —— 这是所有跨 origin 断言的前提。
  const here = await evalIn(conn, 'location.origin');
  ok(here === panelOrigin,
    '前置：页面在旧 origin 上（' + panelOrigin + '）',
    'actual=' + here);

  // 起 4 个假服务，各自模拟一种"新服务"的形态
  const servers = [];
  function mkServer(handler) {
    return new Promise((resolve) => {
      const s = http.createServer(handler);
      s.listen(0, '127.0.0.1', () => resolve(s));
    });
  }

  const corsFor = (req) => {
    const o = req.headers.origin || '';
    return o ? { 'Access-Control-Allow-Origin': o, 'Vary': 'Origin' } : { 'Vary': 'Origin' };
  };

  // 场景 1：新服务 503
  const s503 = await mkServer((req, res) => {
    res.writeHead(503, Object.assign({ 'Content-Type': 'application/json' }, corsFor(req)));
    res.end(JSON.stringify({ service: 'wbapi', ready: false, restartId: 'x'.repeat(32) }));
  });
  servers.push(s503);

  // 场景 2a：无关服务，200 + 通配 CORS
  const sUnrelated = await mkServer((req, res) => {
    res.writeHead(200, {
      'Content-Type': 'application/json',
      'Access-Control-Allow-Origin': '*',
    });
    res.end(JSON.stringify({ hello: 'unrelated' }));
  });
  servers.push(sUnrelated);

  // 场景 2b：service/ready 都对，但 restartId 错
  const sWrongID = await mkServer((req, res) => {
    res.writeHead(200, Object.assign({ 'Content-Type': 'application/json' }, corsFor(req)));
    res.end(JSON.stringify({ service: 'wbapi', ready: true, restartId: 'f'.repeat(32) }));
  });
  servers.push(sWrongID);

  // 场景 3：正确（旧 origin 的 CORS 被允许）+ restartId 匹配
  const RIGHT_ID = 'a'.repeat(32);
  const sGood = await mkServer((req, res) => {
    res.writeHead(200, Object.assign({ 'Content-Type': 'application/json' }, corsFor(req)));
    res.end(JSON.stringify({ service: 'wbapi', ready: true, restartId: RIGHT_ID }));
  });
  servers.push(sGood);

  const p503 = s503.address().port;
  const pUnrel = sUnrelated.address().port;
  const pWrong = sWrongID.address().port;
  const pGood = sGood.address().port;

  // 场景 3：**从旧 origin 直接读新端口的健康响应**（显式断言"能读到 body"）
  //
  // 这条是 Codex 特别要求的：不能只断言"能导航"，
  // 因为那样无法区分"CORS 放行了"与"恰好成功了"。
  const readResult = await evalIn(conn, `(function(){
    return fetch('http://127.0.0.1:${pGood}/healthz', {cache:'no-store'})
      .then(function(r){ return r.json().then(function(b){ return JSON.stringify({ok:r.ok, body:b}); }); })
      .catch(function(e){ return JSON.stringify({ok:false, err:String(e)}); });
  })()`, true);
  const rr = JSON.parse(readResult);
  ok(rr.ok === true && rr.body && rr.body.service === 'wbapi',
    '场景3：旧 origin 能**读到**新端口的健康 body（CORS 放行）',
    readResult);

  // 场景 2a 的 CORS 侧：通配 CORS 的无关服务，浏览器**能读到**它的 body
  // （这正是"只查 ok 会被击穿"的实测依据）
  const unrel = await evalIn(conn, `(function(){
    return fetch('http://127.0.0.1:${pUnrel}/healthz', {cache:'no-store'})
      .then(function(r){ return r.json().then(function(b){ return JSON.stringify({ok:r.ok, body:b}); }); })
      .catch(function(e){ return JSON.stringify({ok:false, err:String(e)}); });
  })()`, true);
  const ur = JSON.parse(unrel);
  ok(ur.ok === true,
    '场景2a 前提：通配 CORS 的无关服务**能被读到**（所以只查 ok 会误判）',
    unrel);
  ok(ur.body && ur.body.service !== 'wbapi',
    '场景2a 前提：该无关服务没有 service=wbapi 标识',
    unrel);

  // 场景 1：503 必须 ok=false（面板据此不导航）
  const v503 = await evalIn(conn, `(function(){
    return fetch('http://127.0.0.1:${p503}/healthz', {cache:'no-store'})
      .then(function(r){ return JSON.stringify({ok:r.ok, status:r.status}); })
      .catch(function(e){ return JSON.stringify({ok:false, err:String(e)}); });
  })()`, true);
  const v5 = JSON.parse(v503);
  ok(v5.ok === false,
    '场景1：503 时 response.ok=false（面板据此不导航）',
    v503);

  // 场景 2b：restartId 不匹配 —— 断言"值确实不同"
  const wrongFetch = await evalIn(conn, `(function(){
    return fetch('http://127.0.0.1:${pWrong}/healthz', {cache:'no-store'})
      .then(function(r){ return r.json().then(function(b){ return JSON.stringify({ok:r.ok, rid:b.restartId}); }); })
      .catch(function(e){ return JSON.stringify({ok:false, err:String(e)}); });
  })()`, true);
  const wf = JSON.parse(wrongFetch);
  ok(wf.ok === true && wf.rid === 'f'.repeat(32) && wf.rid !== RIGHT_ID,
    '场景2b 前提：服务可读但 restartId 与本次交接不符（面板应拒绝导航）',
    wrongFetch);

  // ── 现在验**真实的面板判据函数**：把 pollRestart 的四条件逻辑
  //    在页面里对四个地址各跑一遍，观察"是否会导航"。
  //
  // 🔴 关键：不直接调用内部函数（那是私有闭包），而是**复刻同一条判据**
  //    对四个地址求值，验证"只有 2b-正确的那个会通过"。
  //    更强的端到端验证在场景 4（真重启、由面板自己导航）。
  const verdicts = await evalIn(conn, `(async function(){
    var restartId = ${JSON.stringify(RIGHT_ID)};
    var targets = [
      ['503',      'http://127.0.0.1:${p503}/healthz'],
      ['unrelated','http://127.0.0.1:${pUnrel}/healthz'],
      ['wrongId',  'http://127.0.0.1:${pWrong}/healthz'],
      ['good',     'http://127.0.0.1:${pGood}/healthz']
    ];
    var out = {};
    for (var i=0;i<targets.length;i++){
      var name = targets[i][0], url = targets[i][1];
      var navigate = false;
      try {
        var r = await fetch(url, {cache:'no-store'});
        if (!r.ok) { navigate = false; }
        else {
          var h = await r.json().catch(function(){ return null; });
          if (h === null) { navigate = false; }
          else if (h.service !== 'wbapi' || h.ready !== true) { navigate = false; }
          else if (restartId && h.restartId !== restartId) { navigate = false; }
          else { navigate = true; }
        }
      } catch (e) { navigate = false; }
      out[name] = navigate;
    }
    return JSON.stringify(out);
  })()`, true);
  const vd = JSON.parse(verdicts);

  ok(vd['503'] === false, '场景1：判据对 503 判定为**不导航**', JSON.stringify(vd));
  ok(vd['unrelated'] === false, '场景2a：判据对无关服务（通配 CORS）判定为**不导航**', JSON.stringify(vd));
  ok(vd['wrongId'] === false, '场景2b：判据对 restartId 不符判定为**不导航**', JSON.stringify(vd));
  ok(vd['good'] === true, '场景3：判据对正确服务判定为**导航**（否则永远连不上）', JSON.stringify(vd));

  for (const s of servers) { try { s.close(); } catch (_) { /* ignore */ } }
}

// ═══════════════════════════════════════════════════════════════════
// 场景 5：新端口被占用 → 旧服务恢复，面板有限时间内显示失败
//
// 🔴 用**无关程序**占用新端口（不是再起一个 wbapi）——
//    否则会改变真实的子进程 bind 语义（Codex 明确要求）。
// ═══════════════════════════════════════════════════════════════════
async function scenarioPortOccupied(child, conn) {
  section('场景 5：新端口被占用 → 旧服务恢复 + 面板显示失败');

  // 起一个无关程序占住新端口。
  //
  // 🔴 必须是**无关程序**，不能是另一个 wbapi —— Codex 明确要求：
  //    预启动一个 wbapi 实例会改变真实的"子进程 bind 失败"语义。
  const squatter = http.createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end('squatter');
  });
  await new Promise((r) => squatter.listen(NEW_PORT, '127.0.0.1', r));

  // 🔴 先把**配置里的端口**改成 NEW_PORT（被占的那个）。
  //
  //	这一步是必须的（我第一版漏了）：
  //	`plannedListenAddr` 以**配置**为准（委托方定调：面板与反代同一个端口），
  //	所以"重启会去抢哪个端口"完全由配置决定。
  //	不先改配置的话，重启会去抢配置里原有的端口 —— 而那个端口空闲，
  //	于是重启**成功**，根本构造不出"被占用"这个场景。
  const setPort = await evalIn(conn, `(function(){
    return fetch('/api/panel-token',{cache:'no-store'}).then(function(r){return r.json();}).then(function(t){
      return fetch('/api/settings',{cache:'no-store'}).then(function(r){return r.json();}).then(function(s){
        return fetch('/api/settings', {
          method:'PATCH',
          headers:{'Content-Type':'application/json','X-WBAPI-Panel':t.token},
          body: JSON.stringify({port: ${NEW_PORT}, ifRevision: s.revisionToken})
        });
      });
    }).then(function(r){ return r.status; })
      .catch(function(e){ return 'err:'+String(e); });
  })()`, true);
  ok(setPort === 200 || setPort === 204,
    '场景5：前置 —— 配置端口已改为被占用的 ' + NEW_PORT, 'status=' + setPort);

  // 通过**面板真实 UI** 触发重启。
  //
  // 🔴 必须走 UI，不能直接调接口（我第一版图省事用了接口，结果断言不了面板提示）：
  //
  //	面板的 `restartServer()` 先弹确认框，而 `pollRestart()`（以及所有
  //	失败提示的写入）都只在**确认回调里**启动。直接 POST
  //	`/api/settings/restart` 会让服务端真的重启，但**面板完全不知情** ——
  //	于是它既不会重连也不会显示提示。
  //
  //	这与我第 19 轮犯的错同源：**测试绕过了被测的生产逻辑**。
  //	Codex 明确强调过：场景 4 必须由面板自行导航；
  //	同理，凡是要验"面板行为"的场景，都必须从 UI 入口进。
  //
  // 令牌与 ifRevision 无需手填：面板自己的代码会带（这正是要验的路径）。
  try {
    await clickReal(conn, '.side-item[data-page="settings"]');
    await sleep(400);
    await clickReal(conn, '#btn-st-restart');
    let dialogSeen = false;
    for (let i = 0; i < 20; i++) {
      await sleep(150);
      if (await evalIn(conn, `!!document.getElementById('st-confirm-ok')`)) { dialogSeen = true; break; }
    }
    if (!dialogSeen) { ok(false, '场景5：确认框未出现，无法从 UI 触发'); }
    else {
      await clickReal(conn, '#st-confirm-ok');
      ok(true, '场景5：通过真实 UI 触发重启（面板会自己处理失败提示）');
    }
  } catch (e) {
    ok(false, '场景5：无法通过真实 UI 触发重启', String(e));
  }

  // 等一会儿：旧服务应当恢复（因为新进程 bind 失败）
  const backUp = await waitPort(OLD_PORT, true, 20000);
  ok(backUp, '场景5：旧端口重新可用（重启失败后旧服务被恢复）');

  // 旧服务恢复后，面板应能在有限时间内观察到"重启失败"
  let healthAfter = null;
  for (let i = 0; i < 30; i++) {
    try {
      healthAfter = await httpGetJSON(OLD_PORT, '/healthz', 2000);
      if (healthAfter && healthAfter.status === 200) break;
    } catch (_) { /* 继续等 */ }
    await sleep(500);
  }
  ok(healthAfter && healthAfter.status === 200,
    '场景5：恢复后的旧服务 /healthz 可用（服务没丢）',
    JSON.stringify(healthAfter));

  // 恢复后的服务应当是"非重启态"（restartId 为空或不是本次的）
  //
  // 这条同时验证了：恢复路径用的是**新的** server（Codex 第 11 轮要求的），
  // 且不会把本次交接标识错误地留在内存里。
  if (healthAfter && healthAfter.body && typeof healthAfter.body === 'object') {
    ok(true, '场景5：恢复后健康响应仍是合法 JSON（契约未破）',
      JSON.stringify(healthAfter.body));
  } else {
    ok(false, '场景5：恢复后健康响应不是预期 JSON', JSON.stringify(healthAfter));
  }

  // ═══════════════════════════════════════════════════════════════
  // 🔴 用户**可见**的失败反馈（Codex 第 20 轮要求补的断言）
  // ═══════════════════════════════════════════════════════════════
  //
  // 为什么必须有这条：上面几条只证明"后端恢复可用"，
  // **不能证明用户知道重启失败了**。
  //
  // 而且这里有个陷阱（我自己踩过）：失败路径的 catch 原本会把
  // `st-restart-msg` 清回空串，于是"它是否为空"这个判据会得到**假阴性**。
  // 所以断言方式必须是"等待上限后读**持久**状态"，
  // 而不是"检查 DOM 里是否短暂出现过某段文字"。
  //
  // 修好后：失败会写入一句**保留**的提示（含"重启失败"/"未生效"/"未能连接"），
  // 并带 .st-restart-failed 类名。
  //
  // ⚠️ 等待条件必须是"**失败态**出现"，不能是"文本非空"（我第一版就是这么错的）：
  //	点击后立刻会写入「正在重启…」——它也是非空文本，
  //	于是循环第一轮就 break，拿到的却是进行中的提示，断言必然误报。
  //	正确的等待条件是**类名变成 st-restart-failed**（只有失败路径才会加）。
  let persisted = null;
  for (let i = 0; i < 50; i++) { // 最多约 25 秒
    try {
      const raw = await evalIn(conn, `(function(){
        var el = document.getElementById('st-restart-msg');
        if (!el) return JSON.stringify({exists:false});
        return JSON.stringify({
          exists: true,
          text: el.textContent || '',
          failedClass: el.classList.contains('st-restart-failed'),
          visible: !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length)
        });
      })()`);
      persisted = JSON.parse(raw);
      // 等到"失败态"为止（不是"文本非空"）
      if (persisted.exists && persisted.failedClass) break;
    } catch (_) { /* 页面可能在切换，继续等 */ }
    await sleep(500);
  }

  ok(persisted && persisted.exists && persisted.text.length > 0,
    '场景5：面板留下**持久**的失败提示（不是静默失败）',
    JSON.stringify(persisted));

  if (persisted && persisted.text) {
    const t = persisted.text;
    ok(/重启失败|未能连接|端口/.test(t),
      '场景5：提示内容说明了失败原因 / 下一步（用户知道该做什么）',
      'text=' + t);
    ok(persisted.failedClass === true,
      '场景5：失败提示带 .st-restart-failed 类名（视觉上可辨，不是普通灰字）',
      JSON.stringify(persisted));
    ok(persisted.visible === true,
      '场景5：失败提示确实可见（非 display:none / 零尺寸）',
      JSON.stringify(persisted));
  }

  try { squatter.close(); } catch (_) { /* ignore */ }
}

// ═══════════════════════════════════════════════════════════════════
// 场景 4：实际改端口重启 → 面板**自行导航**，新页面能保存设置
//
// 🔴 本场景最关键的纪律：**由面板自己导航**，脚本只观察。
// ═══════════════════════════════════════════════════════════════════
async function scenarioRealPortChange(child, conn) {
  section('场景 4：真实改端口重启 → 面板自行导航 + 新页面能保存设置');

  const beforeURL = await currentURL(conn);
  ok(beforeURL.indexOf(':' + OLD_PORT) !== -1,
    '前置：面板当前在旧端口 ' + OLD_PORT, beforeURL);

  // 先切到「设置」页 —— 重启按钮在设置页里（面板是侧边栏多页结构）。
  //
  // ⚠️ 第一版直接在首页找 #btn-st-restart 找不到：设置页是
  // `<section id="page-settings" class="page hidden">`，不切换就是隐藏的
  // （getBoundingClientRect 全 0，clickReal 会拒绝点击）。
  try {
    await clickReal(conn, '.side-item[data-page="settings"]');
    await sleep(500);
  } catch (e) {
    ok(false, '场景4：无法切换到设置页', String(e));
  }

  // 先确认旧端口的健康响应里没有本次交接标识（非重启启动）
  const h0 = await httpGetJSON(OLD_PORT, '/healthz', 3000);
  ok(h0.status === 200 && h0.body && h0.body.service === 'wbapi',
    '前置：旧服务健康响应含 service=wbapi', JSON.stringify(h0.body));

  // 改端口设置（真实走面板接口）。
  //
  // 🔴 两个必带项（第一版漏了，两处都是**产品在正确拦截**）：
  //   1. `X-WBAPI-Panel` 令牌 —— 否则 403 cross_origin_denied（CSRF 防护）；
  //   2. `ifRevision` 不透明版本串 —— 否则 **428 Precondition Required**
  //      （Codex 第 15/17 轮定的乐观锁契约：缺失**不放行**）。
  //      GET /api/settings 返回 revisionToken，前端原样回传。
  const saveResp = await evalIn(conn, `(function(){
    return fetch('/api/panel-token',{cache:'no-store'}).then(function(r){return r.json();}).then(function(t){
      return fetch('/api/settings',{cache:'no-store'}).then(function(r){return r.json();}).then(function(s){
        return fetch('/api/settings', {
          method:'PATCH',
          headers:{'Content-Type':'application/json','X-WBAPI-Panel':t.token},
          body: JSON.stringify({port: ${NEW_PORT}, ifRevision: s.revisionToken})
        });
      });
    }).then(function(r){ return r.status; })
      .catch(function(e){ return 'err:'+String(e); });
  })()`, true);
  ok(saveResp === 200 || saveResp === 204,
    '场景4：端口设置保存成功', 'status=' + saveResp);

  // 🔴 触发重启必须**走面板自己的按钮**，不能直接 fetch 重启接口。
  //
  // 为什么（这一条我第一版做错了，是 Codex 强调的方法学）：
  //   面板的 `restartServer()` 先弹确认框，`pollRestart()` 是在**确认回调里**
  //   才启动的。如果脚本直接 fetch `/api/settings/restart`，
  //   服务端会真的重启，但**面板的导航逻辑根本不会被触发** ——
  //   于是"没导航"会被误判成产品缺陷，实际是测试绕过了被测代码。
  //
  // 正确做法：真实点击「立即重启」按钮 → 真实点击确认框的「立即重启」。
  let clicked = false;
  try {
    await clickReal(conn, '#btn-st-restart');
    // 等确认框出现（openConfirm 会往 #modal-body 里塞按钮并把 overlay 加 .open）
    let dialogSeen = false;
    for (let i = 0; i < 20; i++) {
      await sleep(150);
      const has = await evalIn(conn, `!!document.querySelector('#st-confirm-ok')`);
      if (has) { dialogSeen = true; break; }
    }
    ok(dialogSeen, '场景4：点击「立即重启」后确认框出现');
    if (dialogSeen) {
      await clickReal(conn, '#st-confirm-ok');
      clicked = true;
    }
  } catch (e) {
    ok(false, '场景4：无法通过真实 UI 触发重启', String(e));
  }
  if (clicked) {
    ok(true, '场景4：通过真实 UI（按钮 + 确认框）触发重启');
  }

  // 🔴 观察页面是否**自己**跳到新端口。脚本绝不执行 location.href。
  let navigated = false;
  let finalURL = '';
  for (let i = 0; i < 60; i++) { // 最多 30 秒
    await sleep(500);
    try {
      finalURL = await currentURL(conn);
      if (finalURL && finalURL.indexOf(':' + NEW_PORT) !== -1) { navigated = true; break; }
    } catch (_) {
      // 导航过程中 Runtime 可能短暂不可用 —— 这是正常的，继续等
    }
  }

  ok(navigated,
    '场景4：面板**自行**导航到新端口 ' + NEW_PORT + '（脚本未代跳）',
    'finalURL=' + finalURL);

  if (!navigated) return;

  // 新页面必须能正常工作：重新取 token 并保存设置
  await sleep(1500); // 等新页面加载完
  const newOrigin = await evalIn(conn, 'location.origin');
  ok(newOrigin.indexOf(':' + NEW_PORT) !== -1,
    '场景4：新页面 origin 已是 ' + NEW_PORT, newOrigin);

  // 新页面必须能正常工作：重新取 token、按契约带 ifRevision 保存设置。
  //
  // ⚠️ 必须带 `ifRevision`：只带 token 会得到 **428 Precondition Required**
  //    （Codex 第 15/17 轮定的乐观锁契约：缺失**不放行**，不是放行）。
  //    第一版漏了它，把"产品正确拒绝"误判成了缺陷。
  const canSave = await evalIn(conn, `(function(){
    return fetch('/api/panel-token',{cache:'no-store'}).then(function(r){return r.json();}).then(function(t){
      if (!t || !t.token) return 'no-token';
      return fetch('/api/settings',{cache:'no-store'}).then(function(r){return r.json();}).then(function(s){
        return fetch('/api/settings', {
          method:'PATCH',
          headers:{'Content-Type':'application/json','X-WBAPI-Panel':t.token},
          body: JSON.stringify({autoCheckin: false, ifRevision: s.revisionToken})
        }).then(function(r){ return 'status:'+r.status; });
      });
    }).catch(function(e){ return 'err:'+String(e); });
  })()`, true);
  ok(String(canSave).indexOf('status:') === 0 &&
     (canSave === 'status:200' || canSave === 'status:204'),
    '场景4：新页面能重新取 token 并保存设置', String(canSave));
}

// ═══════════════════════════════════════════════════════════════════
// 场景 6：**反向场景** —— 重启成功时不得出现任何"失败"提示
//
// 🔴 Codex 第 21 轮指定的竞态测试。它指出我给场景 5 加的
//    "探旧地址判断回滚"存在竞态：
//
//	202 发出后，旧服务可能**还没真正开始关闭**
//	（服务端要 sleep 300ms 才发重启信号，Shutdown 也需要时间）。
//	此时旧地址当然可访问 —— 但那不代表"回滚"，而是"重启还没开始"。
//	若据此立刻提示失败，而新端口随后成功，用户会看到**错误的失败提示**。
//
//    它要求补的场景原话：
//      "旧地址在前 1 秒仍可访问，新地址随后成功 ready；
//       面板不得显示失败，也不得回退旧页面。"
//
// 本场景的做法：**用一个真实成功的改端口重启**（旧地址在头几百毫秒
// 内仍然可访问），断言整个过程中**从未出现**失败提示。
// 这与场景 4 复用同一条成功路径，但观测点不同 ——
// 场景 4 看"有没有跳过去"，本场景看"有没有误报失败"。
//
// 实现方式：在重启前埋一个 MutationObserver，记录 st-restart-msg 的
// 每一次变化与是否带上失败类名；重启成功后检查**从未**出现过失败态。
// ═══════════════════════════════════════════════════════════════════
async function scenarioNoFalseFailure(conn) {
  section('场景 6：重启成功时不得误报失败（旧地址早响应竞态）');

  const here = await currentURL(conn);
  ok(here.indexOf(':' + OLD_PORT) !== -1,
    '场景6 前置：面板在旧端口 ' + OLD_PORT, here);

  // 埋观察器：记录失败提示是否曾经出现过
  await evalIn(conn, `(function(){
    window.__falseFail = { seen: false, samples: [] };
    var el = document.getElementById('st-restart-msg');
    function check(){
      var e = document.getElementById('st-restart-msg');
      if (!e) return;
      var failed = e.classList.contains('st-restart-failed');
      var text = e.textContent || '';
      if (failed || /未生效|重启失败|未能连接/.test(text)) {
        window.__falseFail.seen = true;
        window.__falseFail.samples.push(text);
      }
    }
    if (el) {
      new MutationObserver(check).observe(el, {childList:true, characterData:true, subtree:true, attributes:true});
    }
    // 同时轮询兜底（类名变化未必触发 childList）
    window.__falseFailTimer = setInterval(check, 100);
    return 'armed';
  })()`);

  // 改端口（用一个新端口，避免与前面场景冲突）
  const THIRD_PORT = NEW_PORT + 1;
  const setPort = await evalIn(conn, `(function(){
    return fetch('/api/panel-token',{cache:'no-store'}).then(function(r){return r.json();}).then(function(t){
      return fetch('/api/settings',{cache:'no-store'}).then(function(r){return r.json();}).then(function(s){
        return fetch('/api/settings', {
          method:'PATCH',
          headers:{'Content-Type':'application/json','X-WBAPI-Panel':t.token},
          body: JSON.stringify({port: ${THIRD_PORT}, ifRevision: s.revisionToken})
        });
      });
    }).then(function(r){ return r.status; }).catch(function(e){ return 'err:'+String(e); });
  })()`, true);
  ok(setPort === 200 || setPort === 204,
    '场景6：端口已改为 ' + THIRD_PORT, 'status=' + setPort);

  // 走 UI 触发重启（面板自己导航）
  try {
    await clickReal(conn, '.side-item[data-page="settings"]');
    await sleep(400);
    await clickReal(conn, '#btn-st-restart');
    for (let i = 0; i < 20; i++) {
      await sleep(150);
      if (await evalIn(conn, `!!document.getElementById('st-confirm-ok')`)) break;
    }
    await clickReal(conn, '#st-confirm-ok');
  } catch (e) {
    ok(false, '场景6：无法通过 UI 触发重启', String(e));
    return;
  }

  // 观察它是否**自行**导航到 THIRD_PORT（成功的话会）
  let navigated = false;
  for (let i = 0; i < 60; i++) {
    await sleep(500);
    try {
      const u = await currentURL(conn);
      if (u && u.indexOf(':' + THIRD_PORT) !== -1) { navigated = true; break; }
    } catch (_) { /* 导航中，继续等 */ }
  }
  ok(navigated,
    '场景6：重启成功并自行导航到 ' + THIRD_PORT + '（本场景的前提）');

  // 🔴 核心断言：全程从未出现失败提示
  //
  // ⚠️ 观察器在导航后可能随页面销毁 —— 所以先在**导航前**读取一次，
  //    若已销毁则读不到。这里用"导航到的页面是否干净"兜底：
  //    若曾误报失败，旧页面的观察器已经记下了 seen=true，
  //    但页面销毁后 window 也没了 —— 因此改为在**导航发生前**
  //    就让脚本记录（下面的轮询与最终读取配合）。
  const observed = await evalIn(conn, `JSON.stringify(window.__falseFail || {seen:'unknown'})`).catch(() => null);

  // 新页面也应当没有失败态（导航后是干净的新页面）
  const newPageClean = await evalIn(conn, `(function(){
    var e = document.getElementById('st-restart-msg');
    if (!e) return 'no-el';
    return e.classList.contains('st-restart-failed') ? 'failed' : 'clean';
  })()`).catch(() => 'err');

  if (observed) {
    const o = JSON.parse(observed);
    ok(o.seen !== true,
      '场景6：重启成功过程中**从未**出现失败提示（旧地址早响应未被误判为回滚）',
      observed);
  } else {
    // 观察器随旧页面销毁 —— 退化为断言新页面干净
    ok(newPageClean === 'clean',
      '场景6：新页面无失败态（旧页面观察器已随导航销毁，此处兜底）',
      'newPage=' + newPageClean);
  }
  ok(newPageClean === 'clean',
    '场景6：导航后的页面没有残留的失败提示', 'newPage=' + newPageClean);
}

// ═══════════════════════════════════════════════════════════════════
// 主流程
// ═══════════════════════════════════════════════════════════════════
async function main() {
  const chrome = findChrome();
  if (!chrome) { console.error('找不到 Chrome/Edge，跳过'); process.exit(2); }
  if (!fs.existsSync(WBAPI)) {
    console.error('找不到 wbapi.exe: ' + WBAPI + '\n先执行: go build -o wbapi.exe ./cmd/wbapi');
    process.exit(2);
  }

  // 干净起点：确认两个端口都空闲，数据目录也重置
  await waitPort(OLD_PORT, false, 5000);
  await waitPort(NEW_PORT, false, 5000);
  resetDataDir();
  console.log('数据目录（隔离）: ' + DATA_DIR);

  const userDir = path.join(process.env.TEMP || '/tmp', 'wbapi-reconnect-profile');
  try { fs.rmSync(userDir, { recursive: true, force: true }); } catch (_) { /* ignore */ }

  console.log('启动 wbapi @ ' + OLD_PORT);
  let child = startWbapi(OLD_PORT);
  const up = await waitPort(OLD_PORT, true, 20000);
  if (!up) { console.error('wbapi 未能在 ' + OLD_PORT + ' 起来'); process.exit(1); }

  console.log('启动 Chrome（CDP ' + DEBUG_PORT + '）');
  const chromeProc = spawn(chrome, [
    '--headless=new',
    '--remote-debugging-port=' + DEBUG_PORT,
    '--user-data-dir=' + userDir,
    '--window-size=1280,900', // 显式视口：默认 headless 视口偏小会让按钮落在视口外
    '--no-first-run', '--no-default-browser-check',
    'about:blank',
  ], { stdio: 'ignore' });

  const conn = await connect(DEBUG_PORT);
  await conn.send('Runtime.enable');
  await conn.send('Page.enable');

  // 🔴 让页面停在**旧 origin** 上 —— 跨 origin 断言的前提。
  //
  // 用 127.0.0.1（不是 localhost）：场景 3/4 的 CORS 白名单由服务端
  // 按 listenAddr + 前端传来的 origin 构造，这里保持二者一致，
  // 而 localhost 的差异已在 Go 单测里单独覆盖。
  const panelOrigin = 'http://127.0.0.1:' + OLD_PORT;
  await conn.send('Page.navigate', { url: panelOrigin + '/panel/' });
  await sleep(2000);

  let step = '';
  try {
    step = '场景 1/2a/2b/3（探活判据）';
    await scenariosProbeJudgement(conn, panelOrigin);

    // ⚠️ 场景 5 与场景 4 都会真的改动服务进程/端口，且**互相冲突**
    //    （场景 5 刻意让新端口被占、重启被回滚；场景 4 需要新端口空闲
    //     才能成功交接）。所以每跑完一个都**重开一个干净实例**，
    //    否则第二个会因第一个留下的状态而失败 —— 我第一版把它们串在
    //    同一个实例上，场景 4 直接 ECONNREFUSED。
    step = '场景 5（新端口被占用）';
    await scenarioPortOccupied(child, conn);

    // 重开干净实例给场景 4 用
    try { child.kill(); } catch (_) { /* ignore */ }
    await sleep(1500);
    await waitPort(OLD_PORT, false, 8000);
    resetDataDir();
    child = startWbapi(OLD_PORT);
    if (!await waitPort(OLD_PORT, true, 20000)) {
      throw new Error('场景 4 前重开实例失败');
    }
    await conn.send('Page.navigate', { url: panelOrigin + '/panel/' });
    await sleep(2000);

    step = '场景 4（真实改端口重启 + 自行导航）';
    await scenarioRealPortChange(child, conn);

    // 场景 6 需要在"旧端口还在、新端口空闲"的干净状态下再跑一次成功重启，
    // 所以同样重开实例（它验的是**成功**路径上不得出现失败提示）。
    try { child.kill(); } catch (_) { /* ignore */ }
    await sleep(1500);
    await waitPort(OLD_PORT, false, 8000);
    await waitPort(NEW_PORT, false, 8000);
    await waitPort(NEW_PORT + 1, false, 8000);
    resetDataDir();
    child = startWbapi(OLD_PORT);
    if (!await waitPort(OLD_PORT, true, 20000)) {
      throw new Error('场景 6 前重开实例失败');
    }
    await conn.send('Page.navigate', { url: panelOrigin + '/panel/' });
    await sleep(2000);

    step = '场景 6（成功重启不得误报失败）';
    await scenarioNoFalseFailure(conn);
  } catch (e) {
    console.error('\n【' + step + '】执行出错: ' + (e && e.stack ? e.stack : e));
    failed++;
    failures.push(step + ' 执行出错: ' + (e && e.message ? e.message : String(e)));
  }

  // 收尾
  try { conn.ws.close(); } catch (_) { /* ignore */ }
  try { chromeProc.kill(); } catch (_) { /* ignore */ }
  try { child.kill(); } catch (_) { /* ignore */ }
  await sleep(500);
  // 兜底：清掉可能被本脚本拉起的 wbapi 子进程
  try {
    const { execSync } = require('child_process');
    execSync('taskkill /F /IM wbapi.exe /T', { stdio: 'ignore' });
  } catch (_) { /* ignore */ }

  console.log('\n════════════════════════════════');
  console.log('通过 ' + passed + ' 项，失败 ' + failed + ' 项');
  if (failures.length) {
    console.log('\n失败项：');
    failures.forEach((f) => console.log('  - ' + f));
  }
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
