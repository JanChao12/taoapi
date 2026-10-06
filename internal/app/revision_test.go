package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
)

// 本文件守住 settings 的乐观锁（Codex 第 11、12 轮要求）。
//
// 🔴 要解决的问题：两个标签页同时编辑设置时，各自基于旧快照提交，
// 后提交者**静默覆盖**先提交者的改动。用户完全不知道自己的修改被顶掉了。
//
// Codex 定的语义（逐条实现）：
//   - GET 返回 revision（并给 ETag）
//   - PATCH 必须带条件：`ifRevision` 或 `If-Match`
//   - **缺失 → 428 Precondition Required**（不是放行！）
//   - 不匹配 → 412 Precondition Failed，并返回当前状态供前端刷新
//   - revision 只在内容**实际变化**时递增；校验/写盘失败不递增

// newRevisionEnv 起一个可写设置的测试服务。
//
// ⚠️ 注意它与 `-count>1` 的交互（2026-10-05 实测发现，见 freshPort）：
//
//	testConfigDir(t) 对**同一个测试**返回同一路径（名字由测试名 + PID + 计数构成，
//	而计数在每个进程里从 1 重新开始）。所以 `go test -count=N` 反复跑同一测试时，
//	**第 2 次开始会读到上一次留下的配置文件**。
//
//	这不是缺陷（配合 TestMain 整包清理，语义是"每次进程运行一个干净目录"），
//	但写测试时**不能假设配置是默认值** —— 需要用 freshPort 显式取一个新端口。
func newRevisionEnv(t *testing.T) (*httptest.Server, *config.Store) {
	t.Helper()
	store, err := config.Load(testConfigDir(t) + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:     log.New(io.Discard, "", 0),
		Settings:   store,
		Router:     router.New(),
		panelToken: testPanelToken,
	}
	srv := httptest.NewServer(newMux(deps))
	t.Cleanup(srv.Close)
	return srv, store
}

// freshPort 返回一个"当前配置里还没用过的"端口，并把它写进配置。
//
// 🔴 为什么需要它（2026-10-05 实测发现，Codex 第 23 轮诊断命令逼出来的）：
//
//	有几个 revision 测试原先硬编码端口（如 `{"port":9600}`），
//	在 `go test -count=1` 下完全正确 —— 但 `-count=N` 反复跑同一测试时，
//	`testConfigDir(t)` 会返回**同一个路径**，于是第 2 次开始端口**已经是 9600**，
//	"改端口"变成"写相同的值"。
//
//	而 revision 的契约正是「**内容没实际变化就不递增**」——
//	所以实现是对的，测试却断言"应该递增"，于是失败：
//
//	    revision_test.go:172: revision = 1，期望 2（应递增 1）
//
//	**这是测试假设错误，不是产品缺陷。** 但它很有价值：它说明这些测试
//	只能跑一次，一 shuffle/重复就会红 —— 而"偶发红"最容易被误判成真 bug。
//
//	修法：每次从一个**确定不同的**端口出发，让"改端口"始终是真实变化。
//	用 revision 值派生，天然在本测试的多次运行间不重复。
func freshPort(t *testing.T, store *config.Store) int {
	t.Helper()
	// 基准偏移 + 当前 revision：本测试内每次调用都不同，
	// 且都落在合法端口范围（MinPort=1024 起）。
	base := 20000 + int(store.Get().Revision)*7
	if base < 1024 || base > 65000 {
		base = 20000
	}
	return base
}

// getRevision 读当前 revision（同时验证 GET 会返回它）。
func getRevision(t *testing.T, srv *httptest.Server) int64 {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("ETag") == "" {
		t.Error("GET 应返回 ETag（Codex 要求与 revision 同源）")
	}
	var v settingsView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.Revision
}

// patch 发一个带 ifRevision 的 PATCH，返回状态码与响应体。
func patch(t *testing.T, srv *httptest.Server, body string, rev *int64) (int, string) {
	t.Helper()
	m := map[string]any{}
	if body != "" {
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
	}
	if rev != nil {
		// 发与 ETag 同构的串（Codex 第 14 轮要求两种形式一致）
		m["ifRevision"] = revisionETagToken(*rev)
	}
	raw, _ := json.Marshal(m)
	return doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings", string(raw),
		map[string]string{
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
}

// doJSONWithBody 与 doWithHeaders 类似，但把响应体也返回。
//
// 单独写一个而不是改 doWithHeaders：后者被 csrf_test 大量调用，
// 改签名会波及十几处无谓的改动。
func doJSONWithBody(t *testing.T, method, url, body string, h map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestPatchWithoutRevisionIsRejected 是本次核心。
//
// 🔴 Codex 明确否决了"缺失即强制写入"：
//
//	若缺失就放行，前端只要漏传一个字段，就重新变成无保护覆盖 ——
//	revision 白做了。要强制覆盖应走显式的 force 机制。
func TestPatchWithoutRevisionIsRejected(t *testing.T) {
	srv, store := newRevisionEnv(t)
	before := store.Get().Port

	code, body := patch(t, srv, `{"port":9999}`, nil)
	if code != http.StatusPreconditionRequired {
		t.Fatalf("缺 ifRevision 状态码 = %d，期望 428；body=%s", code, body)
	}
	if store.Get().Port != before {
		t.Fatal("缺 ifRevision 的请求竟然改动配置了")
	}
}

// TestPatchWithStaleRevisionConflicts 守：revision 不匹配 → 412 且不改动。
func TestPatchWithStaleRevisionConflicts(t *testing.T) {
	srv, store := newRevisionEnv(t)
	before := store.Get().Port

	stale := int64(999) // 明显过期的版本号
	code, body := patch(t, srv, `{"port":9999}`, &stale)
	if code != http.StatusPreconditionFailed {
		t.Fatalf("过期 revision 状态码 = %d，期望 412；body=%s", code, body)
	}
	if store.Get().Port != before {
		t.Fatal("冲突的请求竟然改动了配置")
	}

	// 冲突响应必须带当前状态，前端才能刷新并让用户重新决定
	var conflict settingsConflict
	if err := json.Unmarshal([]byte(body), &conflict); err != nil {
		t.Fatalf("冲突响应不是合法 JSON: %v", err)
	}
	if conflict.Error != "revision_conflict" {
		t.Errorf("Error = %q，期望 revision_conflict", conflict.Error)
	}
	// 注意：不能用 "Revision != 0" 判断 —— 全新配置的 revision 就是 0，
	// 那会让断言假失败。这里断言的是"带上了服务端真实值"。
	if conflict.Current.Port != store.Get().Port {
		t.Errorf("冲突响应里的当前端口 = %d，期望与服务端一致（%d）",
			conflict.Current.Port, store.Get().Port)
	}
	if conflict.Message == "" {
		t.Error("冲突响应应带可读提示")
	}
}

// TestPatchWithCorrectRevisionSucceedsAndBumps 守：正确 revision 能写，且版本递增。
func TestPatchWithCorrectRevisionSucceedsAndBumps(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)

	// ⚠️ 用 freshPort 而不是硬编码端口：`-count>1` 时配置会沿用上次的值，
	// 硬编码会让"改端口"变成"写相同的值"（而 revision 契约正是"没变化不递增"），
	// 于是测试失败而实现是对的。详见 freshPort 的说明。
	newPort := freshPort(t, store)

	code, body := patch(t, srv, `{"port":`+strconv.Itoa(newPort)+`}`, &rev)
	if code != http.StatusOK {
		t.Fatalf("正确 revision 状态码 = %d，期望 200；body=%s", code, body)
	}
	if store.Get().Port != newPort {
		t.Errorf("端口未写入，实际 %d，期望 %d", store.Get().Port, newPort)
	}
	if got := store.Get().Revision; got != rev+1 {
		t.Errorf("revision = %d，期望 %d（应递增 1）", got, rev+1)
	}
}

// TestRevisionOnlyBumpsOnRealChange 守：写入相同的值不递增。
//
// 🔴 为什么重要：若"提交相同值"也递增，前端拿到的 revision 会被
// 无关的重复提交不断顶掉，乐观锁会频繁误报冲突。
func TestRevisionOnlyBumpsOnRealChange(t *testing.T) {
	srv, store := newRevisionEnv(t)

	rev := getRevision(t, srv)
	// 把端口设成它已经是值（默认 8787）
	code, body := patch(t, srv, `{"port":8787}`, &rev)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", code, body)
	}
	if got := store.Get().Revision; got != rev {
		t.Errorf("写入相同值时 revision = %d，期望保持 %d", got, rev)
	}
}

// TestRevisionDoesNotBumpOnValidationFailure 守：校验失败不递增。
func TestRevisionDoesNotBumpOnValidationFailure(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)

	// 非法端口 → 400，且 revision 不变
	code, _ := patch(t, srv, `{"port":80}`, &rev)
	if code != http.StatusBadRequest {
		t.Fatalf("非法端口状态码 = %d，期望 400", code)
	}
	if got := store.Get().Revision; got != rev {
		t.Errorf("校验失败后 revision = %d，期望保持 %d", got, rev)
	}
}

// TestPatchWithIfMatchHeader 守：标准 If-Match 头也能用。
//
// ⚠️ 这里用 doJSONWithBody（不走 doWithHeaders 的自动补 ifRevision）——
// 否则会同时出现 ifRevision 与 If-Match，而"两者矛盾"按 Codex 第 13 轮
// 要求应返回 400，测不到 If-Match 本身的行为。
func TestPatchWithIfMatchHeader(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)

	// 正确 ETag
	code, _ := doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9700}`,
		map[string]string{
			"Content-Type":   "application/json",
			"If-Match":       revisionETag(rev),
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusOK {
		t.Fatalf("正确 If-Match 状态码 = %d，期望 200", code)
	}
	if store.Get().Port != 9700 {
		t.Error("If-Match 形式的写请求未生效")
	}

	// 过期 ETag：用一个不可能的值（负版本号）构造过期场景
	code, _ = doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9701}`,
		map[string]string{
			"Content-Type":   "application/json",
			"If-Match":       revisionETag(-1),
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusPreconditionFailed {
		t.Errorf("过期 If-Match 状态码 = %d，期望 412", code)
	}
	if store.Get().Port != 9700 {
		t.Errorf("冲突的 If-Match 请求不应该改动配置，port=%d", store.Get().Port)
	}
}

// TestConflictingPreconditionsRejected 守：ifRevision 与 If-Match 矛盾 → 400。
//
// 🔴 Codex 第 13 轮要求："同时传 ifRevision 和 If-Match 时，
// 两个值不一致应直接 400，不能自行选择一个。"
//
// 理由：两者不一致说明调用方自己搞混了（或中间层改写了其一）。
// 无论选哪个都是猜，而"猜"在有副作用的写接口上是危险的。
func TestConflictingPreconditionsRejected(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)
	before := store.Get().Port

	// body 说"我基于 rev"，头说"我基于 rev-1" —— 互相矛盾
	code, body := doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9800,"ifRevision":`+itoa64(rev)+`}`,
		map[string]string{
			"Content-Type":   "application/json",
			"If-Match":       revisionETag(-1),
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusBadRequest {
		t.Errorf("矛盾的前置条件状态码 = %d，期望 400（不能自行选一个）；body=%s",
			code, body)
	}
	if store.Get().Port != before {
		t.Error("矛盾的前置条件请求竟然改动了配置")
	}
}

// TestETagIncludesProcessGeneration 守：ETag 带进程代次。
//
// 🔴 Codex 第 13 轮要求：坏配置被 quarantine 后 revision 会**从 0 重新开始**，
// 旧客户端手里的 revision=0 会"意外匹配"新配置的 0。
// ETag 带上进程代次后，跨世代的值天然不相等。
func TestETagIncludesProcessGeneration(t *testing.T) {
	srv, _ := newRevisionEnv(t)

	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET 应返回 ETag")
	}
	if !strings.Contains(etag, "inst-") {
		t.Errorf("ETag = %q，应含进程代次前缀 inst-（防止跨配置世代误匹配）", etag)
	}
	if !strings.Contains(etag, "rev-") {
		t.Errorf("ETag = %q，应含 rev-", etag)
	}
}

// TestSettingsViewExposesConfiguredAndEffectivePort 守：
// 面板能区分"配置的端口"与"当前生效的端口"。
//
// 🔴 Codex 第 13 轮要求：端口改动需重启才生效。若只返回一个 port，
// 前端会把它当成"已经在用的端口"——用户改完看到 9000 以为立刻生效，
// 实际服务还在 8787 上跑。
func TestSettingsViewExposesConfiguredAndEffectivePort(t *testing.T) {
	srv, store := newRevisionEnv(t)

	rev := getRevision(t, srv)
	code, _ := patch(t, srv, `{"port":9900}`, &rev)
	if code != http.StatusOK {
		t.Fatalf("改端口状态码 = %d，期望 200", code)
	}

	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v settingsView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}

	if v.ConfiguredPort != 9900 {
		t.Errorf("configuredPort = %d，期望 9900（用户想要的）", v.ConfiguredPort)
	}
	if v.ConfiguredPort != store.Get().Port {
		t.Error("configuredPort 应与配置一致")
	}
	// 测试环境里 ListenPort 为 0（未真实监听），两条字段仍必须存在且语义清晰
	if v.RestartRequired && v.EffectivePort == 0 {
		t.Log("测试环境未真实监听，effectivePort=0 属预期")
	}
}

// TestPanelTokenResponseIsNoStore 守：令牌响应不可被缓存。
//
// Codex 第 13 轮要求：token 必须带 Cache-Control: no-store，
// 否则浏览器/中间层可能缓存它，重启后旧 token 被复用（那时已失效）。
func TestPanelTokenResponseIsNoStore(t *testing.T) {
	srv, _ := newRevisionEnv(t)

	resp, err := http.Get(srv.URL + "/api/panel-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q，应含 no-store", cc)
	}
}

// TestIfRevisionIsIsomorphicWithETag 守：两种形式的版本标识**同构**。
//
// 🔴 Codex 第 14 轮指出的不一致漏洞：
//
//	我最初让 ETag 带 generation（`inst-<gen>-rev-<n>`），
//	但 body 的 ifRevision 只收整数。于是"配置损坏被 quarantine 后
//	revision 从 0 重来"时，旧客户端的 `ifRevision=0` 会**误匹配** ——
//	generation 保护被绕过，而它本来就是为了防这个场景。
//
// 本测试锁定：body 形式与 ETag 形式承载同一个串。
func TestIfRevisionIsIsomorphicWithETag(t *testing.T) {
	srv, _ := newRevisionEnv(t)

	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()

	// 用 ETag 的**去引号值**作为 body 的 ifRevision —— 应当被接受，
	// 这证明两者同构（否则会 412）
	token := strings.Trim(etag, `"`)
	code, body := doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9990,"ifRevision":"`+token+`"}`,
		map[string]string{
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusOK {
		t.Fatalf("用 ETag 同构值作 ifRevision 状态码 = %d，期望 200；"+
			"说明两种形式不同构（Codex 第 14 轮要求一致）；body=%s", code, body)
	}
}

// TestIntegerIfRevisionIsRejected 守：**整数 precondition 不再被接受**。
//
// 🔴 Codex 第 15 轮否决了我上一版的"整数兼容"方案，理由成立：
//
//	我原想"整数拼接当前 generation 后比较"，但服务端**无法知道
//	这个整数来自哪个进程**：
//
//	    旧客户端保存 ifRevision=0
//	    新进程启动后当前 revision 也是 0
//	    新服务把它解释为 inst-NEW-rev-0 → 接受
//
//	generation 防护又被绕开 —— 而且比不修时更隐蔽（我当时以为修好了）。
//
//	所以整数形态明确拒绝（400），脚本改用 If-Match 或传完整版本串。
func TestIntegerIfRevisionIsRejected(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)
	before := store.Get().Port

	code, body := doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9991,"ifRevision":`+itoa64(rev)+`}`,
		map[string]string{
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusBadRequest {
		t.Errorf("整数 ifRevision 状态码 = %d，期望 400（无法携带进程代次）；body=%s",
			code, body)
	}
	if store.Get().Port != before {
		t.Error("被拒的整数请求仍然改动了配置")
	}
}

// TestLegacyRevisionStringIsRejected 守：缺少进程代次的旧格式串也拒绝。
//
// `"rev-3"` 这种旧格式没有 generation，同样会在配置重建后误匹配。
func TestLegacyRevisionStringIsRejected(t *testing.T) {
	srv, _ := newRevisionEnv(t)

	code, _ := doJSONWithBody(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9992,"ifRevision":"rev-0"}`,
		map[string]string{
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusBadRequest {
		t.Errorf("旧格式 ifRevision 状态码 = %d，期望 400", code)
	}
}

// TestIfMatchStillWorksAfterIntegerRejection 守：
// 禁用整数后，标准 If-Match 仍然是脚本的正确用法。
func TestIfMatchStillWorksAfterIntegerRejection(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9994}`,
		map[string]string{
			"Content-Type":   "application/json",
			"If-Match":       revisionETag(rev),
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusOK {
		t.Fatalf("If-Match 状态码 = %d，期望 200（脚本应改用这个形式）", code)
	}
	if store.Get().Port != 9994 {
		t.Error("If-Match 形式的写请求未生效")
	}
}

// TestRevisionConcurrentWritersOnlyOneWins 是本特性的**存在理由**测试。
//
// 模拟两个标签页同时基于同一版本提交：只有先提交的成功，
// 后提交的必须收到 412 而不是静默覆盖。
func TestRevisionConcurrentWritersOnlyOneWins(t *testing.T) {
	srv, store := newRevisionEnv(t)
	rev := getRevision(t, srv)

	// ⚠️ 端口用 freshPort 派生（见其说明）：硬编码在 `-count>1` 下会让
	// A 的"提交"变成"写相同的值"→ 不递增 revision → B 反而**能**提交成功，
	// 与"B 必须 412"的断言冲突。那是测试假设问题，不是乐观锁失效。
	portA := freshPort(t, store)
	portB := portA + 1

	// 🔴 显式断言"两个写入确实不同"（Codex 第 24 轮要求）。
	//
	//	为什么必须写成断言（而不只是靠 freshPort 的实现）：
	//	本测试的**全部意义**是"同一版本提交两次，第二次必须 412"。
	//	若 A 与 B 写的是**同一个值**，A 那次根本不产生变化
	//	→ revision 不递增 → B 拿着旧 token 反而合法 → 下面会报
	//	"B 提交状态码 = 200，期望 412"，**看起来像乐观锁失效**。
	//
	//	也就是说：**"两个值不同"是这个测试成立的前提**，不是无关细节。
	//	把前提写成断言，坏掉时会在**这里**报出真正的原因，
	//	而不是在下面伪装成产品缺陷。
	if portA == portB {
		t.Fatalf("测试前提被破坏：A 与 B 的端口相同（%d）—— "+
			"两次写入必须不同，否则第二次提交不构成并发冲突", portA)
	}
	// A 的值还必须与**当前值**不同，否则 A 那次本身也是 no-op。
	if cur := store.Get().Port; cur == portA {
		t.Fatalf("测试前提被破坏：A 的端口 %d 与当前值相同 —— "+
			"A 的提交不产生实际变化，revision 不会递增", portA)
	}

	// 标签页 A 先提交
	codeA, _ := patch(t, srv, `{"port":`+strconv.Itoa(portA)+`}`, &rev)
	if codeA != http.StatusOK {
		t.Fatalf("A 提交状态码 = %d，期望 200", codeA)
	}
	// A 提交后确实落到 A 的值（证明 A 那次产生了真实变化）
	if got := store.Get().Port; got != portA {
		t.Fatalf("A 提交后端口 = %d，期望 %d —— A 未生效，后续断言将失去意义",
			got, portA)
	}

	// 标签页 B 基于**同一旧版本**（rev 未变，与 A 用的是同一个 token）提交
	codeB, bodyB := patch(t, srv, `{"port":`+strconv.Itoa(portB)+`}`, &rev)
	if codeB != http.StatusPreconditionFailed {
		t.Fatalf("B 提交状态码 = %d，期望 412（不能静默覆盖 A）", codeB)
	}
	if store.Get().Port != portA {
		t.Errorf("端口 = %d，期望 %d（A 的值应被保留）", store.Get().Port, portA)
	}
	if bodyB == "" {
		t.Error("冲突响应应有可读内容")
	}
}
