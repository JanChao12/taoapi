package app

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
)

// ─────────────────────────────────────────────────────────────
// 面板「删除账号」的护栏测试（2026-10-06）
//
// 委托方要求（原话）：「你需要做一个删除账号按钮」
//
// 🔴 这是本批需求里**唯一不可逆**的操作，所以护栏要覆盖：
//  1. 真的删掉了，且**落盘**（否则重启后账号"复活"）
//  2. 只删目标账号，不误伤其他（多删即不可逆的数据损失）
//  3. 与批量动作并发时**不会复活**已删账号（Codex 第 36 轮 Q4）
//  4. 走 CSRF 防护（否则恶意网页能删光账号）
//
// ⚠️ 测试用的账号文件在 t.TempDir()，**绝不碰真实 accounts.json** ——
// 本项目曾因写账号文件丢失全部凭据（见 AGENTS.md）。
// ─────────────────────────────────────────────────────────────

// newRemoveEnv 起一个带账号与**真实 persister**的服务。
//
// 为什么用真实 Persister（而不是假实现）：auth.Persister 是具体类型
// （persist.go:63），无法替换成接口假件；而"删除是否落盘"恰恰是本组
// 测试的核心断言之一 —— 用真实实现 + 临时目录反而更可信。
func newRemoveEnv(t *testing.T) (*httptest.Server, *auth.Store, *auth.Persister) {
	t.Helper()

	dir := t.TempDir()
	// 用与生产完全一致的 codec（storageCodec 在 Windows 上是 DPAPI）——
	// 这样"落盘后能读回"的断言才有意义。
	persister := auth.NewPersister(filepath.Join(dir, "accounts.json"), storageCodec())
	st := auth.NewStore()

	cfg, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Logger:     log.New(io.Discard, "", 0),
		Accounts:   st,
		Persister:  persister,
		Settings:   cfg,
		Router:     router.New(),
		ListenPort: mustFreePort(t), // guardManagementAPI 的 origin 校验需要
		panelToken: testPanelToken,
	}
	srv := httptest.NewServer(newMux(deps))
	t.Cleanup(srv.Close)
	return srv, st, persister
}

// mustFreePort 取一个本机空闲端口号。
func mustFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// postActionWithToken 发带 CSRF 令牌的面板动作请求。
//
// 与 api_accounts_test.go 的 postAction 的区别：那个**不带令牌**（用于
// 测不套 CSRF 的旧路径），删除走 guardManagementAPI，必须带令牌。
func postActionWithToken(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	return postActionWithHeaders(t, srv.URL+"/api/accounts/action", body,
		map[string]string{panelTokenHeader: testPanelToken})
}

// postActionWithHeaders 发 POST 并返回状态码与原始响应体。
func postActionWithHeaders(t *testing.T, url, body string, h map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestRemoveAccountDeletesAndPersists 守：删除真的生效，且**写了盘**。
//
// 若不落盘：Store.Remove 只改内存，重启后账号会"复活"。
func TestRemoveAccountDeletesAndPersists(t *testing.T) {
	srv, st, persister := newRemoveEnv(t)
	st.Put(&auth.Account{UID: "13800000009", Nickname: "测试号"})

	code, body := postActionWithToken(t, srv, `{"uid":"13800000009","action":"remove"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", code, body)
	}

	if _, ok := st.Get("13800000009"); ok {
		t.Error("账号仍在存储里 —— 删除没生效")
	}

	// 落盘验证：从磁盘重新加载，账号必须**不在**
	reloaded, err := persister.Load()
	if err != nil {
		t.Fatalf("重新加载账号文件失败: %v", err)
	}
	if _, ok := reloaded.Get("13800000009"); ok {
		t.Error("账号已从内存删除但仍留在磁盘上 —— 重启后会复活")
	}
}

// TestRemoveNonexistentAccountReturns404 守：删不存在的账号要有明确错误。
func TestRemoveNonexistentAccountReturns404(t *testing.T) {
	srv, _, _ := newRemoveEnv(t)

	code, body := postActionWithToken(t, srv, `{"uid":"nobody-here","action":"remove"}`)
	if code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404，body=%s", code, body)
	}
	if !strings.Contains(body, "account_not_found") {
		t.Errorf("错误码不对，body=%s", body)
	}
}

// TestRemoveOnlyRemovesTargetAccount 守：只删目标账号，不误伤其他。
//
// 凭据不可恢复，多删一个账号是不可逆的数据损失。
func TestRemoveOnlyRemovesTargetAccount(t *testing.T) {
	srv, st, _ := newRemoveEnv(t)
	st.Put(&auth.Account{UID: "keep-1", Nickname: "保留1"})
	st.Put(&auth.Account{UID: "delete-me", Nickname: "要删的"})
	st.Put(&auth.Account{UID: "keep-2", Nickname: "保留2"})

	code, body := postActionWithToken(t, srv, `{"uid":"delete-me","action":"remove"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", code, body)
	}

	if _, ok := st.Get("delete-me"); ok {
		t.Error("目标账号没被删掉")
	}
	for _, uid := range []string{"keep-1", "keep-2"} {
		if _, ok := st.Get(uid); !ok {
			t.Errorf("账号 %s 被误删了 —— 删除只应影响目标账号", uid)
		}
	}
}

// TestRemoveDuringBulkDoesNotResurrect 守：并发下已删账号**不会复活**。
//
// 🔴 对应 Codex 第 36 轮 Q4 的裁定：批量动作遍历 List() 的共享指针快照
// 期间做上游调用；若此刻删除该账号，在途请求的回写走 Mutate(uid)，
// 而账号已从 map 删除 ⇒ Mutate 返回 false，**不写入** ⇒ 不复活。
func TestRemoveDuringBulkDoesNotResurrect(t *testing.T) {
	_, st, _ := newRemoveEnv(t)
	st.Put(&auth.Account{UID: "in-flight", Nickname: "在途"})

	// 模拟"批量动作已拿到快照"——这正是 handleCheckinAll 的第一行
	snapshot := st.List()
	if len(snapshot) != 1 {
		t.Fatalf("快照应有 1 个账号，实际 %d", len(snapshot))
	}

	// 此刻用户删除了该账号
	if !st.Remove("in-flight") {
		t.Fatal("Remove 应返回 true")
	}

	// 在途请求跑完，按 uid 回写（handleCheckinAll 里的 Mutate 调用）
	wrote := st.Mutate("in-flight", func(a *auth.Account) bool {
		a.CheckinDay = "2026-10-06"
		return true
	})

	if wrote {
		t.Error("Mutate 对已删除账号返回了 true —— 回写路径会写脏数据")
	}
	if _, ok := st.Get("in-flight"); ok {
		t.Error("已删除的账号被在途请求复活了 —— 这正是 Codex 点名的风险")
	}
	if snapshot[0] == nil {
		t.Error("快照指针不应为 nil（Go 不会让已取到的指针悬空）")
	}
}

// TestRemoveActionIsGuardedByCSRF 守：删除走 guardManagementAPI（防 CSRF）。
//
// 若没有防护，用户浏览器里任何打开的恶意网页都能悄悄把账号删光。
func TestRemoveActionIsGuardedByCSRF(t *testing.T) {
	srv, st, _ := newRemoveEnv(t)
	st.Put(&auth.Account{UID: "victim", Nickname: "受害者"})

	// 刻意**不带** X-WBAPI-Panel 令牌
	code, _ := postActionWithHeaders(t, srv.URL+"/api/accounts/action",
		`{"uid":"victim","action":"remove"}`, nil)

	if code == http.StatusOK {
		t.Error("不带 CSRF 令牌的删除请求被接受了 —— 恶意网页可以删光账号")
	}
	if _, ok := st.Get("victim"); !ok {
		t.Error("CSRF 拦截失败：账号已被删除")
	}
}

// TestRemoveCancelsInFlightLogin 守：删除时会取消进行中的登录。
//
// Codex 第 36 轮 (c)：「应取消与该账号关联的进行中登录会话。」
//
// ⚠️ 本测试只验证"取消动作被触发了"，**不能**证明"取消一定来得及" ——
// Codex 明确指出：已进入提交阶段的登录无法回滚。那段窗口是已知残留风险。
func TestRemoveCancelsInFlightLogin(t *testing.T) {
	run := &loginRun{phase: "starting", cancelCh: make(chan struct{})}
	activeLogin.mu.Lock()
	activeLogin.current = run
	activeLogin.mu.Unlock()
	t.Cleanup(func() {
		activeLogin.mu.Lock()
		activeLogin.current = nil
		activeLogin.mu.Unlock()
	})

	srv, st, _ := newRemoveEnv(t)
	st.Put(&auth.Account{UID: "logging-in", Nickname: "登录中"})

	code, body := postActionWithToken(t, srv, `{"uid":"logging-in","action":"remove"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", code, body)
	}

	run.mu.Lock()
	canceled := run.canceled
	run.mu.Unlock()
	if !canceled {
		t.Error("删除账号时没有取消进行中的登录 —— " +
			"登录成功后可能把凭据写回已删除的账号")
	}
}

// TestUnknownActionDoesNotDelete 守：未知动作不能被"猜"成删除。
func TestUnknownActionDoesNotDelete(t *testing.T) {
	srv, st, _ := newRemoveEnv(t)
	st.Put(&auth.Account{UID: "auto-victim", Nickname: "不该被自动删"})

	code, body := postActionWithToken(t, srv, `{"uid":"auto-victim","action":"remove_all"}`)
	if code == http.StatusOK {
		t.Errorf("未知动作被接受了，body=%s", body)
	}
	if _, ok := st.Get("auto-victim"); !ok {
		t.Error("未知动作竟然删除了账号")
	}
}
