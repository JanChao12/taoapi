// capture_e2e_test.go：用**生产函数本体**端到端验证凭据捕获。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么必须走生产函数（Codex 第 29 轮要求）
// ═══════════════════════════════════════════════════════════════════
//
// 2026-10-05 的真实事故：`captureCredentials` 在 CDP 事件回调里
// **同步**调 `fetchResponseBody` ⇒ 与 `readLoop` 自死锁，
// 真实登录**永远抓不到凭据**（连续 4 次 10 分钟超时）。
//
// 而当时测试**全绿** —— 因为 `integration_test.go` 把捕获逻辑
// **抄了一遍**放进测试里，没走生产函数，天然不含那个死锁。
//
// **教训：测试抄生产逻辑 = 测了个假的。**
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 构造方式（Codex 第 30 轮纠正，我原先搞错了）
// ═══════════════════════════════════════════════════════════════════
//
// 我最初的写法：另开一个 CDP 会话 B 去 `Runtime.evaluate` 发请求，
// 生产会话 A 只监听。**结果 A 什么都收不到**（sighting 摘要为空）。
//
// Codex 第 30 轮裁定原因：
//
//	「`Network.enable` 的状态至少是 session 级的；**不能假设 A 启用后，
//	   B 通过 `Runtime.evaluate` 发出的请求一定会送到 A。**
//	   **生产代码不应要求另一个 CDP session 替它发请求。**」
//
// ⇒ 正确构造：**用生产会话自己**在页面里触发请求。
//
//	这仍是"页面发出的 fetch"（不是直接调 Go 解析函数），
//	足以验证事件关联、取 body、解析、以及死锁修复。
//
// 跨会话是否可见属于**独立诊断**，不作为生产契约
// （见 session_isolation_test.go）。
//
// 默认跳过（需要真浏览器）：
//
//	$env:WBAPI_RUN_BROWSER_TESTS="1"; go test ./internal/login/ -run TestCaptureE2E -v
package login

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// credBody 构造与上游**真实形态**一致的凭据响应体。
//
// 🔴 2026-10-05 由真实登录更正：凭据**嵌套在 `data` 里**，
// 且带业务信封（code/msg/requestId）—— 早期实现按顶层结构写，
// 导致真实登录时"缺少 accessToken"而失败。
//
// ⚠️ 测试数据必须来自**实测样本**，不能来自实现者的假设 ——
// 否则测试只是在验证自己的想象（这个教训代价很大）。
func credBody(access, refresh string) string {
	return fmt.Sprintf(
		`{"code":0,"msg":"OK","requestId":"00000000-0000-4000-8000-000000000000",`+
			`"data":{"accessToken":%q,"refreshToken":%q,`+
			`"expiresIn":3283200,"refreshExpiresIn":3456000,"tokenType":"Bearer"}}`,
		access, refresh)
}

// describeRaw 描述一个 CDP 返回值的**形态**（不打印内容）。
func describeRaw(raw []byte) string {
	s := string(raw)
	switch {
	case s == "":
		return "(空)"
	case s[0] == '{':
		return fmt.Sprintf("JSON 对象，%d 字节", len(s))
	default:
		return fmt.Sprintf("%d 字节，首字符 %q", len(s), s[0])
	}
}

// startFakeCredServerAt 在指定路径上返回"凭据形态"的 JSON。
//
// 只接受 POST —— 与实测的上游方法一致。
func startFakeCredServerAt(t *testing.T, path, body string) (base string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起本地服务失败: %v", err)
	}
	mux := http.NewServeMux()
	// /blank：一个**同源**落地页，用于消除 CORS 预检。
	//
	// 为什么需要：页面在 about:blank（origin=null）时，fetch 到本服务端
	// 是跨 origin ⇒ 浏览器先发 OPTIONS 预检；而生产校验只接受 POST。
	// 先导航到这里，后续 fetch 就是同源的（与真实场景一致：
	// 登录页与凭据端点都在 www.codebuddy.cn）。
	mux.HandleFunc("/blank", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><title>blank</title>"))
	})
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 加延迟，确保 loadingFinished 落在 responseReceived 之后
		// （与真实网络同构；过早取 body 会拿到空内容）。
		time.Sleep(120 * time.Millisecond)
		w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	return fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port), func() {
		srv.Close()
		ln.Close()
	}
}

// TestCaptureE2ESameSessionFetch 是 Codex 第 30 轮要求的**硬测试**：
// 在**生产会话自己**的页面上触发 fetch，生产函数必须捕获到凭据。
//
// 若将来有人把取 body 改回回调内同步执行（自死锁），或把
// SetEventHandler 挪到 Network.enable 之后，本测试会**超时失败**。
func TestCaptureE2ESameSessionFetch(t *testing.T) {
	requireBrowserEnv(t)

	const wantState = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	base, closeSrv := startFakeCredServerAt(t, "/console/login/enterprise",
		credBody(fakeAccess, fakeRefresh))
	defer closeSrv()

	// 仅测试：把捕获端点指向本地假服务端（实例级接缝）。
	restore := setCredEndpointForTest(base + "/console/login/enterprise")
	defer restore()

	sess, err := StartBrowser(LoginOptions{
		StartURL:       "about:blank",
		StartupTimeout: 40 * time.Second,
	})
	if err != nil {
		t.Fatalf("启动浏览器失败: %v", err)
	}
	defer sess.Close()

	// ── 关键：由**生产会话自己**触发请求 ──
	//
	// 做法：给 captureCredentials 传一个"onReady"回调，
	// 它拿到**生产会话自己的** cdpSession，在同一个会话里 evaluate。
	// 这样请求与监听在同一个 Network 域上（Codex 第 30 轮要求）。
	//
	// 🔴🔴 但还有一个更隐蔽的坑（2026-10-05 实测，Codex 第 31 轮预言）：
	//
	//	页面在 about:blank（origin=null），fetch 到 http://127.0.0.1:PORT
	//	属于**跨 origin** 请求 ⇒ 浏览器先发 **OPTIONS 预检**，
	//	而生产校验正确地拒绝 OPTIONS（它不是 POST）。
	//
	//	诊断原文：「方法校验未通过，实际方法: OPTIONS×1」
	//
	// ⇒ **这是我测试构造的问题，不是生产缺陷**：
	//	真实场景里登录页与端点**同源**（都在 www.codebuddy.cn），
	//	不会产生 CORS 预检。
	//
	// 解决：先把页面导航到**假服务端自己的源**上，让后续 fetch 同源。
	//	这样就没有预检，只有真正的 POST。
	triggerURL := base + "/console/login/enterprise?state=" + wantState
	triggerDone := make(chan string, 1)

	start := time.Now()
	creds, err := captureCredentialsWithTrigger(
		sess, wantState, 30*time.Second,
		func(p Phase) {},
		nil,
		func(s *cdpSession) {
			// ① 先同源化：导航到假服务端自己的页面（消除 CORS 预检）。
			if _, err := s.Call("Page.navigate", map[string]any{
				"url": base + "/blank",
			}, 15*time.Second); err != nil {
				triggerDone <- "导航失败: " + err.Error()
				return
			}
			// 给导航一点时间落地（Network 事件会继续流向同一个会话）。
			time.Sleep(1200 * time.Millisecond)

			// ② 同源 fetch —— 此时不会再发 OPTIONS 预检。
			expr := fmt.Sprintf(
				`fetch(%q, {method:"POST", headers:{"Content-Type":"application/json"}, body:"{}"})`+
					`.then(r=>r.text()).catch(e=>"ERR:"+e)`,
				triggerURL)
			raw, err := s.Call("Runtime.evaluate", map[string]any{
				"expression":    expr,
				"awaitPromise":  true,
				"returnByValue": true,
			}, 20*time.Second)
			if err != nil {
				triggerDone <- "调用失败: " + err.Error()
				return
			}
			triggerDone <- describeRaw(raw)
		})
	took := time.Since(start)

	select {
	case d := <-triggerDone:
		t.Logf("页面内 fetch（生产会话自己发起）结果形态: %s", d)
	default:
		t.Log("（trigger 回调未产生结果 —— 可能未执行）")
	}

	if err != nil {
		t.Fatalf("❌ captureCredentials 失败（耗时 %s）: %v\n"+
			"诊断摘要: %q\n"+
			"若为「等待登录超时」，说明事件没被接住。",
			took.Round(time.Millisecond), err, lastCaptureSighting())
	}

	if creds.AccessToken != fakeAccess {
		t.Errorf("accessToken 不符：期望 %q，得到 %q", fakeAccess, creds.AccessToken)
	}
	if creds.RefreshToken != fakeRefresh {
		t.Errorf("refreshToken 不符：期望 %q，得到 %q", fakeRefresh, creds.RefreshToken)
	}
	t.Logf("✅ 生产函数端到端成功（耗时 %s）：同会话触发 → 事件接收 → 请求关联 → 取 body → 解析凭据",
		took.Round(time.Millisecond))
}
