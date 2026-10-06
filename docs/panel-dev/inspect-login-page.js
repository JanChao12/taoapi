// 通过 CDP 读取登录页的真实状态：可见文本、localStorage、以及页面对 state 的处理。
//
// 为什么需要它：委托方两次登录都被页面拒绝（"请返回客户端点击「重新发起登录」重试"），
// 而该文案**不在主 bundle 里** —— 说明是运行期由接口返回的错误。
// 必须看到页面实际拿到了什么响应，才能判断是 state 无效、还是缺某个必需参数。
//
// 用法：node inspect-login-page.js <cdp端口>
const port = process.argv[2];
if (!port) { console.error("用法: node inspect-login-page.js <cdp端口>"); process.exit(2); }

const base = `http://127.0.0.1:${port}`;

// 最小 CDP over WebSocket（Node 18+ 自带 WebSocket 客户端）
async function main() {
  const targets = await (await fetch(`${base}/json/list`)).json();
  const page = targets.find(t => t.type === "page" && t.url.includes("codebuddy.cn"));
  if (!page) { console.error("未找到 codebuddy 页面"); process.exit(1); }

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();

  ws.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
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

  const evalJS = async (expr) => {
    const r = await call("Runtime.evaluate", {
      expression: expr,
      returnByValue: true,
      awaitPromise: true,
    });
    if (r.result?.exceptionDetails) return `[异常] ${r.result.exceptionDetails.text}`;
    return r.result?.result?.value;
  };

  console.log("=== 页面基本信息 ===");
  console.log("URL     :", page.url);
  console.log("Title   :", await evalJS("document.title"));
  console.log("ReadyState:", await evalJS("document.readyState"));

  console.log("\n=== 页面可见文本（前 1200 字符）===");
  const text = await evalJS("document.body ? document.body.innerText : '(no body)'");
  console.log(typeof text === "string" ? text.slice(0, 1200) : text);

  console.log("\n=== localStorage（只列键名与值长度，不打印值）===");
  console.log(await evalJS(`
    (() => {
      try {
        const out = [];
        for (let i = 0; i < localStorage.length; i++) {
          const k = localStorage.key(i);
          const v = localStorage.getItem(k) || "";
          out.push(k + " (len=" + v.length + ")");
        }
        return out.join("\\n") || "(空)";
      } catch (e) { return "读取失败: " + e.message; }
    })()
  `));

  console.log("\n=== 页面上的错误提示元素 ===");
  console.log(await evalJS(`
    (() => {
      const sel = ['.ant-message','[class*=error]','[class*=Error]','[class*=fail]','[class*=Fail]'];
      const found = [];
      for (const s of sel) {
        document.querySelectorAll(s).forEach(el => {
          const t = (el.innerText || "").trim();
          if (t && t.length < 400) found.push(s + " => " + t);
        });
      }
      return [...new Set(found)].join("\\n") || "(未找到错误元素)";
    })()
  `));

  console.log("\n=== 页面 URL 的 query 参数 ===");
  console.log(await evalJS("location.search"));

  ws.close();
}

main().catch(e => { console.error("失败:", e.message); process.exit(1); });
