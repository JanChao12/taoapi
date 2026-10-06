// 在登录页上抓取网络请求：看页面对 state 到底调了什么接口、拿到什么状态码。
//
// 背景：页面一加载就显示"登录失败/请返回客户端点击「重新发起登录」重试"，
// 说明它在**初始化阶段**就判定失败。要定位原因必须看到它发了什么、收到什么。
//
// 🔴 只记录 URL、方法、状态码、以及**安全的**响应字段形态；
//    响应体一律不打印（可能含凭据）。
//
// 用法：node trace-login-network.js <cdp端口> [监听秒数]
const port = process.argv[2];
const seconds = Number(process.argv[3] || 25);
if (!port) { console.error("用法: node trace-login-network.js <cdp端口> [秒]"); process.exit(2); }

const base = `http://127.0.0.1:${port}`;

// 只关心这些端点的形态（其余噪音不打印）
const INTERESTING = [
  "codebuddy.cn/api", "codebuddy.cn/console", "codebuddy.cn/v2",
  "codebuddy.cn/auth", "plugin", "login", "state",
];

async function main() {
  const targets = await (await fetch(`${base}/json/list`)).json();
  const page = targets.find(t => t.type === "page" && t.url.includes("codebuddy.cn"));
  if (!page) { console.error("未找到 codebuddy 页面"); process.exit(1); }

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();
  const reqs = new Map(); // requestId -> {url, method}

  ws.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); return; }

    if (msg.method === "Network.requestWillBeSent") {
      const { requestId, request } = msg.params;
      const url = request.url;
      if (!INTERESTING.some(k => url.includes(k))) return;
      reqs.set(requestId, { url, method: request.method });
      console.log(`→ ${request.method} ${url}`);
    }

    if (msg.method === "Network.responseReceived") {
      const { requestId, response } = msg.params;
      const r = reqs.get(requestId);
      if (!r) return;
      console.log(`← ${response.status} ${response.mimeType || ""} ${r.url}`);
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

  // 必须在导航前启用；这里页面已加载，所以我们**重新加载**一次来完整观察。
  await call("Network.enable", {});
  await call("Page.enable", {});

  console.log(`=== 重新加载登录页，监听 ${seconds} 秒 ===\n`);
  await call("Page.reload", { ignoreCache: true });

  await new Promise(r => setTimeout(r, seconds * 1000));

  console.log("\n=== 完成 ===");
  ws.close();
}

main().catch(e => { console.error("失败:", e.message); process.exit(1); });
