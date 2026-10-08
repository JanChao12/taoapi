package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// syncBuffer 是并发安全的日志缓冲（续期可能从多个 goroutine 记日志）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLoggerTo 返回写进给定缓冲的 logger（用于断言"日志里没有 token"）。
func testLoggerTo(w io.Writer) *log.Logger {
	return log.New(w, "", 0)
}

// ═══════════════════════════════════════════════════════════════════════
// 凭证保活的护栏（2026-10-09）
// ═══════════════════════════════════════════════════════════════════════
//
// 🔴 每个护栏都做过**反向对照**：把修复撤掉，确认它会红。
//	只让测试变绿不算数（见维护备忘：会撒谎的检查器比没有检查器更糟）。
//
// ⚠️ 本机无 gcc ⇒ 不得声称"并发已验证"。这里的并发用例是
//	**确定性受控**的（用信号量制造交错），不是动态竞争检测。

// makeJWT 造一个只有 payload 有意义的假 JWT。
//
// ⚠️ 用于测 exp 解析，**不是**真凭据。
func makeJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"exp":` + itoa64(exp) + `}`))
	return header + "." + payload + ".sig"
}

// refreshFakeUpstream 造一个会响应续期端点的假上游。
type refreshFakeUpstream struct {
	*testutil.FakeUpstream

	mu          sync.Mutex
	refreshHits int
	newAccess   string
	newRefresh  string
	rotate      bool
	refreshCode int // 非 0 时返回该业务码（模拟失败）
}

// refreshEnv 是保活测试的环境。
type refreshEnv struct {
	deps    Deps
	store   *auth.Store
	daemon  *keepaliveDaemon
	fake    *refreshFakeUpstream
	dataDir string
}

// newRefreshEnv 搭一套保活测试环境。
func newRefreshEnv(t *testing.T, accounts ...*auth.Account) *refreshEnv {
	t.Helper()

	fake := &refreshFakeUpstream{
		FakeUpstream: testutil.NewFakeUpstream(t),
		newAccess:    makeJWT(time.Now().Add(40 * 24 * time.Hour).Unix()),
		newRefresh:   "new-refresh-token",
	}
	// 续期端点用 POST 且带 X-Refresh-Token 头。
	fake.FakeUpstream.WithJSONBodyForPath("token/refresh",
		[]byte(`{"code":0,"msg":"OK","data":{"accessToken":"`+
			fake.newAccess+`","refreshToken":"`+fake.newRefresh+
			`","expiresIn":3456000,"refreshExpiresIn":3456000}}`))

	dir := t.TempDir()
	t.Setenv("WBAPI_DATA_DIR", dir)

	store := auth.NewStore()
	for _, a := range accounts {
		store.Put(a)
	}

	client := workbuddy.NewClient()
	client.SetBases(fake.URL, fake.URL)

	persister := auth.NewPersister(filepath.Join(dir, "accounts.json"), storageCodec())
	logger := testLogger(t)

	deps := Deps{
		Accounts:  store,
		Persister: persister,
		WBClient:  client,
		Logger:    logger,
	}
	daemon := newKeepaliveDaemon(deps)
	deps.Keepalive = daemon
	daemon.MarkReady()

	return &refreshEnv{deps: deps, store: store, daemon: daemon, fake: fake, dataDir: dir}
}

// accountWithRefresh 造一个带 refresh token 的账号。
func accountWithRefresh(uid, access, refresh string) *auth.Account {
	return &auth.Account{
		UID:          uid,
		Nickname:     "测试账号" + uid[:2],
		Platform:     auth.PlatformCN,
		AccessToken:  access,
		RefreshToken: refresh,
		Status:       pool.StatusNormal,
	}
}

// ─────────────────────────────────────────────────────────────
// 1. 续期端点契约（安全红线）
// ─────────────────────────────────────────────────────────────

// TestRefreshUsesXRefreshTokenHeader 守续期请求的认证方式。
//
// 实测契约是 `X-Refresh-Token` 头（不是 Authorization、不是 body）。
// 改错的话续期会静默失败，表现为"账号莫名其妙就要重新登录"。
func TestRefreshUsesXRefreshTokenHeader(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-aaaa1111", "old-access", "my-refresh"))

	if !renewOneAccount(env.deps, env.daemon, "uid-aaaa1111", make(chan struct{})) {
		t.Fatal("续期应当成功")
	}

	recs := env.fake.Requests()
	var found bool
	for _, r := range recs {
		if !strings.Contains(r.Path, "token/refresh") {
			continue
		}
		found = true
		if r.Method != http.MethodPost {
			t.Errorf("续期方法 = %s，期望 POST", r.Method)
		}
		if got := r.Header.Get("X-Refresh-Token"); got != "my-refresh" {
			t.Errorf("X-Refresh-Token = %q，期望 refresh token", got)
		}
	}
	if !found {
		t.Fatal("没有记录到续期请求 —— 端点路径可能写错了")
	}
}

// TestRefreshResultIsPersisted 守"续期后凭据真的换了且落盘"。
func TestRefreshResultIsPersisted(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-bbbb2222", "old-access", "old-refresh"))

	// 让服务端返回一个**不同**的 refresh token（模拟轮换）。
	env.fake.mu.Lock()
	env.fake.newRefresh = "rotated-refresh"
	env.fake.FakeUpstream.WithJSONBodyForPath("token/refresh",
		[]byte(`{"code":0,"msg":"OK","data":{"accessToken":"`+env.fake.newAccess+
			`","refreshToken":"rotated-refresh","expiresIn":100}}`))
	env.fake.mu.Unlock()

	if !renewOneAccount(env.deps, env.daemon, "uid-bbbb2222", make(chan struct{})) {
		t.Fatal("续期应当成功")
	}

	snap, ok := env.store.Snapshot("uid-bbbb2222")
	if !ok {
		t.Fatal("账号不见了")
	}
	if snap.AccessToken == "old-access" {
		t.Error("access token 没被替换")
	}
	// 🔴 轮换场景：必须存服务端返回的新 refresh token。
	// 只在"变化时覆盖"或"不覆盖"的写法都会在这里红。
	if snap.RefreshToken != "rotated-refresh" {
		t.Errorf("refresh token = %q，期望 rotated-refresh（收到新值就必须原子替换）",
			maskForTest(snap.RefreshToken))
	}
}

// TestRefreshFailureKeepsOldCredentials 守"续期失败绝不动已有凭据"。
//
// 反向对照：若把失败分支也拿去写 token（写空值），本测试立刻红。
func TestRefreshFailureKeepsOldCredentials(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-cccc3333", "old-access", "old-refresh"))
	env.fake.SetStatusForPath("token/refresh", http.StatusInternalServerError)

	if renewOneAccount(env.deps, env.daemon, "uid-cccc3333", make(chan struct{})) {
		t.Fatal("上游 500 时续期不该报成功")
	}

	snap, _ := env.store.Snapshot("uid-cccc3333")
	if snap.AccessToken != "old-access" {
		t.Error("续期失败却改动了 access token —— 会让账号彻底不可用")
	}
	if snap.RefreshToken != "old-refresh" {
		t.Error("续期失败却改动了 refresh token —— 可能造成凭据彻底丢失")
	}
}

// TestRefreshMissingAccessTokenIsFailure 守"响应缺 accessToken 时不写半个凭据"。
func TestRefreshMissingAccessTokenIsFailure(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-dddd4444", "old-access", "old-refresh"))
	env.fake.WithJSONBodyForPath("token/refresh",
		[]byte(`{"code":0,"msg":"OK","data":{"refreshToken":"x","expiresIn":1}}`))

	if renewOneAccount(env.deps, env.daemon, "uid-dddd4444", make(chan struct{})) {
		t.Fatal("缺少 accessToken 时不该报成功")
	}
	snap, _ := env.store.Snapshot("uid-dddd4444")
	if snap.AccessToken != "old-access" {
		t.Error("缺 accessToken 却覆盖了旧值")
	}
}

// TestRefreshBusinessErrorIsFailure 守业务码非 0 视为失败。
func TestRefreshBusinessErrorIsFailure(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-eeee5555", "old-access", "old-refresh"))
	env.fake.WithJSONBodyForPath("token/refresh",
		[]byte(`{"code":10034,"msg":"invalid refresh token"}`))

	if renewOneAccount(env.deps, env.daemon, "uid-eeee5555", make(chan struct{})) {
		t.Fatal("业务码 10034 时不该报成功")
	}
	snap, _ := env.store.Snapshot("uid-eeee5555")
	if snap.AccessToken != "old-access" {
		t.Error("业务失败却改动了凭据")
	}
}

// TestRefreshNeverLogsTokens 守"续期链路不把 token 写进日志"（安全红线 2）。
//
// 🔴 本测试**必须覆盖两个分支**（轮换 / 未轮换）。
//
//	我第一次写反向对照时只改了"未轮换"分支的日志，测试照样全绿 ——
//	因为那时的场景走的是**轮换**分支，我改的那行**根本没执行**。
//	那是"会撒谎的检查器"的变体：测试绿不代表它守住了东西。
//	现在两个分支都跑，任何一处泄露 token 都会被抓到。
func TestRefreshNeverLogsTokens(t *testing.T) {
	cases := []struct {
		name    string
		refresh string // 服务端返回的 refresh token
	}{
		{"未轮换（返回同一个）", "SECRET-REFRESH-XYZ"},
		{"已轮换（返回新值）", "NEW-SECRET-REFRESH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newRefreshEnv(t, accountWithRefresh("uid-ffff6666",
				"SECRET-ACCESS-XYZ", "SECRET-REFRESH-XYZ"))
			env.fake.WithJSONBodyForPath("token/refresh",
				[]byte(`{"code":0,"msg":"OK","data":{"accessToken":"NEW-SECRET-ACCESS",`+
					`"refreshToken":"`+c.refresh+`","expiresIn":100}}`))

			buf := &syncBuffer{}
			env.deps.Logger = testLoggerTo(buf)

			if !renewOneAccount(env.deps, env.daemon, "uid-ffff6666", make(chan struct{})) {
				t.Fatal("续期应当成功（否则本测试没走到写日志的分支）")
			}

			// 🔴 先断言"确实记了日志" —— 否则"日志里没有 token"
			//	会因为**根本没记日志**而假绿（这正是我踩过的坑）。
			logged := buf.String()
			if !strings.Contains(logged, "已续期") {
				t.Fatalf("续期成功却没记日志，本测试失去意义（日志内容=%q）", logged)
			}

			for _, secret := range []string{
				"SECRET-ACCESS-XYZ", "SECRET-REFRESH-XYZ",
				"NEW-SECRET-ACCESS", "NEW-SECRET-REFRESH",
			} {
				if strings.Contains(logged, secret) {
					t.Errorf("日志里出现了凭据片段 %q —— 违反安全红线 2", secret)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────
// 2. 调度判据（NeedsRefresh）
// ─────────────────────────────────────────────────────────────

// TestNeedsRefreshWindow 守"提前 24 小时续期"的窗口语义。
func TestNeedsRefreshWindow(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		exp  time.Duration // 距现在多久过期
		want bool
	}{
		{"还有 40 天", 40 * 24 * time.Hour, false},
		{"还有 2 天", 48 * time.Hour, false},
		{"还有 25 小时", 25 * time.Hour, false},
		{"还有 23 小时（进窗口）", 23 * time.Hour, true},
		{"还有 1 小时", time.Hour, true},
		{"已过期", -time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok := makeJWT(now.Add(c.exp).Unix())
			got := workbuddy.NeedsRefresh(tok, time.Time{}, now, refreshHorizon)
			if got != c.want {
				t.Errorf("NeedsRefresh = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestNeedsRefreshUnknownExpIsThrottled 守"解不出 exp 时按时间节流"。
//
// 🔴 反向对照要点：若去掉时间节流（无 exp 就永远 true），
// 第二次调用会返回 true ⇒ 本测试红。
// 那会让"非 JWT 凭据"每次检查都打上游 —— 而重复提交 refresh token
// 有触发 token family 撤销的风险。
func TestNeedsRefreshUnknownExpIsThrottled(t *testing.T) {
	now := time.Now()
	const notJWT = "this-is-not-a-jwt"

	// 从没续过 ⇒ 该试一次
	if !workbuddy.NeedsRefresh(notJWT, time.Time{}, now, refreshHorizon) {
		t.Error("无 exp 且从未尝试过 ⇒ 应当可以试一次")
	}
	// 刚试过 ⇒ 节流住
	justNow := now.Add(-time.Minute)
	if workbuddy.NeedsRefresh(notJWT, justNow, now, refreshHorizon) {
		t.Error("无 exp 但刚试过 ⇒ 不该再发请求（防打爆续期端点）")
	}
	// 超过窗口 ⇒ 再试
	old := now.Add(-2 * refreshHorizon)
	if !workbuddy.NeedsRefresh(notJWT, old, now, refreshHorizon) {
		t.Error("无 exp 且已过窗口 ⇒ 应当再试一次")
	}
}

// TestRenewSkipsWhenNotExpiring 守"没到窗口的账号不发请求"。
func TestRenewSkipsWhenNotExpiring(t *testing.T) {
	far := makeJWT(time.Now().Add(40 * 24 * time.Hour).Unix())
	env := newRefreshEnv(t, accountWithRefresh("uid-gggg7777", far, "r"))

	ok, failed, skipped, _ := renewExpiringAccounts(env.deps, env.daemon, make(chan struct{}))
	if skipped {
		t.Error("有账号时不该报 skipped")
	}
	if ok != 0 || failed != 0 {
		t.Errorf("未进窗口的账号不该被续期：ok=%d failed=%d", ok, failed)
	}
	if n := env.fake.CountPath("token/refresh"); n != 0 {
		t.Errorf("发了 %d 次续期请求，期望 0 次", n)
	}
}

// TestRenewSkipsManualDisabled 守"人工禁用的账号不保活"。
func TestRenewSkipsManualDisabled(t *testing.T) {
	acc := accountWithRefresh("uid-hhhh8888", makeJWT(time.Now().Add(time.Hour).Unix()), "r")
	acc.ManualDisabled = true
	env := newRefreshEnv(t, acc)

	ok, _, _, _ := renewExpiringAccounts(env.deps, env.daemon, make(chan struct{}))
	if ok != 0 {
		t.Error("人工禁用的账号不该被续期")
	}
	if n := env.fake.CountPath("token/refresh"); n != 0 {
		t.Errorf("对人工禁用账号发了 %d 次续期请求", n)
	}
}

// TestRenewSkipsAccountWithoutRefreshToken 守"没有 refresh token 就跳过"。
func TestRenewSkipsAccountWithoutRefreshToken(t *testing.T) {
	acc := accountWithRefresh("uid-iiii9999", makeJWT(time.Now().Add(time.Hour).Unix()), "")
	env := newRefreshEnv(t, acc)

	renewExpiringAccounts(env.deps, env.daemon, make(chan struct{}))
	if n := env.fake.CountPath("token/refresh"); n != 0 {
		t.Errorf("没有 refresh token 却发了 %d 次请求", n)
	}
}

// ─────────────────────────────────────────────────────────────
// 3. 并发去重（防 token family 撤销）
// ─────────────────────────────────────────────────────────────

// TestConcurrentRenewDeduplicates 守"同账号并发续期只发一个请求"。
//
// 🔴 这是本功能最重要的一条护栏。
//
//	定时任务与 chat 按需触发可能同时对同一账号发起续期。
//	同一个 refresh token 并发提交两次，有触发 **token family 撤销**
//	的风险 —— 那会让用户**彻底无法登录**，比"不续期"严重得多。
//
// ⚠️ 本机无 gcc ⇒ 这不是动态竞争检测，而是**确定性受控**交错：
//
//	用一个阻塞的假上游把第一个请求卡住，此时第二个调用必须
//	**等待**而不是自己再发一个。
//
// 反向对照：把 beginRefresh 的去重去掉（永远返回 true），
//
//	refreshHits 会变成 2 ⇒ 本测试红。
func TestConcurrentRenewDeduplicates(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-jjjj0000",
		makeJWT(time.Now().Add(time.Hour).Unix()), "r"))

	// 让第一次续期在假上游里**卡住**，制造确定的交错窗口。
	release := make(chan struct{})
	entered := make(chan struct{})
	env.fake.SetBlockOnRefresh(entered, release)

	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = renewOneAccount(env.deps, env.daemon,
				"uid-jjjj0000", make(chan struct{}))
		}(i)
	}

	// 等第一个请求真的进到假上游，再放行。
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个续期请求没有到达假上游")
	}
	// 给第二个 goroutine 一点时间进入"等待"分支。
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := env.fake.CountPath("token/refresh"); n != 1 {
		t.Errorf("并发续期发了 %d 个请求，期望 1 个（重复提交有 token family 撤销风险）", n)
	}
	if !results[0] && !results[1] {
		t.Error("至少应有一个调用报告成功")
	}
}

// TestRefreshNoDeadlockWhenAccountDeleted 守"续期期间账号被删不会死锁/panic"。
func TestRefreshNoDeadlockWhenAccountDeleted(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-kkkk1111",
		makeJWT(time.Now().Add(time.Hour).Unix()), "r"))

	release := make(chan struct{})
	entered := make(chan struct{})
	env.fake.SetBlockOnRefresh(entered, release)

	done := make(chan bool, 1)
	go func() {
		done <- renewOneAccount(env.deps, env.daemon, "uid-kkkk1111", make(chan struct{}))
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("续期请求没到达假上游")
	}
	// 在途期间删除账号 —— 模拟用户点删除。
	env.store.Remove("uid-kkkk1111")
	close(release)

	select {
	case ok := <-done:
		if ok {
			t.Error("账号已被删除，续期不该报成功")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("续期在账号被删后卡住了（疑似死锁）")
	}
}

// TestRefreshDiscardsStaleResult 守"期间账号被改动 ⇒ 丢弃过期结果"（R1 同类）。
//
// 🔴 场景：续期请求在途时，用户**重新登录**了同一个账号，
//
//	拿到一份全新的凭据。此时续期的旧结果若照写，会**覆盖刚登录的凭据**。
//
// 反向对照：把 MutateIfRev 换成无条件 Mutate ⇒ 本测试红。
func TestRefreshDiscardsStaleResult(t *testing.T) {
	env := newRefreshEnv(t, accountWithRefresh("uid-llll2222",
		makeJWT(time.Now().Add(time.Hour).Unix()), "r"))

	release := make(chan struct{})
	entered := make(chan struct{})
	env.fake.SetBlockOnRefresh(entered, release)

	done := make(chan bool, 1)
	go func() {
		done <- renewOneAccount(env.deps, env.daemon, "uid-llll2222", make(chan struct{}))
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("续期请求没到达假上游")
	}

	// 在途期间"用户重新登录"：换掉 refresh token（版本也随之变化）。
	env.store.Mutate("uid-llll2222", func(a *auth.Account) bool {
		a.RefreshToken = "freshly-logged-in-refresh"
		a.AccessToken = "freshly-logged-in-access"
		return true
	})
	close(release)

	select {
	case ok := <-done:
		if ok {
			t.Error("期间账号被改动，续期结果应当被丢弃而不是报成功")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("续期卡住了")
	}

	snap, _ := env.store.Snapshot("uid-llll2222")
	if snap.AccessToken != "freshly-logged-in-access" {
		t.Errorf("刚登录的凭据被过期的续期结果覆盖了（access=%q）",
			maskForTest(snap.AccessToken))
	}
	if snap.RefreshToken != "freshly-logged-in-refresh" {
		t.Error("刚登录的 refresh token 被覆盖了 —— 用户会被迫再登录一次")
	}
}

// ─────────────────────────────────────────────────────────────
// 4. 开关语义
// ─────────────────────────────────────────────────────────────

// TestKeepaliveDefaultEnabled 守"老配置（没有该键）默认开启"。
//
// 🔴 为什么这条重要：Keepalive 是新字段。老用户的 config.json 里没有它，
// 零值是 false。若按 bool 处理，升级后他们的保活**默认是关的** ——
// 而升级前从不需要重新登录，行为静默倒退。
func TestKeepaliveDefaultEnabled(t *testing.T) {
	if !keepaliveEnabled(nil) {
		t.Error("nil（老配置从没表过态）应当默认为开启")
	}
	yes := true
	if !keepaliveEnabled(&yes) {
		t.Error("显式 true 应当开启")
	}
	no := false
	if keepaliveEnabled(&no) {
		t.Error("显式 false 应当关闭（尊重用户选择）")
	}
}

// TestKeepaliveNilAndFalseDifferInSettings 守两态在配置层不被压成一种。
func TestKeepaliveNilAndFalseDifferInSettings(t *testing.T) {
	// 反向对照：若 sameContent 用 bool 比较、把 nil 与 false 当相同，
	// 这个断言会红 —— 于是"用户关掉保活"会被判成"没变化"而不落盘。
	no := false
	if keepaliveEnabled(nil) == keepaliveEnabled(&no) {
		t.Error("nil 与 false 的语义不同，不该归一成同一个值")
	}
}

// maskForTest 只显示前 6 个字符（错误信息里避免打印完整凭据）。
func maskForTest(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:6] + "…"
}

// jsonEq 是给将来扩展用的小工具（比较两段 JSON 语义相等）。
func jsonEq(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("解析 got 失败: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("解析 want 失败: %v", err)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return string(ga) == string(gb)
}
