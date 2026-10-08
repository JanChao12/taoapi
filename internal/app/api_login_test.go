// api_login_test.go：网页登录端点的护栏测试。
//
// 钉住的契约（每条对应 Codex 第 28 轮或项目红线的要求）：
//
//   - 响应**绝不**含 token（项目红线：token 不进任何 API 响应）
//   - 错误信息**绝不**含 token（脱敏兜底要真的有效）
//   - 无浏览器时返回明确错误并引导导入（Codex Q4：不静默下载浏览器）
//   - 未启动流程时 status 返回 idle（不残留上次结果）
//   - 没有进行中流程时 cancel 是安全的空操作
//   - 方法不符返回 405
package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/login"
)

// newRecorder 返回一个标准响应记录器。
func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// mustRequest 构造一个测试请求（body 为空串表示无请求体）。
func mustRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, target, nil)
	} else {
		r, err = http.NewRequest(method, target, strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return r
}

// ─────────────────────────────────────────────────────────────
// 脱敏：这是本文件最重要的部分
// ─────────────────────────────────────────────────────────────

// TestSanitizeErrorMessageStripsJWT 守住错误信息里的 JWT 被抹掉。
//
// 这是安全红线：错误信息是 token 泄露最常见的通道。
func TestSanitizeErrorMessageStripsJWT(t *testing.T) {
	secret := "eyJhbGciOiJSUzI1NiJ9.SUPERSECRETPAYLOAD.SUPERSECRETSIG"
	msg := "请求失败: token=" + secret + " 被拒绝"
	got := sanitizeErrorMessage(msg)

	if strings.Contains(got, secret) {
		t.Fatalf("错误信息泄露了完整 JWT: %s", got)
	}
	if strings.Contains(got, "SUPERSECRETPAYLOAD") {
		t.Fatalf("错误信息泄露了 JWT 片段: %s", got)
	}
}

// TestSanitizeErrorMessageStripsLongStrings 守住超长串被抹掉。
//
// 不透明的 refresh token 不是 JWT 形态也可能很长。
func TestSanitizeErrorMessageStripsLongStrings(t *testing.T) {
	secret := strings.Repeat("a", 300)
	got := sanitizeErrorMessage("failed with " + secret)
	if strings.Contains(got, secret) {
		t.Fatalf("错误信息泄露了超长串: %s", got)
	}
}

// TestSanitizeErrorMessageTruncates 守住超长错误被截断。
func TestSanitizeErrorMessageTruncates(t *testing.T) {
	msg := strings.Repeat("错误 ", 500)
	got := sanitizeErrorMessage(msg)
	if len(got) > 400 {
		t.Fatalf("错误信息未截断，长度 %d", len(got))
	}
}

// TestSanitizeErrorMessageStripsControlChars 守住控制字符被去掉（防日志注入）。
func TestSanitizeErrorMessageStripsControlChars(t *testing.T) {
	got := sanitizeErrorMessage("a\nb\rc\x00d\x1be")
	for _, r := range got {
		if r < 0x20 && r != ' ' || r == 0x7f {
			t.Fatalf("仍含控制字符: %q", got)
		}
	}
}

// TestLoginErrorMessageFriendlyMapping 守住已知错误有友好中文提示。
func TestLoginErrorMessageFriendlyMapping(t *testing.T) {
	cases := []struct {
		in   string
		want string // 期望包含的关键词
	}{
		{"login: 未找到受支持的浏览器（需要 Chrome 或 Edge）", "导入凭据"},
		{"login: 已有登录流程在进行中", "进行中"},
		{"login: 等待登录超时（10m0s）", "超时"},
		{"login: 已取消", "取消"},
		{"login: 浏览器连接已断开（窗口是否被关闭？）", "浏览器"},
	}
	for _, c := range cases {
		got := loginErrorMessage(errString(c.in))
		if !strings.Contains(got, c.want) {
			t.Errorf("输入 %q 的提示应含 %q，实际 %q", c.in, c.want, got)
		}
	}
}

// errString 是一个简单的 error 实现（避免引 errors 包只为造一个 error）。
type errString string

func (e errString) Error() string { return string(e) }

// ─────────────────────────────────────────────────────────────
// 端点行为
// ─────────────────────────────────────────────────────────────

// TestLoginStatusIdleWhenNoRun 守住"没流程时返回 idle"。
func TestLoginStatusIdleWhenNoRun(t *testing.T) {
	// 确保没有进行中的流程。
	activeLogin.mu.Lock()
	prev := activeLogin.current
	activeLogin.current = nil
	activeLogin.mu.Unlock()
	t.Cleanup(func() {
		activeLogin.mu.Lock()
		activeLogin.current = prev
		activeLogin.mu.Unlock()
	})

	rec := newRecorder()
	handleLoginStatus(Deps{}, rec, mustRequest(t, http.MethodGet, "/api/accounts/login/status", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["busy"] != false {
		t.Errorf("busy 应为 false，得到 %v", out["busy"])
	}
	if out["phase"] != "idle" {
		t.Errorf("phase 应为 idle，得到 %v", out["phase"])
	}
	// 🔴 不得含任何凭据字段。
	assertNoTokenFields(t, rec.Body.String())
}

// TestLoginStatusRejectsNonGET 守住方法校验。
func TestLoginStatusRejectsNonGET(t *testing.T) {
	rec := newRecorder()
	handleLoginStatus(Deps{}, rec, mustRequest(t, http.MethodPost, "/api/accounts/login/status", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
}

// TestLoginStartRejectsNonPOST 守住方法校验。
func TestLoginStartRejectsNonPOST(t *testing.T) {
	rec := newRecorder()
	handleLoginStart(Deps{}, rec, mustRequest(t, http.MethodGet, "/api/accounts/login/start", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
}

// TestLoginStartRejectsUnknownPlatform 守：非法平台必须**明确报错**。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么不能静默回退（2026-10-08 国际化改造）
// ═══════════════════════════════════════════════════════════════════
//
//	国内版与国际版是**两套账号体系**。若用户传了个拼错的平台名而程序
//	悄悄按国内版处理，他会打开国内版登录页、绑上一个国内账号 ——
//	而他本意是绑国际版。**静默回退把"明确的失败"变成了"错误的成功"**。
//
//	代价对比：一次 400 用户立刻知道改；一次静默绑错，用户要等到
//	发现"账号怎么查不到额度"才会回头（甚至永远不回头）。
func TestLoginStartRejectsUnknownPlatform(t *testing.T) {
	deps, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
	defer srv.Close()

	for _, bad := range []string{"us", "cn2", "workbuddy", "国际版"} {
		rec := newRecorder()
		body := `{"platform":"` + bad + `"}`
		handleLoginStart(deps, rec, mustRequest(t, http.MethodPost,
			"/api/accounts/login/start", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("platform=%q 状态码 = %d，期望 400（非法平台必须明确拒绝，"+
				"静默回退会绑错平台）", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "bad_platform") {
			t.Errorf("platform=%q 响应应含 bad_platform 错误码，实际: %s",
				bad, rec.Body.String())
		}
	}
}

// TestLoginStartAcceptsKnownPlatforms 守：合法平台被接受（并把平台回报给前端）。
func TestLoginStartAcceptsKnownPlatforms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"cn", "cn"},
		{"intl", "intl"},
		{"", "cn"}, // 缺省 = 国内版（兼容旧前端）
	} {
		// 每个用例都要独立的 env：登录会占互斥位。
		func() {
			deps, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
			defer srv.Close()

			// 清掉可能残留的登录状态，避免 409 干扰本测试。
			activeLogin.mu.Lock()
			prev := activeLogin.current
			activeLogin.current = nil
			activeLogin.mu.Unlock()
			t.Cleanup(func() {
				activeLogin.mu.Lock()
				activeLogin.current = prev
				activeLogin.mu.Unlock()
			})

			rec := newRecorder()
			body := `{}`
			if tc.in != "" {
				body = `{"platform":"` + tc.in + `"}`
			}
			handleLoginStart(deps, rec, mustRequest(t, http.MethodPost,
				"/api/accounts/login/start", body))

			// 要么 202（已受理），要么 503（本机无浏览器）——
			// 两者都说明平台校验**通过了**（没走到 400 分支）。
			if rec.Code == http.StatusBadRequest {
				t.Errorf("platform=%q 被误拒为 400: %s", tc.in, rec.Body.String())
			}
			if rec.Code == http.StatusAccepted &&
				!strings.Contains(rec.Body.String(), `"platform":"`+tc.want+`"`) {
				t.Errorf("platform=%q 的响应应回报 platform=%q，实际: %s",
					tc.in, tc.want, rec.Body.String())
			}
			// 清理：本测试不该真的留下登录流程。
			activeLogin.mu.Lock()
			if activeLogin.current != nil {
				activeLogin.current.canceled = true
				close(activeLogin.current.cancelCh)
				activeLogin.current = nil
			}
			activeLogin.mu.Unlock()
		}()
	}
}

// TestLoginCancelIsSafeWhenIdle 守住"没有流程时取消是空操作"。
func TestLoginCancelIsSafeWhenIdle(t *testing.T) {
	activeLogin.mu.Lock()
	prev := activeLogin.current
	activeLogin.current = nil
	activeLogin.mu.Unlock()
	t.Cleanup(func() {
		activeLogin.mu.Lock()
		activeLogin.current = prev
		activeLogin.mu.Unlock()
	})

	rec := newRecorder()
	handleLoginCancel(Deps{}, rec, mustRequest(t, http.MethodPost, "/api/accounts/login/cancel", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	assertNoTokenFields(t, rec.Body.String())
}

// TestLoginStartReportsBusyWhenAlreadyRunning 守住登录互斥在 API 层的表现。
//
// ⚠️ 这里必须传**真实的依赖**（非空 Accounts/Persister），否则会先被
// "账号功能未启用"的 503 拦下 —— 那样测试等于没测到互斥逻辑。
// 第一版就是因为传了空 Deps 而得到 503，暴露了测试前提写错（不是产品错）。
func TestLoginStartReportsBusyWhenAlreadyRunning(t *testing.T) {
	deps, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
	defer srv.Close()

	activeLogin.mu.Lock()
	prev := activeLogin.current
	activeLogin.current = &loginRun{phase: login.PhaseWaiting, cancelCh: make(chan struct{})}
	activeLogin.mu.Unlock()
	t.Cleanup(func() {
		activeLogin.mu.Lock()
		activeLogin.current = prev
		activeLogin.mu.Unlock()
	})

	rec := newRecorder()
	handleLoginStart(deps, rec, mustRequest(t, http.MethodPost, "/api/accounts/login/start", ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（已有流程）；响应: %s", rec.Code, rec.Body.String())
	}
}

// TestLoginStatusNeverLeaksTokenFields 守住进行中/完成状态下响应都不含 token。
//
// 做法：构造几种 run 状态，逐一检查响应体。
func TestLoginStatusNeverLeaksTokenFields(t *testing.T) {
	secret := "eyJhbGciOiJSUzI1NiJ9.LEAKME.LEAKSIG"

	states := []*loginRun{
		{phase: "waiting", cancelCh: make(chan struct{})},
		{phase: "done", done: true, uid: "abcd1234…", nickname: "13800000001", cancelCh: make(chan struct{})},
		{phase: "failed", done: true, errMsg: "等待登录超时，请重试。", cancelCh: make(chan struct{})},
		{phase: "failed", done: true, errMsg: secret, cancelCh: make(chan struct{})}, // 恶意/异常内容
	}

	for i, run := range states {
		activeLogin.mu.Lock()
		prev := activeLogin.current
		activeLogin.current = run
		activeLogin.mu.Unlock()

		rec := newRecorder()
		handleLoginStatus(Deps{}, rec, mustRequest(t, http.MethodGet, "/x", ""))
		body := rec.Body.String()

		// 🔴 不得出现原始 token（即使用恶意 errMsg 注入）
		if strings.Contains(body, secret) {
			t.Errorf("状态 %d：响应体泄露了 token: %s", i, body)
		}
		// 🔴 不得出现任何 token 字段名。
		assertNoTokenFields(t, body)

		activeLogin.mu.Lock()
		activeLogin.current = prev
		activeLogin.mu.Unlock()
	}
}

// assertNoTokenFields 断言响应体里没有凭据字段名。
//
// 检查字段名（accessToken/refreshToken/...）而不是值 —— 因为值我们
// 本来就不该有；出现字段名就意味着有人把凭据结构直接序列化出去了。
func assertNoTokenFields(t *testing.T, body string) {
	t.Helper()
	for _, f := range []string{"accessToken", "refreshToken", "machineToken", "idToken", "Authorization"} {
		if strings.Contains(body, f) {
			t.Errorf("响应体含凭据字段 %q: %s", f, body)
		}
	}
}
