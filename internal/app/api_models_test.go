package app

import (
	"context"
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
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// ─────────────────────────────────────────────────────────────
// 面板模型列表接口的护栏测试（2026-10-06）
//
// 🔴 这组测试守住的是一条**真实发生过的 bug**：
//
//	面板原本直接请求 `/v1/models` 渲染「模型列表」。那是对外接口，
//	被 requireAPIKey 保护 —— 用户一旦设了密钥，面板请求（只带
//	X-WBAPI-Panel，不带 Bearer）就被 401，页面显示「加载失败」。
//	而同一页的「模型数」取自不校验 key 的 `/status`，于是出现
//	"数得出来 31 个，却列不出来"的矛盾现象。
//
// 关键点：**修复方向是让面板改走同源 /api/models，而不是放开
// /v1/models 的鉴权**。所以下面同时断言两件事：
//  1. /api/models 不设 key 也能取到（面板可用）
//  2. /v1/models 设了 key 后**仍然** 401（对外契约没被破坏）
//
// 第 2 条尤其重要 —— 它是防止后来者为了"让面板能显示"而
// 顺手把 /v1/models 的鉴权去掉。那正是这个 bug 最容易的错误修法。
// ─────────────────────────────────────────────────────────────

// TestPanelModelsWorksWithoutAPIKey 验证面板接口在设了密钥后仍可取到模型。
//
// 这正是原 bug 的回归测试：若有人把面板改回直接请求 /v1/models，
// 或在 /api/models 上误加 requireAPIKey，本测试会失败。
func TestPanelModelsWorksWithoutAPIKey(t *testing.T) {
	srv := newPanelServer(t, nil)

	// 不带任何鉴权头请求面板接口 —— 面板的真实调用方式
	resp, err := http.Get(srv.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("面板 /api/models 状态码 = %d，期望 200"+
			"（面板不带密钥，本接口不应要求鉴权）", resp.StatusCode)
	}

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// Router 未装配时也必须是合法形状（object=list，data 为空数组而非 null）——
	// 面板按 d.data 渲染，null 会让前端渲染逻辑拿到 undefined。
	if got.Object != "list" {
		t.Errorf("object = %q，期望 \"list\"", got.Object)
	}
	if got.Data == nil {
		t.Error("data 不应为 null —— 面板按 d.data 渲染，需要空数组")
	}
}

// TestV1ModelsStillRequiresKey 守住对外接口的鉴权**没有**被顺手放开。
//
// 🔴 这条是本次修复的"反向护栏"：最省事的错误修法是把 /v1/models 的
// requireAPIKey 去掉，那样面板确实能显示了，但对外契约就被破坏了。
// 本测试断言：设了 key 之后，不带 Bearer 的 /v1/models 必须仍然 401。
func TestV1ModelsStillRequiresKey(t *testing.T) {
	srv := newServerWithKey(t, "secret-panel-test-key")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 刻意只带面板的 CSRF 令牌，不带 Authorization —— 模拟面板的请求
	req.Header.Set("X-WBAPI-Panel", "whatever")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/models 不带密钥状态码 = %d，期望 401 —— "+
			"对外接口的鉴权绝不能被放开（面板应改走 /api/models）", resp.StatusCode)
	}
}

// TestPanelModelsExcludesAliasesButV1KeepsThem 守住"别名只从面板列表里去掉"。
//
// 🔴 这是 2026-10-06 委托方要求（「映射的不需要出现在这里」）的护栏，
// 也是一条**双向**契约，两个方向都必须成立：
//
//	面板 /api/models  → **不含**别名（用户看模型目录，不该混入内部映射）
//	对外 /v1/models   → **仍含**别名（客户端可能先查列表再决定是否调用别名）
//
// ⚠️ 只测一半会留下严重隐患：
//   - 若只测"面板无别名"，有人可能顺手把 router.Models() 也改了，
//     于是 /v1/models 丢掉别名 ⇒ **客户端拒绝调用别名**（Codex 早前指出的坑）。
//   - 若只测"/v1 有别名"，有人可能让面板重新包含别名。
//
// 本测试需要**真的注册一个别名**才能验证 —— 否则两边都是空列表，测了个寂寞。
// 用 realias 注入：见下方 setupAliasRouter 的说明。
func TestPanelModelsExcludesAliasesButV1KeepsThem(t *testing.T) {
	srv, alias := newAliasEnv(t)

	// ── 面板接口：不得含别名 ──
	panelList := fetchModelIDs(t, srv.URL+"/api/models", "")
	for _, id := range panelList {
		if id == alias {
			t.Errorf("面板 /api/models 仍含别名 %q —— "+
				"委托方要求「映射的不需要出现在这里」", alias)
		}
	}

	// ── 对外接口：必须仍含别名 ──
	v1List := fetchModelIDs(t, srv.URL+"/v1/models", "Bearer alias-test-key")
	found := false
	for _, id := range v1List {
		if id == alias {
			found = true
		}
	}
	if !found {
		t.Errorf("/v1/models 缺少别名 %q —— 客户端会先查列表再决定是否调用，\n"+
			"别名不在列表里会导致客户端直接拒绝调用它（见 router.Models 注释）", alias)
	}

	// 面板必须比对外接口"少"别名那一条
	if len(panelList) != len(v1List)-1 {
		t.Errorf("面板 %d 条 vs /v1 %d 条 —— 期望相差正好 1 条（那个别名）",
			len(panelList), len(v1List))
	}
}

// TestPanelModelsReportsAliasCount 守：面板能拿到别名数量用于提示语。
//
// 为什么需要：面板列表比客户端少几条，用户会疑惑"是不是漏了"。
// 给出 aliasCount 后，面板可以说明"另有 N 个别名供客户端调用"。
func TestPanelModelsReportsAliasCount(t *testing.T) {
	srv, _ := newAliasEnv(t)

	body := fetchBody(t, srv.URL+"/api/models", "")
	var got struct {
		AliasCount int `json:"aliasCount"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, body)
	}
	if got.AliasCount != 1 {
		t.Errorf("aliasCount = %d，期望 1", got.AliasCount)
	}
}

// fetchModelIDs 取某个接口返回的模型 ID 列表。
func fetchModelIDs(t *testing.T, url, auth string) []string {
	t.Helper()
	body := fetchBody(t, url, auth)
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, body)
	}
	out := make([]string, 0, len(got.Data))
	for _, m := range got.Data {
		out = append(out, m.ID)
	}
	return out
}

// fetchBody 发 GET 并返回响应体（auth 非空时加 Bearer）。
func fetchBody(t *testing.T, url, auth string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d，期望 200", url, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestPanelAppJsUsesSameOriginModels 验证前端**确实**改走了同源接口。
//
// 为什么用"读下发脚本"这种方式测：
// 这是 go:embed 的资源，源码改了但没重编 exe 时（本项目踩过一次的坑）
// 二进制里仍是旧脚本 —— 本测试跑在源码上能通过，但**真实服务**未必。
// 所以它守的是"源码正确"，重编与否要靠外部验收（见交接文档 §3.2）。
func TestPanelAppJsUsesSameOriginModels(t *testing.T) {
	srv := newPanelServer(t, nil)

	resp, err := http.Get(srv.URL + "/panel/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	js := string(body)

	if !strings.Contains(js, "/api/models") {
		t.Error("面板 app.js 未使用同源 /api/models —— 模型列表会因鉴权而加载失败")
	}
	// 不能再用 /v1/models 拉列表（那是需要鉴权的对外接口）
	if strings.Contains(js, "fetchJSON('/v1/models')") {
		t.Error("面板 app.js 仍在直接请求 /v1/models —— " +
			"设了密钥的用户会看到「加载失败」")
	}
}

// newServerWithKey 起一个"已设密钥"的服务，用于测鉴权行为。
//
// 直接复用 settings_test.go 的 newAuthEnv：它已经组装好
// Settings（带 key）+ Router，否则 requireAPIKey 会因 key 为空而放行，
// 测不出 401 行为（委托人要求"key 可为空"，空 key 时 /v1/* 不校验）。
func newServerWithKey(t *testing.T, key string) *httptest.Server {
	t.Helper()
	return newAuthEnv(t, key)
}

// testAlias 是本组测试注册的别名。
const testAlias = "dsf"

// newAliasEnv 起一个"已注册真实模型 + 一个别名"的服务。
//
// 为什么不能用空 Router：空目录下 /api/models 与 /v1/models 都是空列表，
// "别名被过滤掉了"与"压根没有别名"无法区分 —— 那样的测试是假绿的。
// 所以这里用 testutil 的假上游注册真实模型，再挂一个别名。
func newAliasEnv(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")

	r := router.New()
	if err := r.Register(context.Background(),
		newWorkbuddyProviderForTest(newWorkbuddyClientForTest(t, fake.URL))); err != nil {
		t.Fatalf("注册模型失败: %v", err)
	}

	// 选一个已注册的真实模型作为别名目标
	models := r.ModelsWithoutAliases()
	if len(models) == 0 {
		t.Fatal("假上游没有提供任何模型，无法建立别名")
	}
	target := models[0].ID
	if err := r.SetAliases(map[string]string{testAlias: target}); err != nil {
		t.Fatalf("设置别名失败: %v", err)
	}

	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Mutate(func(s *config.Settings) error {
		s.APIKey = "alias-test-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(newMux(Deps{
		Logger:   log.New(io.Discard, "", 0),
		Settings: store,
		Router:   r,
	}))
	t.Cleanup(srv.Close)
	return srv, testAlias
}
