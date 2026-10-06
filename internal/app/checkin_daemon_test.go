package app

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
)

// postJSON 发一个 JSON POST，返回状态码与响应体。
func postJSON(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ─────────────────────────────────────────────────────────────
// 自动签到守护
// ─────────────────────────────────────────────────────────────

// TestTodayCNConsistency 验证内置签到与 CLI 用同一归属日（UTC+8）。
//
// 两者若口径不同，会出现"守护签了、计划任务又签一遍"或漏签。
func TestTodayCNConsistency(t *testing.T) {
	today := todayCN()
	if len(today) != 10 || today[4] != '-' || today[7] != '-' {
		t.Fatalf("todayCN 格式异常: %q", today)
	}

	// UTC+8 的"今天"必落在 UTC 昨天/今天/明天三者之内
	utcToday := time.Now().UTC().Format("2006-01-02")
	utcTomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	utcYesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if today != utcToday && today != utcTomorrow && today != utcYesterday {
		t.Fatalf("todayCN = %q 超出 UTC 昨天/今天/明天范围（时区换算错误）", today)
	}
}

// TestRunDueCheckinsNilClientSafe 验证未装配上游客户端时守护不 panic。
//
// 这是一条真实风险：providerForAccount 在 WBClient==nil 时返回 nil，
// 若直接 .Checkin(...) 会 nil 解引用把整个进程打挂。
func TestRunDueCheckinsNilClientSafe(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a", "uid-b")

	// 确保两个账号都"未签到"，逼守护走到 provider 调用分支
	for _, uid := range []string{"uid-a", "uid-b"} {
		store.Mutate(uid, func(x *auth.Account) bool {
			x.CheckinDay = ""
			return true
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runDueCheckins(deps, nil) // 不应 panic
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runDueCheckins 卡住了")
	}
}

// TestRunDueCheckinsSkipsDisabled 验证人工禁用账号不参与自动签到。
func TestRunDueCheckinsSkipsDisabled(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-off")

	a, _ := store.Get("uid-off")
	a.ManualDisabled = true
	before := a.CheckinDay

	runDueCheckins(deps, nil)

	a2, _ := store.Get("uid-off")
	if a2.CheckinDay != before {
		t.Error("禁用账号不该被自动签到触碰")
	}
}

// TestCheckinDaemonDefaultOff 守：守护默认【不】启用。
//
// 🔴 委托人 2026-10-05 明确"自动签到默认关"。
// 早期版本是"启动即签"，这个测试防止回退成默认开启。
func TestCheckinDaemonDefaultOff(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")
	d, _ := newCheckinDaemon(deps)

	if st := d.Status(); st.Enabled {
		t.Error("守护刚创建时不应处于启用状态（委托人要求默认关闭）")
	}
}

// TestCheckinDaemonStartStop 验证启用/停用切换，且重复调用安全。
func TestCheckinDaemonStartStop(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")
	d, ready := newCheckinDaemon(deps)
	close(ready) // 模拟账号已就绪

	d.Start()
	if st := d.Status(); !st.Enabled {
		t.Fatal("Start 后应为启用状态")
	}
	d.Start() // 幂等：重复 Start 不应起第二个循环
	if st := d.Status(); !st.Enabled {
		t.Error("重复 Start 后仍应为启用")
	}

	d.Stop()
	if st := d.Status(); st.Enabled {
		t.Error("Stop 后应为停用状态")
	}
	d.Stop() // 幂等：重复 Stop 必须安全
	d.Stop()
}

// TestCheckinDaemonStopIsImmediate 守：停用立即生效，不是"等下次启动"。
//
// 委托人明确要求"关掉就停"。这里验证 Stop 能让循环在下一轮前退出，
// 而不是继续跑完整轮调度。
func TestCheckinDaemonStopIsImmediate(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")
	d, _ := newCheckinDaemon(deps)
	// 注意：故意 close(ready) 之前就 Stop，验证等待就绪的阶段也能被打断

	d.Start()
	d.Stop()

	// Stop 后状态必须是停用；且不应该再翻转回启用
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if d.Status().Enabled {
			t.Fatal("Stop 之后状态又变回启用了")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCheckinDaemonShutdownPreventsRestart 守：服务退出后不能再被启动。
func TestCheckinDaemonShutdownPreventsRestart(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")
	d, _ := newCheckinDaemon(deps)

	d.Shutdown()
	d.Start() // 退出后即使有人调 Start 也不该真的起循环

	if st := d.Status(); st.Enabled {
		t.Error("Shutdown 之后不应还能启用")
	}
}

// TestCheckinDaemonStatusFields 守状态字段可被面板读取。
func TestCheckinDaemonStatusFields(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")
	d, _ := newCheckinDaemon(deps)

	st := d.Status()
	if st.Running {
		t.Error("未启动时不应是 Running")
	}
	if !st.LastRunAt.IsZero() {
		t.Error("未触发过时 LastRunAt 应为零值")
	}
	if st.LastResult != "" {
		t.Errorf("未触发过时 LastResult 应为空，实际 %q", st.LastResult)
	}
}

// ─────────────────────────────────────────────────────────────
// 批量签到 / 批量刷新 API
// ─────────────────────────────────────────────────────────────

// TestBatchActionsNilClientNoPanic 验证无上游客户端时批量接口返回而非崩溃。
func TestBatchActionsNilClientNoPanic(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a", "uid-b")

	for _, action := range []string{"checkin_all", "refresh_all"} {
		code, body := postJSON(t, srv.URL+"/api/accounts/action",
			`{"action":"`+action+`"}`)
		if code != http.StatusOK {
			t.Errorf("%s 应 200，实际 %d body=%s", action, code, body)
		}
		if !strings.Contains(body, `"total":2`) {
			t.Errorf("%s 应报告 total=2，实际 %s", action, body)
		}
	}
}

// TestBatchCheckinSkipsAlreadySignedToday 验证批量签到跳过今日已签的账号。
func TestBatchCheckinSkipsAlreadySignedToday(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")

	today := todayCN()
	store.Mutate("uid-a", func(x *auth.Account) bool {
		x.CheckinDay = today
		return true
	})

	code, body := postJSON(t, srv.URL+"/api/accounts/action", `{"action":"checkin_all"}`)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	if !strings.Contains(body, "今日已签到") {
		t.Errorf("已签账号应报『今日已签到』且不发请求，实际 %s", body)
	}
}

// TestActionMissingUIDStillValidated 验证既无 uid 也非批量动作时给出 400。
func TestActionMissingUIDStillValidated(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")

	code, body := postJSON(t, srv.URL+"/api/accounts/action", `{"action":"nonsense"}`)
	if code != http.StatusBadRequest {
		t.Errorf("未知动作应 400，实际 %d body=%s", code, body)
	}
}

// ─────────────────────────────────────────────────────────────
// 面板导入 API
// ─────────────────────────────────────────────────────────────

// TestImportAPIValidation 验证导入校验与成功路径。
func TestImportAPIValidation(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
	url := srv.URL + "/api/accounts/import"

	cases := []struct {
		name string
		body string
		want int
	}{
		{"空对象", `{}`, http.StatusBadRequest},
		{"accounts 为空数组", `{"accounts":[]}`, http.StatusBadRequest},
		{"坏 JSON", `{not json`, http.StatusBadRequest},
		{"缺 uid/token 的项被计失败", `{"accounts":[{"nickname":"x"}]}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := postJSON(t, url, c.body)
			if code != c.want {
				t.Errorf("状态码 = %d，期望 %d，body=%s", code, c.want, body)
			}
		})
	}
}

// TestImportAPIAddsAccount 验证导入写入仓库、响应不回显 token。
func TestImportAPIAddsAccount(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")

	body := `{"accounts":[{"uid":"uid-imported-0001","nickname":"新号",
		"accessToken":"SECRET-TOKEN-XYZ","refreshToken":"SECRET-REFRESH-XYZ",
		"domain":"www.codebuddy.cn"}]}`
	code, resp := postJSON(t, srv.URL+"/api/accounts/import", body)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", code, resp)
	}

	if strings.Contains(resp, "SECRET-TOKEN-XYZ") || strings.Contains(resp, "SECRET-REFRESH-XYZ") {
		t.Fatal("🔴 导入响应回显了 token")
	}

	a, ok := store.Get("uid-imported-0001")
	if !ok {
		t.Fatal("账号未被写入仓库")
	}
	if a.AccessToken != "SECRET-TOKEN-XYZ" {
		t.Error("token 未正确保存")
	}
	if a.Nickname != "新号" {
		t.Errorf("昵称 = %q", a.Nickname)
	}
}

// TestImportAPIUpdatesExisting 验证同 uid 重复导入是更新而非新增。
func TestImportAPIUpdatesExisting(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
	before := store.Len()

	postJSON(t, srv.URL+"/api/accounts/import",
		`{"accounts":[{"uid":"uid-a","nickname":"改过的名字","accessToken":"new-token"}]}`)

	if store.Len() != before {
		t.Errorf("重复 uid 不应新增账号：之前 %d，之后 %d", before, store.Len())
	}
	a, _ := store.Get("uid-a")
	if a.Nickname != "改过的名字" {
		t.Errorf("昵称应被更新，实际 %q", a.Nickname)
	}
	if a.AccessToken != "new-token" {
		t.Errorf("token 应被更新")
	}
}

// TestImportConcurrentWithSchedule 验证并发导入与调度快照不互相破坏。
func TestImportConcurrentWithSchedule(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		for i := 0; i < 20; i++ {
			buf.Reset()
			buf.WriteString(`{"accounts":[{"uid":"uid-conc-`)
			buf.WriteString(string(rune('a' + i%26)))
			buf.WriteString(`","nickname":"n","accessToken":"t"}]}`)
			postJSON(t, srv.URL+"/api/accounts/import", buf.String())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			_ = store.PoolAccounts(time.Now())
		}
	}()
	wg.Wait()

	if _, ok := store.Get("uid-a"); !ok {
		t.Fatal("并发导入后原账号丢失")
	}
}
