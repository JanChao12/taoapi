package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
)

// 本文件守住设置 API 与 /v1/* 鉴权的契约（docs/第9轮-接口契约-冻结.md §2、§3）。
//
// 🔴 最重要的一条是 TestSettingsGetNeverLeaksKey：
// apiKey 是本地 bearer 凭据，任何接口响应都不得包含明文。

// newSettingsEnv 起一个带设置存储的服务。
//
// 用 t.TempDir() 隔离磁盘：设置文件会被真实读写，
// 绝不能让测试污染用户的 ~/.wbapi/config.json。
func newSettingsEnv(t *testing.T) (*httptest.Server, *config.Store) {
	t.Helper()

	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatalf("加载测试设置失败: %v", err)
	}

	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Settings: store,
		Router:   router.New(),
	}
	srv := httptest.NewServer(newMux(deps))
	t.Cleanup(srv.Close)
	return srv, store
}

// doJSON 发一个 JSON 请求并返回状态码与响应体。
//
// 🔴 对 PATCH /api/settings 会自动补上 ifRevision（乐观锁，Codex 第 12 轮）：
// 这些测试关心的是鉴权/校验/写盘本身，不该每处都手动取一遍 revision。
// 专门测乐观锁行为的用例在 revision_test.go 里，那里显式控制该字段。
func doJSON(t *testing.T, method, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	body = autoIfRevision(t, method, url, body)
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
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

// autoIfRevision 给 PATCH /api/settings 的请求体补 ifRevision。
//
// 只在"确实是该接口的 PATCH"且"恰好指向自身"时补：目标是
// httptest 起的服务，URL 里带 /api/settings 即认。
// 体不是 JSON 对象、或已经有 ifRevision 时原样返回。
func autoIfRevision(t *testing.T, method, url, body string) string {
	t.Helper()
	if method != http.MethodPatch || !strings.Contains(url, "/api/settings") {
		return body
	}
	var m map[string]any
	if body == "" {
		m = map[string]any{}
	} else if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body // 非 JSON 对象（例如专门测非法 JSON 的用例）原样传递
	}
	if _, has := m["ifRevision"]; has {
		return body
	}
	// 从同一服务的 GET 取当前 revision
	base := url[:strings.Index(url, "/api/settings")]
	resp, err := http.Get(base + "/api/settings")
	if err != nil {
		return body
	}
	defer resp.Body.Close()
	var v settingsView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return body
	}
	m["ifRevision"] = revisionETagToken(v.Revision)
	raw, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(raw)
}

// TestSettingsGetReturnsConfiguredAPIKey 是 2026-10-06 契约变更后的护栏。
//
// 🔴 本测试取代了原来的 TestSettingsGetNeverLeaksKey。
//
//	原因是**委托方明确改变了契约**（原话「密钥不需要隐藏，直接显式展出出来，
//	方便别人复制」）。契约改了，断言就必须跟着改 ——
//	不能一边让接口返回明文、一边留着"绝不含明文"的断言（那是自相矛盾）。
//	但**反向护栏不能少**：见本文件末尾的
//	TestNonSettingsResponsesNeverContainAPIKey。
//
// 断言三件事：
//  1. 完整 key 确实返回（新契约成立）；
//  2. hint 仍正确（面板顶部一览还在用它）；
//  3. ⚠️ 返回的是**本地 API key**，不是上游 token —— 红线未被放宽。
func TestSettingsGetReturnsConfiguredAPIKey(t *testing.T) {
	srv, store := newSettingsEnv(t)

	secret := "super-secret-key-abcdef"
	if err := store.Mutate(func(s *config.Settings) error {
		s.APIKey = secret
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	code, body := doJSON(t, http.MethodGet, srv.URL+"/api/settings", "", nil)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", code)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}

	// 1) 明文返回（新契约）
	if got["apiKey"] != secret {
		t.Errorf("apiKey = %v，期望完整密钥 %q", got["apiKey"], secret)
	}
	// 2) hint 仍正确
	if got["apiKeyHint"] != "supe…" {
		t.Errorf("apiKeyHint = %v，期望 \"supe…\"", got["apiKeyHint"])
	}
	if got["apiKeySet"] != true {
		t.Error("apiKeySet 应为 true")
	}
	// 3) 红线未被放宽：响应里不能出现**上游 token** 类字段
	for _, forbidden := range []string{"accessToken", "refreshToken", "access_token", "refresh_token"} {
		if _, exists := got[forbidden]; exists {
			t.Errorf("响应出现了上游凭据字段 %q —— "+
				"本地 apiKey 的例外【不】适用于上游 token", forbidden)
		}
	}
}

// TestSettingsGetWhenKeyEmpty 验证未设 key 时的响应（委托人要求 key 可为空）。
func TestSettingsGetWhenKeyEmpty(t *testing.T) {
	srv, store := newSettingsEnv(t)
	if err := store.Mutate(func(s *config.Settings) error {
		s.APIKey = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, body := doJSON(t, http.MethodGet, srv.URL+"/api/settings", "", nil)
	var got map[string]any
	_ = json.Unmarshal([]byte(body), &got)
	if got["apiKeySet"] != false {
		t.Error("key 为空时 apiKeySet 应为 false")
	}
	if got["apiKeyHint"] != "" {
		t.Errorf("key 为空时不应有 hint，实际 %v", got["apiKeyHint"])
	}
}

// TestSettingsPatchSetAndClearKey 验证"设置"与"明确清空"两种语义。
//
// 🔴 关键：契约要求用指针字段区分"未修改"与"改成零值"。
// 若用值类型，前端不传 apiKey 会被误解成"要清空"。
func TestSettingsPatchSetAndClearKey(t *testing.T) {
	srv, store := newSettingsEnv(t)

	// 设置新 key
	code, body := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"apiKey":"newkey123"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("设置 key 状态码 = %d，body=%s", code, body)
	}
	if got := store.Get().APIKey; got != "newkey123" {
		t.Fatalf("key 未写入，实际 %q", got)
	}

	// 只改端口，key 必须保持不变（"缺失=未修改"）
	if code, body := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9123}`, nil); code != http.StatusOK {
		t.Fatalf("改端口状态码 = %d，body=%s", code, body)
	}
	if got := store.Get().APIKey; got != "newkey123" {
		t.Errorf("只改端口时 key 被意外改动: %q（缺失字段应视为未修改）", got)
	}
	if got := store.Get().Port; got != 9123 {
		t.Errorf("端口未写入，实际 %d", got)
	}

	// 明确清空
	if code, body := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"apiKeyClear":true}`, nil); code != http.StatusOK {
		t.Fatalf("清空 key 状态码 = %d，body=%s", code, body)
	}
	if got := store.Get().APIKey; got != "" {
		t.Errorf("key 应为空，实际 %q", got)
	}
}

// TestSettingsPatchRejectsConflictingKeyChange 守：同时设值与清空 → 400。
//
// 猜用户意图不如明确报错。
func TestSettingsPatchRejectsConflictingKeyChange(t *testing.T) {
	srv, _ := newSettingsEnv(t)

	code, _ := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"apiKey":"x","apiKeyClear":true}`, nil)
	if code != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400（语义冲突）", code)
	}
}

// TestSettingsPatchRejectsBadPort 守端口校验。
func TestSettingsPatchRejectsBadPort(t *testing.T) {
	srv, store := newSettingsEnv(t)
	before := store.Get().Port

	for _, p := range []string{"0", "80", "1023", "65536", "-1"} {
		code, body := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
			`{"port":`+p+`}`, nil)
		if code != http.StatusBadRequest {
			t.Errorf("端口 %s 状态码 = %d，期望 400（body=%s）", p, code, body)
		}
	}
	if got := store.Get().Port; got != before {
		t.Errorf("被拒的端口不应写入，实际 %d", got)
	}

	// 合法端口应接受
	if code, body := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":1024}`, nil); code != http.StatusOK {
		t.Errorf("端口 1024 应被接受，状态码 = %d body=%s", code, body)
	}
}

// TestSettingsPatchAliasesValidated 守别名在 API 层被校验。
func TestSettingsPatchAliasesValidated(t *testing.T) {
	srv, store := newSettingsEnv(t)

	// 目标模型不存在 → 必须拒绝（router 已有模型目录，但这里为空）
	code, _ := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"aliases":{"dsf":"workbuddy/not-exist"}}`, nil)
	if code == http.StatusOK {
		t.Error("指向不存在模型的别名应被拒绝")
	}
	if len(store.Get().Aliases) != 0 {
		t.Error("被拒的别名不应写入配置")
	}
}

// TestSettingsPatchAutoCheckinToggles 验证开关能改并驱动回调。
func TestSettingsPatchAutoCheckinToggles(t *testing.T) {
	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	var calls []bool
	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Settings: store,
		Router:   router.New(),
	}
	deps.applyAutoCheckin = func(enabled bool) { calls = append(calls, enabled) }

	srv := httptest.NewServer(newMux(deps))
	defer srv.Close()

	if code, body := doJSON(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"autoCheckin":true}`, nil); code != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", code, body)
	}
	if !store.Get().AutoCheckin {
		t.Error("配置未更新")
	}
	if len(calls) != 1 || calls[0] != true {
		t.Errorf("applyAutoCheckin 未被正确调用: %v（开关要立即生效，不是等重启）", calls)
	}
}

// TestSettingsMethodNotAllowed 守 HTTP 方法限制。
func TestSettingsMethodNotAllowed(t *testing.T) {
	srv, _ := newSettingsEnv(t)
	if code, _ := doJSON(t, http.MethodDelete, srv.URL+"/api/settings", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE 状态码 = %d，期望 405", code)
	}
	if code, _ := doJSON(t, http.MethodPut, srv.URL+"/api/settings/restart", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT restart 状态码 = %d，期望 405", code)
	}
}

// ─────────────────────────────────────────────────────────────
// /v1/* 鉴权
// ─────────────────────────────────────────────────────────────

// newAuthEnv 起一个带 key 的服务，并注册一个可用模型以便 /v1/models 返回内容。
func newAuthEnv(t *testing.T, apiKey string) *httptest.Server {
	t.Helper()
	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Mutate(func(s *config.Settings) error {
		s.APIKey = apiKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Settings: store,
		Router:   router.New(),
	}
	srv := httptest.NewServer(newMux(deps))
	t.Cleanup(srv.Close)
	return srv
}

// TestV1RequiresKeyWhenSet 守：设了 key 就必须带对。
func TestV1RequiresKeyWhenSet(t *testing.T) {
	srv := newAuthEnv(t, "secret")

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"不带 Authorization", "", http.StatusUnauthorized},
		{"错误 key", "Bearer wrong", http.StatusUnauthorized},
		{"不是 Bearer", "Basic secret", http.StatusUnauthorized},
		{"只有 Bearer 无值", "Bearer ", http.StatusUnauthorized},
		{"正确 key", "Bearer secret", http.StatusOK},
		{"bearer 小写也应接受", "bearer secret", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := map[string]string{}
			if c.header != "" {
				h["Authorization"] = c.header
			}
			code, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/models", "", h)
			if code != c.want {
				t.Errorf("状态码 = %d，期望 %d", code, c.want)
			}
		})
	}
}

// TestV1AllowsWhenKeyEmpty 守：key 为空时放行（委托人明确要求"key 可以为空"）。
func TestV1AllowsWhenKeyEmpty(t *testing.T) {
	srv := newAuthEnv(t, "")

	code, _ := doJSON(t, http.MethodGet, srv.URL+"/v1/models", "", nil)
	if code != http.StatusOK {
		t.Errorf("key 为空时应放行，状态码 = %d", code)
	}
}

// TestV1AuthAppliesToChat 守：鉴权对 chat 接口同样生效。
//
// 只保护 /v1/models 而漏掉 chat 是最容易犯的错 —— 那等于没保护。
func TestV1AuthAppliesToChat(t *testing.T) {
	srv := newAuthEnv(t, "secret")

	body := `{"model":"workbuddy/x","messages":[{"role":"user","content":"hi"}]}`
	code, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", body, nil)
	if code != http.StatusUnauthorized {
		t.Errorf("chat 未带 key 状态码 = %d，期望 401", code)
	}

	// 带对 key 时应通过鉴权（后续会因模型不存在等报别的错，但不应是 401）
	code, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer secret"})
	if code == http.StatusUnauthorized {
		t.Error("带正确 key 仍被 401")
	}
}

// TestSettingsEndpointsNotKeyProtected 守：面板与管理接口不需要 key。
//
// 理由（契约 §2）：只监听回环，且面板要能在未设 key 时打开。
// 若这里红了，说明有人"顺手"给面板也加了鉴权 —— 那会让用户
// 在设了 key 之后反而打不开设置页（鸡生蛋问题）。
func TestSettingsEndpointsNotKeyProtected(t *testing.T) {
	srv := newAuthEnv(t, "secret")

	for _, p := range []string{"/api/settings", "/panel/", "/status", "/healthz"} {
		code, _ := doJSON(t, http.MethodGet, srv.URL+p, "", nil)
		if code == http.StatusUnauthorized {
			t.Errorf("%s 被要求鉴权（期望不需要）：面板/状态接口只监听回环，不应鉴权", p)
		}
	}
}

// TestNonSettingsResponsesNeverContainAPIKey 是明文回显的**反向护栏**。
//
// 🔴 这条测试的存在理由（Codex 第 36 轮 T2 明确要求）：
//
//	委托方同意让 /api/settings 明文返回本地 key，但契约的准确表述是：
//	**「除 /api/settings 外，任何 API 响应都不得包含本地 key。」**
//	没有这条反向护栏，"明文返回"很容易在后续改动里扩散到
//	/status、/api/accounts、错误响应等地方 —— 那才是真正的泄露扩大。
//
// Codex 特别强调：要覆盖**成功和错误**响应，
// 「不要只检查几个正常路径」。
func TestNonSettingsResponsesNeverContainAPIKey(t *testing.T) {
	// 用高熵值，避免偶然的子串匹配
	const secret = "a7f3c9e1b5d24680f1e3c5a7b9d02468"
	srv := newAuthEnv(t, secret)

	paths := []struct {
		method string
		path   string
		why    string
	}{
		{http.MethodGet, "/status", "运行状态（面板与外部都可读）"},
		{http.MethodGet, "/api/accounts", "账号列表"},
		{http.MethodGet, "/api/stats?days=1", "用量统计"},
		{http.MethodGet, "/api/models", "面板模型列表"},
		{http.MethodGet, "/healthz", "健康检查"},
		{http.MethodGet, "/api/settings", "⚠️ 这是唯一例外，单独排除"},
		// ── 错误路径（Codex 要求必须覆盖）──
		{http.MethodGet, "/api/accounts/nonexistent", "账号查找失败"},
		{http.MethodGet, "/v1/models", "未鉴权（401）响应"},
		{http.MethodGet, "/api/stats?days=abc", "参数错误响应"},
		{http.MethodGet, "/no-such-endpoint", "404 响应"},
		{http.MethodPost, "/api/accounts/action", "非法动作的错误响应"},
	}

	for _, tc := range paths {
		if tc.path == "/api/settings" {
			continue // 唯一允许返回明文的接口
		}
		code, body := doJSON(t, tc.method, srv.URL+tc.path, "", nil)
		if strings.Contains(body, secret) {
			t.Errorf("%s %s 的响应（HTTP %d）含明文 key —— %s\n"+
				"契约：除 /api/settings 外，任何 API 响应都不得包含本地 key",
				tc.method, tc.path, code, tc.why)
		}
	}
}

// TestSettingsResponseNoStore 守：含明文的响应不得被 HTTP 缓存留存。
//
// Codex 第 36 轮 T4：「GET 响应可能被浏览器缓存，尤其是服务端未设置
// 缓存头时。」这条不能阻止开发者工具/扩展/截图，但能避免
// **普通 HTTP 缓存成为额外副本**。
func TestSettingsResponseNoStore(t *testing.T) {
	srv, _ := newSettingsEnv(t)

	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q，期望含 no-store —— "+
			"明文密钥不应被浏览器/中间层缓存", cc)
	}
}
