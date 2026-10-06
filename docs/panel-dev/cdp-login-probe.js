// 登录凭据来源探针：定位 accessToken/refreshToken 的真实来源
//
// 原则（Codex 第 28 轮要求）：
//   - 全新独立 profile，导航前连接 CDP
//   - 不观察密码/验证码提交，不保存 HAR/Cookie/Storage/响应体原始值
//   - 只输出字段名、类型、长度、存在性、步骤顺序
//   - 凭据原值仅留在内存，绝不落盘、绝不打印
//
// 用法：
//   node cdp-login-probe.js <cdp-port> <浏览器exe路径> [profile目录]
// 例：
//   node cdp-login-probe.js 19300 "C:\Program Files\Google\Chrome\Application\chrome.exe"
//
// 产出：控制台诊断 + probe-report.json（仅字段名/长度）

const http = require('http');
const net = require('net');
const crypto = require('crypto');
const fs = require('fs');
const { spawn } = require('child_process');
const os = require('os');
const path = require('path');

const PORT = parseInt(process.argv[2] || '19300', 10);
const BROWSER = process.argv[3] || 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const PROFILE = process.argv[4] || path.join(os.tmpdir(), 'wbapi-login-probe-' + Date.now());
const LOGIN_URL = 'https://www.codebuddy.cn/login/?platform=CLI';

// ---------- 极简 WebSocket 客户端 ----------
// 产品实现要用 Go stdlib 手写；此处用 Node 只为快速拿到结论
function wsConnect(wsUrl, timeoutMs = 15000) {
  return new Promise((resolve, reject) => {
    const u = new URL(wsUrl);
    const key = crypto.randomBytes(16).toString('base64');
    const sock = net.connect(parseInt(u.port, 10), u.hostname);
    let buf = Buffer.alloc(0);
    let handshakeDone = false;
    let onEvent = () => {};
    const pending = new Map();
    let nextId = 1;
    const timer = setTimeout(() => reject(new Error('ws connect timeout')), timeoutMs);

    sock.on('connect', () => {
      sock.write(
        `GET ${u.pathname} HTTP/1.1\r\nHost: ${u.host}\r\nUpgrade: websocket\r\n` +
        `Connection: Upgrade\r\nSec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\n\r\n`
      );
    });

    sock.on('error', (e) => { clearTimeout(timer); reject(e); });

    sock.on('data', (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      if (!handshakeDone) {
        const idx = buf.indexOf('\r\n\r\n');
        if (idx === -1) return;
        const head = buf.slice(0, idx).toString();
        if (!/ 101 /.test(head.split('\r\n')[0])) {
          clearTimeout(timer);
          return reject(new Error('handshake: ' + head.split('\r\n')[0]));
        }
        handshakeDone = true;
        clearTimeout(timer);
        buf = buf.slice(idx + 4);
        resolve({ call, close, on: (fn) => { onEvent = fn; } });
      }
      while (buf.length >= 2) {
        const opcode = buf[0] & 0x0f;
        let len = buf[1] & 0x7f, off = 2;
        if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4; }
        else if (len === 127) { if (buf.length < 10) return; len = Number(buf.readBigUInt64BE(2)); off = 10; }
        if (buf.length < off + len) return;
        const payload = buf.slice(off, off + len);
        buf = buf.slice(off + len);
        if (opcode === 0x1) {
          let obj = null;
          try { obj = JSON.parse(payload.toString('utf8')); } catch {}
          if (!obj) continue;
          if (obj.id && pending.has(obj.id)) {
            const { res, rej, t } = pending.get(obj.id);
            pending.delete(obj.id); clearTimeout(t); res(obj);
          } else {
            onEvent(obj);
          }
        } else if (opcode === 0x9) {
          const pong = Buffer.alloc(2 + payload.length);
          pong[0] = 0x8a; pong[1] = payload.length;
          payload.copy(pong, 2);
          sock.write(pong);
        }
      }
    });

    function raw(obj) {
      const data = Buffer.from(JSON.stringify(obj), 'utf8');
      const mask = crypto.randomBytes(4);
      let header;
      if (data.length < 126) { header = Buffer.alloc(2); header[1] = 0x80 | data.length; }
      else if (data.length < 65536) { header = Buffer.alloc(4); header[1] = 0x80 | 126; header.writeUInt16BE(data.length, 2); }
      else { header = Buffer.alloc(10); header[1] = 0x80 | 127; header.writeBigUInt64BE(BigInt(data.length), 2); }
      header[0] = 0x81;
      const masked = Buffer.alloc(data.length);
      for (let i = 0; i < data.length; i++) masked[i] = data[i] ^ mask[i % 4];
      sock.write(Buffer.concat([header, mask, masked]));
    }

    function call(method, params = {}, tMs = 20000) {
      const id = nextId++;
      return new Promise((res, rej) => {
        const t = setTimeout(() => {
          if (pending.has(id)) { pending.delete(id); rej(new Error('timeout ' + method)); }
        }, tMs);
        pending.set(id, { res, rej, t });
        raw({ id, method, params });
      });
    }
    function close() { try { sock.destroy(); } catch {} }
  });
}

// ---------- 工具 ----------
function httpGet(p) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: PORT, path: p, timeout: 5000 }, (r) => {
      let d = ''; r.on('data', (c) => d += c); r.on('end', () => res(d));
    }).on('error', rej).on('timeout', function () { this.destroy(new Error('http timeout')); });
  });
}

// 只判断"像不像 token"，绝不返回原值
//
// ⚠️ 关键教训（第 28 轮实测）：登录页本身就会在 localStorage 写一个 238 字符的
// 访客标识（如 `e418bef1uidsk`），它**不是凭据**。若把"出现长字符串"当成登录完成，
// 探针会在用户还没登录时就误报退出 —— 第一次跑就踩了这个坑。
// 因此：只有 **JWT 形态**（三段 base64url、以 ey 开头）才算命中。
function tokenKind(v) {
  if (typeof v !== 'string' || v.length < 40) return null;
  // 只认 JWT（Keycloak 签发的一定是 JWT；登录页的访客标识不是）
  if (v.split('.').length === 3 && v.startsWith('ey')) {
    try {
      const h = JSON.parse(Buffer.from(v.split('.')[0], 'base64').toString('utf8'));
      return 'JWT(alg=' + (h.alg || '?') + ')';
    } catch { return 'JWT'; }
  }
  return null;
}

// 站点自身的访客标识等"像 token 但不是凭据"的东西，单独归类，不触发完成
function noiseKind(v) {
  if (typeof v !== 'string' || v.length < 40) return null;
  if (/^[0-9a-f]{32,}$/i.test(v)) return 'hex' + v.length;
  if (v.length > 200) return 'opaque' + v.length;
  return null;
}

const R = {
  startedAt: new Date().toISOString(),
  browser: null,
  cookiesBefore: [], cookiesAfter: [],
  localStorageBefore: [], localStorageAfter: [],
  tokenHits: [],
  interestingResponses: [],
  responseShapes: [],     // 网络响应体里的字段名+长度（不含值）
  bodyTokenFields: [],    // 响应体里出现的凭据字段
  steps: [],
};

function log(s) { console.log(s); R.steps.push(s); }

async function snapshot(p, tag) {
  const out = { cookies: [], ls: [] };
  try {
    const ck = await p.call('Network.getCookies', {});
    for (const c of (ck.result?.cookies || [])) {
      out.cookies.push({ name: c.name, domain: c.domain, httpOnly: c.httpOnly, len: c.value.length, kind: tokenKind(c.value) });
      const k = tokenKind(c.value);
      if (k) R.tokenHits.push({ where: tag + '/cookie', name: c.name, domain: c.domain, len: c.value.length, kind: k });
    }
  } catch {}
  for (const isLocal of [true, false]) {
    try {
      const d = await p.call('DOMStorage.getDOMStorageItems', {
        storageId: { securityOrigin: 'https://www.codebuddy.cn', isLocalStorage: isLocal },
      });
      for (const [k, v] of (d.result?.entries || [])) {
        out.ls.push({ area: isLocal ? 'local' : 'session', key: k, len: v.length, kind: tokenKind(v) });
        const kk = tokenKind(v);
        if (kk) R.tokenHits.push({ where: tag + '/' + (isLocal ? 'localStorage' : 'sessionStorage'), name: k, len: v.length, kind: kk });
      }
    } catch {}
  }
  return out;
}

(async () => {
  // 1. 启动独立 profile 浏览器
  log(`启动浏览器: ${BROWSER}`);
  log(`独立 profile: ${PROFILE}`);
  const child = spawn(BROWSER, [
    `--user-data-dir=${PROFILE}`,
    `--remote-debugging-port=${PORT}`,
    '--no-first-run',
    '--no-default-browser-check',
    'about:blank',
  ], { detached: false, stdio: 'ignore' });

  let ver = null;
  for (let i = 0; i < 40; i++) {
    await new Promise((r) => setTimeout(r, 500));
    try { ver = JSON.parse(await httpGet('/json/version')); break; } catch {}
  }
  if (!ver) throw new Error('浏览器 CDP 端口未就绪');
  R.browser = ver.Browser;
  log(`✅ CDP 就绪: ${ver.Browser}`);

  // 2. 找页面 target 并连接（导航前！）
  const list = JSON.parse(await httpGet('/json/list'));
  const page = list.find((t) => t.type === 'page');
  if (!page) throw new Error('没有 page target');
  const p = await wsConnect(page.webSocketDebuggerUrl);
  log('✅ 已连接页面级 CDP');

  const events = [];
  p.on((m) => events.push(m));

  // 3. 【关键】导航前启用各域
  await p.call('Network.enable', {});
  await p.call('Runtime.enable', {});
  await p.call('DOMStorage.enable', {});
  await p.call('Page.enable', {});
  log('✅ 导航前已启用 Network/Runtime/DOMStorage');

  // 4. 登录前基线
  const before = await snapshot(p, 'before');
  R.cookiesBefore = before.cookies; R.localStorageBefore = before.ls;
  log(`登录前基线: cookie=${before.cookies.length}, storage=${before.ls.length}, token命中=${R.tokenHits.length}`);

  // 5. 导航
  const state = crypto.randomUUID();
  await p.call('Page.navigate', { url: `${LOGIN_URL}&state=${state}` });
  log(`✅ 已导航到官方登录页 state=${state.slice(0, 8)}…`);

  console.log('\n' + '='.repeat(72));
  console.log('  👉 请在弹出的浏览器窗口中完成登录（扫码/手机号均可）');
  console.log('     探针不读取密码与验证码，不保存任何凭据原始值');
  console.log('     登录成功后本探针会自动检测到并输出报告');
  console.log('='.repeat(72) + '\n');

  // 6. 采样循环
  //    完成条件：出现 JWT 形态（localStorage / sessionStorage / Cookie），
  //    或某个网络响应体里出现 refresh_token / access_token 字段。
  //    ⚠️ 不能拿"出现长字符串"当完成信号（登录页访客标识会误报）。
  const deadline = Date.now() + 15 * 60 * 1000;
  let done = false;
  let lastLsSig = '';
  const bodyChecked = new Set();

  while (Date.now() < deadline && !done) {
    await new Promise((r) => setTimeout(r, 2500));

    // 6a. 网络响应线索
    for (const e of events) {
      if (e.method === 'Network.responseReceived') {
        const u = e.params.response.url;
        const t = e.params.type;
        if (t === 'XHR' || t === 'Fetch') {
          const short = u.slice(0, 140);
          if (!R.interestingResponses.some((x) => x.url === short)) {
            R.interestingResponses.push({ url: short, status: e.params.response.status, mime: e.params.response.mimeType, reqId: e.params.requestId });
          }
          // 6b. 抓响应体，只看"字段名"，绝不保存值
          if (!bodyChecked.has(e.params.requestId)) {
            bodyChecked.add(e.params.requestId);
            try {
              const bd = await p.call('Network.getResponseBody', { requestId: e.params.requestId }, 8000);
              const body = bd.result?.body || '';
              if (body && body.length < 200000) {
                const fieldNames = [];
                // 只提取可能的凭据字段名 + 值的类型/长度，不存值
                const re = /"(access_token|refresh_token|accessToken|refreshToken|id_token|token|expires_in|expiresAt|uid|enterpriseId|domain|nickname)"\s*:\s*(?:"([^"]{0,4000})"|([\d.]+|true|false|null))/g;
                let m;
                while ((m = re.exec(body))) {
                  const name = m[1];
                  const val = m[2] !== undefined ? m[2] : m[3];
                  const kind = tokenKind(val);
                  fieldNames.push({ name, len: (val || '').length, kind: kind || typeof val });
                  if (kind) {
                    R.bodyTokenFields.push({ url: short, field: name, len: val.length, kind });
                  }
                }
                if (fieldNames.length) {
                  R.responseShapes.push({ url: short, fields: fieldNames.slice(0, 25) });
                  log(`响应体字段: ${short} → ${fieldNames.map((f) => f.name + '(' + f.len + (f.kind ? ',' + f.kind : '') + ')').join(', ').slice(0, 300)}`);
                }
              }
            } catch { /* 响应体可能已释放 */ }
          }
        }
      }
    }

    // 6c. 存储快照
    const snap = await snapshot(p, 'after');
    R.cookiesAfter = snap.cookies; R.localStorageAfter = snap.ls;

    const sig = snap.ls.map((x) => x.key + ':' + x.len).join('|');
    if (sig !== lastLsSig) {
      lastLsSig = sig;
      const tok = snap.ls.filter((x) => x.kind);
      log(`存储变化: ${snap.ls.length} 项，其中 JWT 形态 ${tok.length} 个`);
      for (const n of tok) console.log(`    ⭐ ${n.area} ${n.key} len=${n.len} ${n.kind}`);
    }

    const jwtInStorage = snap.ls.some((x) => x.kind && String(x.kind).startsWith('JWT'));
    const jwtInCookie = snap.cookies.some((c) => c.kind && String(c.kind).startsWith('JWT'));
    const jwtInBody = R.bodyTokenFields.some((f) => f.field === 'refresh_token' || f.field === 'refreshToken');

    if (jwtInStorage || jwtInCookie || jwtInBody) {
      log(`❗检测到凭据（storage=${jwtInStorage} cookie=${jwtInCookie} body=${jwtInBody}）`);
      done = true;
    } else {
      // 心跳，让委托方知道探针还活着
      process.stdout.write('.');
    }
  }

  // 7. 报告
  const uniq = [];
  for (const h of R.tokenHits) if (!uniq.some((x) => x.where === h.where && x.name === h.name)) uniq.push(h);
  R.tokenHits = uniq;

  console.log('\n' + '='.repeat(72));
  console.log('诊断报告（只有字段名/长度，无凭据值）');
  console.log('='.repeat(72));

  console.log('\n【Cookie · 登录后】');
  for (const c of R.cookiesAfter) console.log(`  ${c.name}  dom=${c.domain}  httpOnly=${c.httpOnly}  len=${c.len}  ${c.kind || ''}`);

  console.log('\n【Storage · 登录后】');
  for (const s of R.localStorageAfter) console.log(`  [${s.area}] ${s.key}  len=${s.len}  ${s.kind || ''}`);

  console.log('\n【相关网络响应（XHR/Fetch）】');
  for (const n of R.interestingResponses.slice(0, 40)) console.log(`  ${n.status}  ${n.url}`);

  console.log('\n【响应体字段形态（只有字段名与长度）】');
  if (!R.responseShapes.length) console.log('  （未捕获到含凭据字段的响应体）');
  for (const s of R.responseShapes.slice(0, 20)) {
    console.log(`  ${s.url}`);
    console.log(`     ${s.fields.map((f) => `${f.name}(${f.len}${f.kind ? '|' + f.kind : ''})`).join('  ')}`);
  }

  console.log('\n【⭐⭐ 凭据来源结论】');
  if (R.bodyTokenFields.length) {
    console.log('  👉 凭据出现在【网络响应体】里：');
    for (const f of R.bodyTokenFields) console.log(`     ${f.field}  len=${f.len}  ${f.kind}   来自 ${f.url}`);
  }
  const jwtHits = R.tokenHits.filter((h) => String(h.kind).startsWith('JWT'));
  if (jwtHits.length) {
    console.log('  👉 凭据出现在【本地存储/Cookie】里：');
    for (const h of jwtHits) console.log(`     ${h.where}  ${h.name}  len=${h.len}  ${h.kind}`);
  }
  if (!R.bodyTokenFields.length && !jwtHits.length) {
    console.log('  ❌ 未定位到凭据来源 —— 需要扩展探针（可能走 WebSocket 或另有端点）');
  }

  fs.writeFileSync('probe-report.json', JSON.stringify(R, null, 2));
  console.log('\n✅ probe-report.json 已写入（不含凭据原始值）');
  console.log(`\n浏览器仍在运行（profile: ${PROFILE}）`);
  console.log('如需关闭：关闭那个浏览器窗口即可');

  p.close();
  process.exit(0);
})().catch((e) => {
  console.error('探针异常:', e.message);
  process.exit(1);
});
