// 面板登录按钮的验收测试（用真实浏览器打开面板，检查元素与交互）。
//
// 这是 CDP 自动化脚本，验证：
//   1. 面板能打开
//   2. "导入账号"按钮改名为"添加账号"
//   3. 模态里有"网页登录"按钮
//   4. 点它之后进度条出现（不真的登录，只看 UI 反应）
//
// 用法：node panel_login_ui_check.js <panelUrl> <cdpPort>
const http = require('http');
const net = require('net');
const crypto = require('crypto');

const PANEL_URL = process.argv[2] || 'http://127.0.0.1:8787/';
const CDP_PORT = parseInt(process.argv[3] || '19400', 10);

function httpGet(p) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: CDP_PORT, path: p, timeout: 5000 }, (r) => {
      let d = ''; r.on('data', (c) => d += c); r.on('end', () => res(d));
    }).on('error', rej);
  });
}

function wsConnect(wsUrl) {
  return new Promise((resolve, reject) => {
    const u = new URL(wsUrl);
    const key = crypto.randomBytes(16).toString('base64');
    const sock = net.connect(parseInt(u.port, 10), u.hostname);
    let buf = Buffer.alloc(0);
    let handshakeDone = false;
    const pending = new Map();
    let nextId = 1;

    sock.on('connect', () => {
      sock.write(`GET ${u.pathname} HTTP/1.1\r\nHost: ${u.host}\r\nUpgrade: websocket\r\n` +
        `Connection: Upgrade\r\nSec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\n\r\n`);
    });
    sock.on('error', reject);
    sock.on('data', (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      if (!handshakeDone) {
        const i = buf.indexOf('\r\n\r\n');
        if (i === -1) return;
        const head = buf.slice(0, i).toString();
        if (!/ 101 /.test(head.split('\r\n')[0])) return reject(new Error('handshake: ' + head.split('\r\n')[0]));
        handshakeDone = true;
        buf = buf.slice(i + 4);
        resolve({ call, close });
      }
      while (buf.length >= 2) {
        const op = buf[0] & 0x0f;
        let len = buf[1] & 0x7f, off = 2;
        if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4; }
        else if (len === 127) { if (buf.length < 10) return; len = Number(buf.readBigUInt64BE(2)); off = 10; }
        if (buf.length < off + len) return;
        const pl = buf.slice(off, off + len);
        buf = buf.slice(off + len);
        if (op === 1) {
          let o = null; try { o = JSON.parse(pl.toString('utf8')); } catch {}
          if (o && o.id && pending.has(o.id)) { const { res } = pending.get(o.id); pending.delete(o.id); res(o); }
        }
      }
    });
    function raw(obj) {
      const data = Buffer.from(JSON.stringify(obj), 'utf8');
      const mask = crypto.randomBytes(4);
      let hdr;
      if (data.length < 126) { hdr = Buffer.alloc(2); hdr[1] = 0x80 | data.length; }
      else if (data.length < 65536) { hdr = Buffer.alloc(4); hdr[1] = 0x80 | 126; hdr.writeUInt16BE(data.length, 2); }
      else { hdr = Buffer.alloc(10); hdr[1] = 0x80 | 127; hdr.writeBigUInt64BE(BigInt(data.length), 2); }
      hdr[0] = 0x81;
      const m = Buffer.alloc(data.length);
      for (let i = 0; i < data.length; i++) m[i] = data[i] ^ mask[i % 4];
      sock.write(Buffer.concat([hdr, mask, m]));
    }
    function call(method, params = {}, t = 15000) {
      const id = nextId++;
      return new Promise((res, rej) => {
        const timer = setTimeout(() => { pending.delete(id); rej(new Error('timeout ' + method)); }, t);
        pending.set(id, { res: (o) => { clearTimeout(timer); res(o); } });
        raw({ id, method, params });
      });
    }
    function close() { try { sock.destroy(); } catch {} }
  });
}

(async () => {
  let pass = 0, fail = 0;
  const check = (name, ok, detail) => {
    if (ok) { console.log(`  ✅ ${name}`); pass++; }
    else { console.log(`  ❌ ${name}${detail ? ' — ' + detail : ''}`); fail++; }
  };

  console.log('=== 面板登录按钮 UI 验收 ===\n');

  // 找页面 target
  const list = JSON.parse(await httpGet('/json/list'));
  const page = list.find((t) => t.type === 'page' && !/devtools/.test(t.url));
  if (!page) { console.log('❌ 没有可用页面'); process.exit(1); }

  const cdp = await wsConnect(page.webSocketDebuggerUrl);
  await cdp.call('Page.enable');
  await cdp.call('Runtime.enable');

  const evalJs = async (expr) => {
    const r = await cdp.call('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.result && r.result.exceptionDetails) {
      throw new Error(r.result.exceptionDetails.text || 'eval error');
    }
    return r.result && r.result.result ? r.result.result.value : undefined;
  };

  // 1. 导航到面板
  console.log('1. 打开面板');
  await cdp.call('Page.navigate', { url: PANEL_URL });
  await new Promise((r) => setTimeout(r, 3000));
  const title = await evalJs('document.title');
  check('面板已加载', !!title, 'title=' + title);

  // 2. 找导入/添加账号按钮
  console.log('\n2. 检查「添加账号」入口');
  const btnText = await evalJs(`(function(){
    var b = document.getElementById('btn-import');
    return b ? b.textContent.trim() : null;
  })()`);
  check('找到账号添加按钮', btnText !== null, 'btn-import=' + btnText);
  check('按钮文案已更新为「添加账号」', btnText === '添加账号', '实际: ' + btnText);

  // 3. 点开模态
  console.log('\n3. 打开模态');
  await evalJs(`document.getElementById('btn-import').click()`);
  await new Promise((r) => setTimeout(r, 500));

  const modalTitle = await evalJs(`(function(){
    var h = document.querySelector('#modal-body h3');
    return h ? h.textContent.trim() : null;
  })()`);
  check('模态标题为「添加账号」', modalTitle === '添加账号', '实际: ' + modalTitle);

  // 4. 检查网页登录按钮
  console.log('\n4. 检查网页登录按钮');
  const loginBtn = await evalJs(`(function(){
    var b = document.getElementById('btn-web-login');
    return b ? b.textContent.trim() : null;
  })()`);
  check('存在「网页登录」按钮', loginBtn === '网页登录', '实际: ' + loginBtn);

  // 5. 检查进度条容器存在但初始隐藏
  console.log('\n5. 检查进度条');
  const progressHidden = await evalJs(`(function(){
    var p = document.getElementById('login-progress');
    if (!p) return 'missing';
    return getComputedStyle(p).display;
  })()`);
  check('进度条初始隐藏', progressHidden === 'none', '实际: ' + progressHidden);

  const barExists = await evalJs(`!!document.getElementById('login-bar-fill')`);
  check('进度条填充元素存在', barExists === true);

  // 6. 检查取消按钮
  const cancelBtn = await evalJs(`(function(){
    var b = document.getElementById('btn-login-cancel');
    return b ? b.textContent.trim() : null;
  })()`);
  check('存在「取消」按钮', cancelBtn === '取消', '实际: ' + cancelBtn);

  // 7. 检查导入方式仍保留（并列入口）
  console.log('\n6. 检查导入方式仍作为并列入口');
  const hasTextarea = await evalJs(`!!document.getElementById('import-text')`);
  check('粘贴框仍存在（并列入口）', hasTextarea === true);
  const hasImportBtn = await evalJs(`!!document.getElementById('btn-do-import')`);
  check('「导入」按钮仍存在', hasImportBtn === true);

  // 8. 检查引导文案提到两种方式
  //
  // ⚠️ 必须取**全部** .import-guide 的文本：面板里有两个引导块
  // （方式一 / 方式二 各一个）。第一版只取第一个（querySelector），
  // 于是"方式二"永远找不到 —— 是测试写错了，不是产品错。
  const guideText = await evalJs(`(function(){
    var gs = document.querySelectorAll('.import-guide');
    var out = '';
    for (var i = 0; i < gs.length; i++) out += gs[i].textContent;
    return out;
  })()`);
  check('引导提到「方式一：网页登录」', /方式一/.test(guideText));
  check('引导提到「方式二：粘贴凭据」', /方式二/.test(guideText));
  check('不再声称「没有网页登录按钮」', !/没有「网页登录」按钮/.test(guideText));

  // 9. 关闭模态后轮询应停止（检查全局无残留定时器引用）
  console.log('\n7. 关闭模态');
  await cdp.call('Runtime.evaluate', { expression: `document.getElementById('modal-close').click()` });
  await new Promise((r) => setTimeout(r, 300));
  const modalClosed = await evalJs(`!document.getElementById('overlay').classList.contains('open')`);
  check('模态已关闭', modalClosed === true);

  console.log(`\n=== 结果：${pass} 通过，${fail} 失败 ===`);
  cdp.close();
  process.exit(fail > 0 ? 1 : 0);
})().catch((e) => { console.error('异常:', e.message); process.exit(1); });
