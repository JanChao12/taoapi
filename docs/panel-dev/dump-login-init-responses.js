// 读取登录页初始化阶段几个关键接口的**响应内容**，定位"登录失败"的真实原因。
//
// 背景：页面加载后立刻调
//   POST /console/auth/risk-context
//   GET  /console/accounts
//   GET  /console/validate/refresh-token
// 然后渲染「登录失败 / 请返回客户端点击「重新发起登录」重试」。
//
// 🔴 脱敏规则：响应体里的可疑凭据（JWT / 长不透明串 / *token* 字段的值）
//    一律替换成长度占位，**绝不打印原文**。本脚本只用于定位失败原因。
//
// 用法：node dump-login-init-responses.js <cdp端口>
const port = process.argv[2];
if (!port) { console.error("用法: node dump-login-init-responses.js <cdp端口>"); process.exit(2); }

const base = `http://127.0.0.1:${port}`;

const TARGETS = [
  "/console/auth/risk-context",
  "/console/accounts",
  "/console/validate/refresh-token",
  "/v2/plugin/login/gray-decision",
];

// 脱敏：把"像凭据"的值换掉，只留长度。
function redact(text) {
  let s = text;
  // JWT（三段）
  s = s.replace(/eyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}/g,
    (m) => `«JWT:${m.length}»`);
  // 形如 "xxxToken":"...." 的值
  s = s.replace(/("(?:[A-Za-z_]*[Tt]oken[A-Za-z_]*)"\s*:\s*")([^"]{16,})(")/g,
    (m, a, v, c) => `${a}«len=${v.length}»${c}`);
  // 超长不透明串
  s = s.replace(/[A-Za-z0-9_\-]{60,}/g, (m) => `«opaque:${m.length}»`);
  return s;
}

async function main() {
  const targets = await (await fetch(`${base}/json/list`)).json();
  const page = targets.find(t => t.type === "page" && t.url.includes("codebuddy.cn"));
  if (!page) { console.error("未找到 codebuddy 页面"); process.exit(1); }

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();
  const want = new Map();   // requestId -> url
  const bodies = [];        // {url, status, body}

  ws.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); return; }

    if (msg.method === "Network.responseReceived") {
      const { requestId, response } = msg.params;
      if (!TARGETS.some(t => response.url.includes(t))) return;
      want.set(requestId, { url: response.url, status: response.status });
    }

    if (msg.method === "Network.loadingFinished") {
      const { requestId } = msg.params;
      const info = want.get(requestId);
      if (!info) return;
      want.delete(requestId);
      // 取 body
      call("Network.getResponseBody", { requestId }).then(r => {
        const body = r?.result?.body ?? "";
        bodies.push({ ...info, body });
      });
    }
  });

  await new Promise((res, rej) => {
    ws.addEventListener("open", res);
    ws.addEventListener("error", rej);
  });

  const call = (method, params = {}) => new Promise((resolve) => {
    const myId = ++id;
    pending.set(myId, resolve);
    ws.send(JSON.stringify({ id: myId, method, params }));
  });

  await call("Network.enable", {});
  await call("Page.enable", {});

  console.log("=== 重新加载页面，抓初始化接口响应 ===\n");
  await call("Page.reload", { ignoreCache: true });
  await new Promise(r => setTimeout(r, 15000));

  console.log("=== 响应内容（已脱敏）===\n");
  for (const b of bodies) {
    console.log(`--- ${b.status} ${b.url}`);
    console.log(redact(b.body).slice(0, 1500));
    console.log();
  }
  if (!bodies.length) console.log("(没抓到目标接口的响应)");

  ws.close();
}

main().catch(e => { console.error("失败:", e.message); process.exit(1); });
