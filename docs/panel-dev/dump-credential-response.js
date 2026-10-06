// 专门抓取 `/console/login/enterprise` 的响应体，确认凭据是否真的在里面。
//
// 背景（2026-10-05 实测）：点选账号后该端点被 POST 调用并返回 200，
// 但页面随后调 /console/auth/login 得到 400，于是 UI 显示"登录失败"。
// 需要确认：200 的响应体里到底有没有 accessToken/refreshToken。
//
// 🔴 脱敏：只打印**字段名与长度**，绝不打印 token 值。
//
// 用法：node dump-credential-response.js <cdp端口> <state>
const port = process.argv[2];
const wantState = process.argv[3] || "";
if (!port) { console.error("用法: node dump-credential-response.js <cdp端口> [state]"); process.exit(2); }

const base = `http://127.0.0.1:${port}`;
const TARGET = "/console/login/enterprise";

// 只描述形态，不输出值。
function describe(obj, prefix = "") {
  const lines = [];
  if (obj === null || typeof obj !== "object") {
    return [`${prefix} = ${typeof obj}`];
  }
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (typeof v === "string") {
      const isTokenish = /token/i.test(k);
      const isJWT = /^eyJ[\w-]+\.[\w-]+\.[\w-]+$/.test(v);
      lines.push(`${path}: string len=${v.length}${isJWT ? " «JWT»" : ""}${isTokenish ? " «token-ish»" : ""}`);
    } else if (v === null) {
      lines.push(`${path}: null`);
    } else if (Array.isArray(v)) {
      lines.push(`${path}: array[${v.length}]`);
      if (v.length && typeof v[0] === "object") lines.push(...describe(v[0], `${path}[0]`));
    } else if (typeof v === "object") {
      lines.push(...describe(v, path));
    } else {
      lines.push(`${path}: ${typeof v} = ${v}`);
    }
  }
  return lines;
}

async function main() {
  const targets = await (await fetch(`${base}/json/list`)).json();
  const page = targets.find(t => t.type === "page" && t.url.includes("codebuddy.cn"));
  if (!page) { console.error("未找到 codebuddy 页面"); process.exit(1); }

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();
  const want = new Map();

  ws.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); return; }

    if (msg.method === "Network.responseReceived") {
      const { requestId, response } = msg.params;
      if (!response.url.includes(TARGET)) return;
      want.set(requestId, { url: response.url, status: response.status, method: response.requestHeadersText ? "?" : "?" });
      console.log(`\n[捕获] ${response.status} ${response.url}`);
      if (wantState && !response.url.includes(wantState)) {
        console.log(`  ⚠️ state 与期望不符（期望含 ${wantState}）`);
      } else if (wantState) {
        console.log(`  ✅ state 匹配期望值`);
      }
    }

    if (msg.method === "Network.loadingFinished") {
      const { requestId } = msg.params;
      const info = want.get(requestId);
      if (!info) return;
      want.delete(requestId);
      call("Network.getResponseBody", { requestId }).then(r => {
        const body = r?.result?.body ?? "";
        let parsed = null;
        try { parsed = JSON.parse(body); } catch {}
        console.log(`  响应体 ${body.length} 字节`);
        if (parsed) {
          console.log("  字段形态（只有名字与长度，无值）:");
          for (const l of describe(parsed, "  ")) console.log("  " + l);
        } else {
          console.log("  (非 JSON)");
        }
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

  // 重新加载并重放交互：加载后点"选择账号"里的第一个条目。
  console.log(`=== 监听 ${TARGET}（重新加载页面）===`);
  await call("Page.reload", { ignoreCache: true });
  await new Promise(r => setTimeout(r, 9000));

  // 尝试自动点击"选择账号"里的账号条目（如果能找到）
  const clicked = await call("Runtime.evaluate", {
    expression: `
      (() => {
        const nodes = [...document.querySelectorAll('div,li,button,a')];
        const target = nodes.find(n => {
          const t = (n.innerText || '').trim();
          return /^\\d{11}/.test(t) && t.length < 120;
        });
        if (target) { target.click(); return 'clicked: ' + target.innerText.trim().slice(0,40); }
        return 'not-found';
      })()
    `,
    returnByValue: true,
  });
  console.log("自动点击结果:", clicked?.result?.result?.value);

  await new Promise(r => setTimeout(r, 15000));
  console.log("\n=== 完成 ===");
  ws.close();
}

main().catch(e => { console.error("失败:", e.message); process.exit(1); });
