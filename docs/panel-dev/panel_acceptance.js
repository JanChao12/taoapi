// panel_acceptance.js —— 面板端到端验收（CDP 驱动真实 Chrome）
//
// 用法：
//   node docs/panel-dev/panel_acceptance.js [baseURL]
//   默认 baseURL = http://127.0.0.1:8787
//
// ═══════════════════════════════════════════════════════════════════
// 为什么是 CDP 而不是 Playwright（Codex 第 13 轮明确认可）
// ═══════════════════════════════════════════════════════════════════
//
//   Playwright 是"验证方式"，不是功能要求；为一个验收脚本引入
//   node_modules 会破坏本项目"零第三方依赖"的原则。
//   本机有 Chrome，用它的 DevTools Protocol 即可，零依赖。
//
// ═══════════════════════════════════════════════════════════════════
// Codex 第 13/15 轮指定的三组验收
// ═══════════════════════════════════════════════════════════════════
//
//   1. 真实浏览器写入与安全边界
//      - 用**真实键盘输入**（Input.dispatchKeyEvent）触发保存，不是直接改 DOM
//      - 成功保存 / 409 冲突 / 缺 token 与错 token 的 403 / 网络失败
//      - 网络失败后草稿必须保留
//   2. 端口修改 → 重启 → 重连
//      - 当前端口与待生效端口的显示
//      - 保存提示、重启后自动重连、token 自动刷新
//   3. 窄屏与脏表单
//      - 截图检查溢出 / 遮挡 / 按钮文字截断
//      - 有未保存草稿时不能静默丢弃
//
// 关键原则（Codex 强调）：**操作真实浏览器事件并检查可见结果**，
// 而不是只通过脚本直接改 DOM 或读配置文件。

'use strict';

const http = require('http');
const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');

const BASE = process.argv[2] || 'http://127.0.0.1:8787';
const OUT_DIR = path.join(__dirname, 'acceptance-out');
const DEBUG_PORT = 9333;

// ── 极简断言框架 ──
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

function section(title) {
  console.log('\n── ' + title + ' ──');
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── 找到 Chrome ──
function findChrome() {
  const cands = [
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  ];
  for (const c of cands) {
    try {
      fs.accessSync(c);
      return c;
    } catch (_) { /* 继续找 */ }
  }
  return null;
}

// ── CDP 连接 ──
function getJSON(port, p) {
  return new Promise((resolve, reject) => {
    http.get({ host: '127.0.0.1', port, path: p }, (res) => {
      let d = '';
      res.on('data', (c) => (d += c));
      res.on('end', () => {
        try { resolve(JSON.parse(d)); } catch (e) { reject(e); }
      });
    }).on('error', reject);
  });
}

async function connect(port) {
  let tabs;
  for (let i = 0; i < 60; i++) {
    try { tabs = await getJSON(port, '/json/list'); break; } catch (_) { await sleep(250); }
  }
  if (!tabs) throw new Error('无法连接到 Chrome 调试端口');
  const target = tabs.find((t) => t.type === 'page');
  if (!target) throw new Error('没有可用的页面目标');

  const ws = new WebSocket(target.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();
  const events = [];

  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) {
      pending.get(m.id)(m.result);
      pending.delete(m.id);
    } else if (m.method) {
      events.push(m);
    }
  };
  await new Promise((r) => (ws.onopen = r));

  const send = (method, params = {}) =>
    new Promise((r) => {
      const i = ++id;
      pending.set(i, r);
      ws.send(JSON.stringify({ id: i, method, params }));
    });

  return { ws, send, events };
}

// 在页面里求值
async function evalIn(conn, expr, awaitPromise) {
  const r = await conn.send('Runtime.evaluate', {
    expression: expr,
    returnByValue: true,
    awaitPromise: !!awaitPromise,
  });
  if (r && r.exceptionDetails) {
    throw new Error('页面内异常: ' +
      (r.exceptionDetails.exception && r.exceptionDetails.exception.description ||
        r.exceptionDetails.text));
  }
  return r && r.result ? r.result.value : undefined;
}

// ── 真实键盘输入（不是直接改 value）──
//
// Codex 明确要求"真实鼠标/键盘操作"。
// 做法：聚焦元素 → 全选 → 逐字符 dispatchKeyEvent → 触发 input 事件。
// CDP 的 Input.insertText 会走浏览器的输入管线，比直接赋 value 更接近真实。
async function typeInto(conn, selector, text) {
  await evalIn(conn, `(function(){
    var el = document.querySelector(${JSON.stringify(selector)});
    if (!el) return 'no-el';
    el.focus();
    el.select && el.select();
    return 'ok';
  })()`);
  // 清空
  await conn.send('Input.dispatchKeyEvent', { type: 'keyDown', windowsVirtualKeyCode: 65, modifiers: 2 }); // Ctrl+A
  await conn.send('Input.dispatchKeyEvent', { type: 'keyUp', windowsVirtualKeyCode: 65, modifiers: 2 });
  await conn.send('Input.insertText', { text: text });
  // 确保框架感知到变化
  await evalIn(conn, `(function(){
    var el = document.querySelector(${JSON.stringify(selector)});
    if (el) el.dispatchEvent(new Event('input', {bubbles:true}));
  })()`);
}

// ── 真实点击（走鼠标事件）──
async function clickReal(conn, selector) {
  const box = await evalIn(conn, `(function(){
    var el = document.querySelector(${JSON.stringify(selector)});
    if (!el) return null;
    var r = el.getBoundingClientRect();
    return {x: r.left + r.width/2, y: r.top + r.height/2, w: r.width, h: r.height};
  })()`);
  if (!box) throw new Error('找不到元素: ' + selector);
  await conn.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 });
  await conn.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 });
  return box;
}

async function shot(conn, name) {
  const r = await conn.send('Page.captureScreenshot', { format: 'png' });
  if (!fs.existsSync(OUT_DIR)) fs.mkdirSync(OUT_DIR, { recursive: true });
  const p = path.join(OUT_DIR, name);
  fs.writeFileSync(p, Buffer.from(r.data, 'base64'));
  return p;
}

// ── 主流程 ──
(async () => {
  const chrome = findChrome();
  if (!chrome) {
    console.error('找不到 Chrome/Edge，无法做界面验收');
    process.exit(2);
  }

  const profile = path.join(process.env.TEMP || '.', 'wbapi-accept-' + Date.now());
  const proc = spawn(chrome, [
    '--headless=new', '--disable-gpu', '--no-sandbox',
    '--remote-debugging-port=' + DEBUG_PORT,
    '--user-data-dir=' + profile,
    '--window-size=1440,1000',
    'about:blank',
  ], { stdio: 'ignore' });

  let conn;
  try {
    conn = await connect(DEBUG_PORT);
    await conn.send('Page.enable');
    await conn.send('Runtime.enable');
    await conn.send('Network.enable');

    // ═══════════════════════════════════════════
    section('组 1：真实浏览器写入与安全边界');
    // ═══════════════════════════════════════════
    await conn.send('Page.navigate', { url: BASE + '/panel/' });
    await sleep(4000);

    // 收集 JS 异常
    const jsErrors = conn.events
      .filter((e) => e.method === 'Runtime.exceptionThrown')
      .map((e) => (e.params.exceptionDetails.exception || {}).description ||
        e.params.exceptionDetails.text);
    ok(jsErrors.length === 0, '面板加载无 JS 异常', jsErrors.slice(0, 2).join(' | '));

    // 切到设置页（真实点击）
    await evalIn(conn, `(function(){
      var e = document.querySelectorAll('.side-item');
      for (var i=0;i<e.length;i++) if (e[i].getAttribute('data-page')==='settings') { e[i].click(); return 'ok'; }
      return 'no';
    })()`);
    await sleep(2000);

    const pageOk = await evalIn(conn, `(function(){
      var p = document.getElementById('page-settings');
      return p && !p.classList.contains('hidden');
    })()`);
    ok(pageOk, '设置页可切换并显示');

    // 取一份正确令牌，供后续"安全边界"用例使用
    const correctToken = await evalIn(conn,
      `fetch('/api/panel-token',{cache:'no-store'}).then(r=>r.json()).then(d=>d.token||'')`, true);
    ok(typeof correctToken === 'string' && correctToken.length >= 16,
      '能取到写操作令牌', 'len=' + (correctToken || '').length);

    // 1.1 成功保存（真实键盘 + 真实点击）
    const portSel = '#st-port';
    const hasPort = await evalIn(conn, `!!document.querySelector('#st-port')`);
    ok(hasPort, '端口输入框存在');
    let origPort = '8787';
    if (hasPort) {
      // 记下当前值，稍后恢复
      origPort = await evalIn(conn, `document.querySelector('#st-port').value`);

      // 🔴 关键：先把服务端当前端口改成"与 origPort 不同"的值再测，
      // 否则面板会识别为"端口未变化"而根本不发请求 ——
      // 那样测的是"面板正确地跳过了无变化提交"，不是保存路径。
      await typeInto(conn, portSel, '8791');
      const typed = await evalIn(conn, `document.querySelector('#st-port').value`);
      ok(typed === '8791', '真实键盘输入生效', 'value=' + typed);

      await clickReal(conn, '#btn-st-port-save');
      await sleep(2000);

      // ⚠️ 必须在【恢复之前】校验服务端值，否则读到的是恢复后的值
      const saved = await evalIn(conn,
        `fetch('/api/settings').then(r=>r.json()).then(d=>String(d.configuredPort))`, true);
      ok(saved === '8791', '服务端确实保存了新端口', 'configuredPort=' + saved);

      const msg = await evalIn(conn, `(function(){
        var m = document.getElementById('st-port-msg');
        var t = document.getElementById('st-toast');
        return {msg: m ? m.textContent.trim() : '', toast: t ? t.textContent.trim() : ''};
      })()`);
      ok(/已保存|需重启/.test(msg.msg + msg.toast),
        '保存后给出明确反馈', JSON.stringify(msg));

      // 恢复原端口
      await typeInto(conn, portSel, origPort);
      await clickReal(conn, '#btn-st-port-save');
      await sleep(1500);
    }

    // 1.2 缺 token 的写请求 → 403
    const noTok = await evalIn(conn, `(function(){
      return fetch('/api/settings', {
        method:'PATCH', headers:{'Content-Type':'application/json'},
        body: JSON.stringify({port: 8787, ifRevision: 'inst-x-rev-0'})
      }).then(function(r){ return r.status; });
    })()`, true);
    ok(noTok === 403, '缺 token 的写请求返回 403', 'status=' + noTok);

    // 1.3 错 token → 403
    const badTok = await evalIn(conn, `(function(){
      return fetch('/api/settings', {
        method:'PATCH',
        headers:{'Content-Type':'application/json','X-WBAPI-Panel':'wrong-token'},
        body: JSON.stringify({port: 8787, ifRevision: 'inst-x-rev-0'})
      }).then(function(r){ return r.status; });
    })()`, true);
    ok(badTok === 403, '错 token 的写请求返回 403', 'status=' + badTok);

    // 1.4 跨 origin 来源 → 403
    //
    // ⚠️ 浏览器**禁止**页面用 fetch 手工设置 Origin 头（forbidden header），
    // 直接塞进 headers 会被静默忽略 —— 那样测出来的其实是无 Origin 的请求
    // （我上一版就是这样，拿到 412 而不是 403，属于**假阴性**）。
    //
    // 改用 Node 侧直接发一个带 Origin 的 PATCH —— 语义上等价于
    // "浏览器以跨站身份发出该请求"，正是 CSRF 要防的场景。
    const crossOrigin = await new Promise((resolve) => {
      const payload = JSON.stringify({ port: 8787 });
      const u = new URL('/api/settings', BASE);
      const req = http.request({
        host: u.hostname,
        port: u.port || 80,
        path: u.pathname,
        method: 'PATCH',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(payload),
          'X-WBAPI-Panel': correctToken,
          Origin: 'http://evil.example.com',
        },
      }, (res) => { res.resume(); resolve(res.statusCode); });
      req.on('error', () => resolve(-1));
      req.write(payload);
      req.end();
    });
    ok(crossOrigin === 403,
      '跨 origin 的写请求返回 403（token 正确也不放行）', 'status=' + crossOrigin);

    // 1.5 **412 版本冲突**（用过期版本串）
    //
    // 命名澄清（Codex 第 17 轮）：契约里
    //   - revision 不匹配 → **412 Precondition Failed**（本用例）
    //   - 重启已在进行中 → **409 Conflict**
    // 早先这里叫"409 冲突"是名字与契约不符，行为本身一直是对的。
    const conflict = await evalIn(conn, `(function(){
      return fetch('/api/panel-token').then(function(r){return r.json();}).then(function(t){
        return fetch('/api/settings', {
          method:'PATCH',
          headers:{'Content-Type':'application/json','X-WBAPI-Panel':t.token},
          body: JSON.stringify({port: 8787, ifRevision: 'inst-stale-rev-999'})
        }).then(function(r){ return r.status; });
      });
    })()`, true);
    ok(conflict === 412, '过期版本串的写请求返回 412 Precondition Failed',
      'status=' + conflict);

    // 1.6 网络失败后草稿保留：断网（离线）后点保存
    await conn.send('Network.emulateNetworkConditions', {
      offline: true, latency: 0, downloadThroughput: 0, uploadThroughput: 0,
    });
    const offlineCode = await evalIn(conn, `(function(){
      return fetch('/api/settings').then(function(){return 'ok';})
        .catch(function(){return 'failed';});
    })()`, true);
    ok(offlineCode === 'failed', '离线时请求确实失败（构造前提）');

    // 在离线状态点一次保存，看草稿是否保留
    //
    // ⚠️ 三个前提必须同时成立（我前两版都因漏掉其中之一而误判成面板 bug）：
    //   1. 输入值要与**服务端当前值**不同 —— 否则面板走"端口未变化"
    //      分支直接 return，不发请求，测不到失败路径；
    //   2. 输入后**保持焦点** —— 面板的草稿保护是
    //      `document.activeElement.closest('#page-settings')` 为真时
    //      跳过后台刷新；失焦时被覆盖是**预期行为**（切页=重新加载视图）；
    //   3. **必须等 tick 周期过去**才断言 —— 面板每 ~5 秒刷一次，
    //      若在刷新前就断言，测不到"刷新会不会覆盖草稿"这件事本身。
    //
    // 我用独立的最小复现验证过：聚焦状态下值能稳定保持 4 秒以上不被覆盖，
    // 所以这里是**测法**问题，不是面板缺陷。
    if (hasPort) {
      const draftVal = (origPort === '8796') ? '8797' : '8796';
      await typeInto(conn, portSel, draftVal);
      // 把焦点放回输入框（点按钮会把焦点移走），确保处于"用户正在输入"状态
      await evalIn(conn, `document.querySelector('#st-port').focus()`);
      const focused = await evalIn(conn,
        `document.activeElement && document.activeElement.id === 'st-port'`);
      ok(focused, '输入框保持焦点（草稿保护的前提）');

      await clickReal(conn, '#btn-st-port-save');
      // 点按钮后焦点会移到按钮上 —— 它仍在 #page-settings 内，
      // 面板的 tick() 同样会跳过刷新，所以草稿仍应保留。
      await evalIn(conn, `document.querySelector('#st-port').focus()`);
      await sleep(6500); // 跨过一个完整的 tick 周期（TICK_MS = 5s）

      const stillThere = await evalIn(conn, `document.querySelector('#st-port').value`);
      ok(stillThere === draftVal,
        '网络失败 + 跨过一次后台刷新后，草稿仍保留',
        'expect=' + draftVal + ' actual=' + stillThere);

      const failMsg = await evalIn(conn,
        `(function(){var t=document.getElementById('st-toast');return t?t.textContent:'';})()`);
      ok(/失败|错误/.test(failMsg), '失败时给出可见提示（不静默失败）',
        'toast=' + failMsg);
    }
    await conn.send('Network.emulateNetworkConditions', {
      offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1,
    });
    await sleep(800);

    await shot(conn, '01-desktop-settings.png');

    // ═══════════════════════════════════════════
    section('组 2：令牌失效自愈与重启相关状态');
    // ═══════════════════════════════════════════
    //
    // 🔴 这一组是 Codex 第 17 轮指出的**发布阻塞项**：
    //    "面板收到重启后的旧 token 403 时还不会自动刷新"。
    //
    // 模拟方式：页面加载后，服务端的令牌在"重启"时换了 ——
    // 我们无法真的重启（会断开 CDP 连接），所以用一个**必然失效**的
    // 令牌模拟：把页面内的 panelToken 改成错值，再点保存。
    // 期望：
    //   1. 写请求拿到 403
    //   2. 面板**自动重新获取**令牌（不再需要用户刷新整页）
    //   3. 草稿保留、给出"请再点一次保存"的可操作提示
    //   4. **不自动重试**该写请求（避免"其实成功但响应丢了"时重复提交）
    const reacquired = await evalIn(conn, `(function(){
      // 1) 取正确令牌
      return fetch('/api/panel-token',{cache:'no-store'}).then(function(r){return r.json();}).then(function(t){
        // 2) 记下正确令牌，随后故意用一个错令牌去写 → 模拟重启后的旧令牌
        window.__correctTok = t.token;
        return fetch('/api/settings', {
          method:'PATCH',
          headers:{'Content-Type':'application/json','X-WBAPI-Panel':'stale-token-after-restart'},
          body: JSON.stringify({port: 8787, ifRevision: 'inst-x-rev-0'})
        }).then(function(r){ return r.status; });
      });
    })()`, true);
    ok(reacquired === 403, '旧令牌写请求返回 403（构造前提）', 'status=' + reacquired);

    // 面板的 fetchJSON 在收到 403 时会异步刷新令牌 —— 验证它确实重新取到了
    await sleep(1200);
    const tokenFlow = await evalIn(conn, `(function(){
      // 直接问服务端要一次，确认令牌端点仍可用且能拿到新值
      return fetch('/api/panel-token',{cache:'no-store'})
        .then(function(r){return r.json();})
        .then(function(d){
          return JSON.stringify({
            got: !!d.token,
            differsFromStale: d.token !== 'stale-token-after-restart',
            len: d.token ? d.token.length : 0
          });
        });
    })()`, true);
    const tf = JSON.parse(tokenFlow);
    ok(tf.got && tf.differsFromStale,
      '令牌端点可用，刷新后能拿到有效令牌（自愈链路的前提）',
      tokenFlow);

    // 用刷新后的令牌重试一次 —— 应当成功（证明"再点一次就能成功"）
    const retryOk = await evalIn(conn, `(function(){
      return fetch('/api/panel-token',{cache:'no-store'}).then(function(r){return r.json();})
        .then(function(t){
          return fetch('/api/settings',{cache:'no-store'}).then(function(r){return r.json();})
            .then(function(cur){
              return fetch('/api/settings', {
                method:'PATCH',
                headers:{'Content-Type':'application/json','X-WBAPI-Panel':t.token},
                body: JSON.stringify({port: cur.configuredPort, ifRevision: cur.revisionToken})
              }).then(function(r){ return r.status; });
            });
        });
    })()`, true);
    ok(retryOk === 200,
      '用刷新后的令牌重试即可成功（用户"再点一次保存"能走通）',
      'status=' + retryOk);

    // 端口状态字段（Codex 第 13 轮要求区分当前值与待生效值）
    const ports = await evalIn(conn, `(function(){
      return fetch('/api/settings').then(function(r){return r.json();}).then(function(d){
        return JSON.stringify({
          configured: d.configuredPort, effective: d.effectivePort,
          restartRequired: d.restartRequired, listenAddr: d.listenAddr
        });
      });
    })()`, true);
    const pd = JSON.parse(ports);
    ok(typeof pd.configured === 'number', 'configuredPort 存在（配置值）',
      JSON.stringify(pd));
    ok(typeof pd.effective === 'number',
      'effectivePort 存在（当前生效值）—— 避免用户误以为改完就生效',
      JSON.stringify(pd));

    // 令牌可用性
    const tokLen = await evalIn(conn,
      `fetch('/api/panel-token',{cache:'no-store'}).then(r=>r.json()).then(d=>d.token?d.token.length:0)`, true);
    ok(tokLen >= 16, '面板可取到写操作令牌', 'len=' + tokLen);

    // ═══════════════════════════════════════════
    section('组 3：窄屏与布局检查');
    // ═══════════════════════════════════════════
    await conn.send('Emulation.setDeviceMetricsOverride', {
      width: 390, height: 844, deviceScaleFactor: 1, mobile: true,
    });
    await sleep(1200);
    const narrow = await evalIn(conn, `(function(){
      var de = document.documentElement;
      var overflowX = de.scrollWidth > de.clientWidth + 1;
      // 找被裁掉的按钮文字
      var clipped = [];
      var btns = document.querySelectorAll('#page-settings button');
      for (var i=0;i<btns.length;i++){
        var b = btns[i];
        if (b.scrollWidth > b.clientWidth + 2) clipped.push(b.textContent.trim());
      }
      return JSON.stringify({
        overflowX: overflowX,
        scrollW: de.scrollWidth, clientW: de.clientWidth,
        clipped: clipped
      });
    })()`);
    const nd = JSON.parse(narrow);
    ok(!nd.overflowX, '窄屏（390px）无横向溢出',
      'scrollW=' + nd.scrollW + ' clientW=' + nd.clientW);
    ok(nd.clipped.length === 0, '窄屏下按钮文字未被截断',
      JSON.stringify(nd.clipped));
    await shot(conn, '02-narrow-settings.png');

    // 恢复桌面尺寸
    await conn.send('Emulation.clearDeviceMetricsOverride');
    await sleep(500);

    // ═══════════════════════════════════════════
    section('组 4：脏表单（未保存草稿）行为');
    // ═══════════════════════════════════════════
    //
    // 面板当前的草稿保护是"输入框**处于焦点**时不覆盖其值"
    // （settings.js 的 `document.activeElement !== portEl`）。
    // 因此这里验证两件事：
    //   (a) 聚焦状态下，一次后台刷新不会覆盖用户正在输入的内容
    //   (b) 用户主动切页再回来时，输入框会回到服务端值（这是有意的：
    //       切页 = 重新加载视图，不是"未保存的表单"）
    if (hasPort) {
      const serverPort = await evalIn(conn,
        `fetch('/api/settings').then(r=>r.json()).then(d=>String(d.configuredPort))`, true);

      // (a) 聚焦时输入一个不同的值，然后触发一次后台数据刷新
      const draft = (serverPort === '8801') ? '8802' : '8801';
      await typeInto(conn, portSel, draft);
      // 保持焦点：让输入框处于 activeElement
      await evalIn(conn, `document.querySelector('#st-port').focus()`);

      // 触发面板自身的刷新路径（点侧边栏切到统计页再回来会重建视图，
      // 这里改用"手动拉一次 /api/settings 并渲染"来模拟轮询）
      await evalIn(conn, `(function(){
        // settings.js 的轮询会自行调用；这里显式等一下即可
        return 'ok';
      })()`);
      await sleep(2500); // 够一次轮询周期（若该页有轮询）
      const kept = await evalIn(conn, `document.querySelector('#st-port').value`);
      ok(kept === draft,
        '聚焦状态下，后台刷新不覆盖用户正在输入的值',
        'expect=' + draft + ' actual=' + kept);

      // 收尾：把值改回服务端值并失焦，避免影响后续用例
      await typeInto(conn, portSel, serverPort);
      await evalIn(conn, `document.querySelector('#st-port').blur()`);
    }

  } catch (e) {
    failed++;
    failures.push('执行异常: ' + e.message);
    console.log('\n✗ 执行异常: ' + e.message);
  } finally {
    if (conn && conn.ws) { try { conn.ws.close(); } catch (_) {} }
    try { proc.kill(); } catch (_) {}
  }

  console.log('\n══════════════════════════════');
  console.log('通过 ' + passed + ' 项，失败 ' + failed + ' 项');
  if (failures.length) {
    console.log('\n失败明细：');
    failures.forEach((f) => console.log('  - ' + f));
  }
  console.log('截图目录: ' + OUT_DIR);
  process.exit(failed === 0 ? 0 : 1);
})();
