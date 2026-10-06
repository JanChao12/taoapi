// integration_test.go：端到端验证 CDP 事件路由与响应体提取。
//
// ⚠️ 这个测试会**真的启动一个浏览器进程**，因此默认跳过。
//
//	设置 WBAPI_RUN_BROWSER_TESTS=1 才运行。
//
// 为什么需要它：单元测试能验证 URL 匹配逻辑，但**验证不了**
// "CDP 事件真的能收到、loadingFinished 之后真的能取到 body"——
// 而那正是凭据提取赖以工作的前提。Codex 第 28 轮也提醒过
// 「CDP 连接成功不等于完整凭据提取成功」。
//
// 测试设计：不依赖真实登录（那需要人工），而是让受控浏览器去访问一个
// **我们自己的本地 HTTP 服务**，它返回一个与凭据端点同形态的 JSON。
// 这样验证的是**同一条代码路径**（登记 requestId → 等 finished → 取 body
// → 解析字段），只是响应来源可控，因而可自动化。
package login

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

const fakeAccess = "eyJhbGciOiJSUzI1NiJ9.FAKEACCESS.FAKESIG"
const fakeRefresh = "eyJhbGciOiJIUzUxMiJ9.FAKEREFRESH.FAKESIG"

// requireBrowserEnv 没有环境变量时跳过需要浏览器的测试。
func requireBrowserEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("WBAPI_RUN_BROWSER_TESTS") != "1" {
		t.Skip("需要真实浏览器；设置 WBAPI_RUN_BROWSER_TESTS=1 才运行")
	}
	if _, _, err := FindBrowser(); err != nil {
		t.Skipf("本机无受支持浏览器: %v", err)
	}
}

// startFakeCredServer 起一个返回"凭据形态"JSON 的本地服务。
// 返回基础 URL 与关闭函数。
func startFakeCredServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起本地服务失败: %v", err)
	}
	// ⚠️ 用**实测的真实结构**（嵌套在 data 里 + 业务信封），
	// 不用"顶层结构"那个曾经让真实登录失败的错假设。
	body := fmt.Sprintf(
		`{"code":0,"msg":"OK","data":{"accessToken":%q,"refreshToken":%q,`+
			`"expiresIn":3283200,"refreshExpiresIn":3456000,"tokenType":"Bearer"}}`,
		fakeAccess, fakeRefresh)
	mux := http.NewServeMux()
	mux.HandleFunc("/cred", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 加一点延迟，模拟真实网络，确保 loadingFinished 在 responseReceived 之后。
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

// pageTargetWS 找到一个页面级 target 的 WebSocket URL。
func pageTargetWS(t *testing.T, sess *BrowserSession, timeout time.Duration) string {
	t.Helper()
	httpc := newCDPHTTPClient(sess.CDPBaseURL(), 5*time.Second)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if targets, err := httpc.listTargets(); err == nil {
			for _, tg := range targets {
				if tg.Type == "page" && tg.WebSocketDebuggerURL != "" {
					return tg.WebSocketDebuggerURL
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("未找到页面 target")
	return ""
}

// TestCDPEventRoutingAndBodyFetch 验证核心链路：
// 启用 Network → 导航 → 收到 responseReceived → 等 loadingFinished
// → getResponseBody 拿到正确内容。
func TestCDPEventRoutingAndBodyFetch(t *testing.T) {
	requireBrowserEnv(t)

	base, closeSrv := startFakeCredServer(t)
	defer closeSrv()
	credURL := base + "/cred?state=" + "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	// 先把浏览器起在 about:blank（这样随后的导航必然产生新请求）。
	sess, err := StartBrowser(LoginOptions{StartURL: "about:blank", StartupTimeout: 40 * time.Second})
	if err != nil {
		t.Fatalf("启动浏览器失败: %v", err)
	}
	defer sess.Close()

	wsURL := pageTargetWS(t, sess, 20*time.Second)
	cdp, err := newCDPSession(wsURL, 10*time.Second)
	if err != nil {
		t.Fatalf("建立 CDP 会话失败: %v", err)
	}
	defer cdp.Close()

	// ── 事件登记（与生产代码同构）──
	var (
		mu         sync.Mutex
		candidates = map[string]bool{}
		finished   = make(chan string, 4)
		sawResp    = make(chan string, 4)
	)

	cdp.SetEventHandler(func(ev cdpEvent) {
		switch ev.Method {
		case "Network.responseReceived":
			var p struct {
				RequestID string `json:"requestId"`
				Response  struct {
					URL string `json:"url"`
				} `json:"response"`
			}
			if json.Unmarshal(ev.Params, &p) != nil {
				return
			}
			if p.Response.URL == credURL {
				mu.Lock()
				candidates[p.RequestID] = true
				mu.Unlock()
				select {
				case sawResp <- p.RequestID:
				default:
				}
			}
		case "Network.loadingFinished":
			var p struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(ev.Params, &p) != nil {
				return
			}
			mu.Lock()
			isCand := candidates[p.RequestID]
			mu.Unlock()
			if isCand {
				select {
				case finished <- p.RequestID:
				default:
				}
			}
		}
	})

	// 启用 Network（必须在导航之前）。
	if _, err := cdp.Call("Network.enable", map[string]any{}, 15*time.Second); err != nil {
		t.Fatalf("启用 Network 失败: %v", err)
	}

	// 导航到我们的假凭据端点。
	if _, err := cdp.Call("Page.navigate", map[string]any{"url": credURL}, 15*time.Second); err != nil {
		t.Fatalf("导航失败: %v", err)
	}

	// 应收到 responseReceived。
	var reqID string
	select {
	case reqID = <-sawResp:
		t.Logf("✅ 收到 responseReceived: %s", reqID)
	case <-time.After(20 * time.Second):
		t.Fatal("❌ 未收到 responseReceived —— CDP 事件路由不通")
	}

	// 应收到 loadingFinished。
	select {
	case finID := <-finished:
		if finID != reqID {
			t.Fatalf("loadingFinished 的 requestId 与 responseReceived 不一致")
		}
		t.Logf("✅ 收到 loadingFinished")
	case <-time.After(20 * time.Second):
		t.Fatal("❌ 未收到 loadingFinished")
	}

	// 取响应体。
	body, err := fetchResponseBody(cdp, reqID)
	if err != nil {
		t.Fatalf("❌ getResponseBody 失败: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("❌ 响应体为空")
	}

	// 解析并核对。
	creds, err := extractCredential(body)
	if err != nil {
		t.Fatalf("❌ 解析凭据失败: %v", err)
	}
	if creds.accessToken() != fakeAccess {
		t.Fatalf("accessToken 不符")
	}
	if creds.refreshToken() != fakeRefresh {
		t.Fatalf("refreshToken 不符")
	}
	t.Log("✅ 完整链路通过：事件登记 → 等 finished → 取 body → 解析凭据")
}

// TestCDPEventHandlerNotBuffered 守住"事件不缓冲"（防内存堆积回归）。
//
// 做法：产生大量事件后，确认会话结构里没有累积容器。
// 由于该字段已被移除，这里用"能持续处理事件且不 OOM"间接验证：
// 连续导航多次，每次都能正常收到事件。
func TestCDPEventHandlerNotBuffered(t *testing.T) {
	requireBrowserEnv(t)

	base, closeSrv := startFakeCredServer(t)
	defer closeSrv()

	sess, err := StartBrowser(LoginOptions{StartURL: "about:blank", StartupTimeout: 40 * time.Second})
	if err != nil {
		t.Fatalf("启动浏览器失败: %v", err)
	}
	defer sess.Close()

	wsURL := pageTargetWS(t, sess, 20*time.Second)
	cdp, err := newCDPSession(wsURL, 10*time.Second)
	if err != nil {
		t.Fatalf("建立 CDP 会话失败: %v", err)
	}
	defer cdp.Close()

	var count int
	var mu sync.Mutex
	cdp.SetEventHandler(func(ev cdpEvent) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	if _, err := cdp.Call("Network.enable", map[string]any{}, 15*time.Second); err != nil {
		t.Fatalf("启用 Network 失败: %v", err)
	}

	// 连续导航 10 次，每次都应产生事件。
	for i := 0; i < 10; i++ {
		url := fmt.Sprintf("%s/cred?i=%d", base, i)
		if _, err := cdp.Call("Page.navigate", map[string]any{"url": url}, 15*time.Second); err != nil {
			t.Fatalf("第 %d 次导航失败: %v", i, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	mu.Lock()
	n := count
	mu.Unlock()
	if n < 10 {
		t.Fatalf("事件数偏少（%d），事件路由可能有问题", n)
	}
	t.Logf("✅ 处理了 %d 个事件，无堆积", n)
}

// TestBrowserLifecycleCleansUp 验证真实浏览器会话的收尾：
// 进程退出、端口释放、profile 删除、互斥位释放。
func TestBrowserLifecycleCleansUp(t *testing.T) {
	requireBrowserEnv(t)

	sess, err := StartBrowser(LoginOptions{StartURL: "about:blank", StartupTimeout: 40 * time.Second})
	if err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	port := sess.Port()
	profile := sess.ProfileDir()

	// 启动后应占着互斥位。
	if CurrentSession() == nil {
		t.Error("启动后应占用互斥位")
	}
	// profile 应存在。
	if _, err := os.Stat(profile); err != nil {
		t.Errorf("profile 应存在: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Errorf("关闭返回错误: %v", err)
	}

	// 端口应释放。
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		c.Close()
		t.Error("关闭后端口仍可连接")
	}
	// profile 应删除。
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Errorf("关闭后 profile 应被删除: %s", profile)
	}
	// 互斥位应释放。
	if CurrentSession() != nil {
		t.Error("关闭后互斥位应释放")
	}
}

// TestStartBrowserIsMutuallyExclusive 验证登录互斥。
func TestStartBrowserIsMutuallyExclusive(t *testing.T) {
	requireBrowserEnv(t)

	s1, err := StartBrowser(LoginOptions{StartURL: "about:blank", StartupTimeout: 40 * time.Second})
	if err != nil {
		t.Fatalf("第一次启动失败: %v", err)
	}
	defer s1.Close()

	s2, err := StartBrowser(LoginOptions{StartURL: "about:blank", StartupTimeout: 10 * time.Second})
	if err == nil {
		s2.Close()
		t.Fatal("第二次启动应被拒绝（登录互斥）")
	}
	if err != ErrLoginBusy {
		t.Fatalf("应返回 ErrLoginBusy，得到: %v", err)
	}
}
