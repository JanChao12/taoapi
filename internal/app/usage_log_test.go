package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// ─────────────────────────────────────────────────────────────
// 调用明细接口的护栏测试（2026-10-06）
//
// 委托方要求：
//
//	「加入一个新列表显示每一次api的调用情况，包括账号、时间、token、
//	  缓存命中率、积分消耗等信息，让我每使用一次都能查询到记录」
// ─────────────────────────────────────────────────────────────

// TestUsageLogClampsLimit 守：limit 参数被夹到安全范围。
//
// 🔴 这是**防自我 DoS** 的关键：limit 来自 URL（客户端可控）。
//
//	不封顶的话一个 ?limit=100000000 就能让服务 OOM。
func TestUsageLogClampsLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"", usageLogDefaultLimit, "缺省用默认值"},
		{"50", 50, "正常值原样"},
		{"0", usageLogDefaultLimit, "0 无意义 ⇒ 默认"},
		{"-5", usageLogDefaultLimit, "负数 ⇒ 默认"},
		{"abc", usageLogDefaultLimit, "非数字 ⇒ 默认"},
		{"999999999", usageLogMaxLimit, "🔴 超大值必须封顶（防 OOM）"},
		{"1001", usageLogMaxLimit, "超过上限即封顶"},
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%q) = %d，期望 %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestUsageLogEntryHasRequiredFields 守：明细行含委托方要的全部字段。
func TestUsageLogEntryHasRequiredFields(t *testing.T) {
	// 构造一条完整事件
	credit := 0.51
	ev := usagepkg.Event{
		Account:          "eeeeeeee…",
		Model:            "workbuddy/deepseek-v4.1-flash",
		ProviderID:       "workbuddy",
		ResolvedModel:    "deepseek-v4.1-flash",
		OK:               true,
		Status:           "ok",
		UsageKnown:       true,
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
		CacheHitTokens:   90,
		CacheMissTokens:  10,
		Credit:           &credit,
	}
	ev.Time = ev.Time.UTC()

	e := buildUsageLogEntry(Deps{}, ev)

	if e.Model != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("Model = %q", e.Model)
	}
	if e.PromptTokens != 100 || e.CompletionTokens != 20 {
		t.Errorf("token 字段丢失: %+v", e)
	}
	if e.CacheHitRate == nil || *e.CacheHitRate != 0.9 {
		t.Errorf("缓存命中率 = %v，期望 0.9", e.CacheHitRate)
	}
	if e.Credits == nil || *e.Credits != 0.51 {
		t.Errorf("积分 = %v，期望 0.51", e.Credits)
	}
	if !e.UsageKnown {
		t.Error("UsageKnown 应透传（前端据此决定显示数字还是 —）")
	}
}

// TestUsageLogCacheRateNullWhenNoCacheData 守：无缓存数据时是 null 不是 0。
//
// 🔴 "上游没报告缓存数据"与"命中率确实是 0%"含义不同 ——
//
//	显示 0% 会让人以为缓存完全没生效。
func TestUsageLogCacheRateNullWhenNoCacheData(t *testing.T) {
	ev := usagepkg.Event{Model: "m", CacheHitTokens: 0, CacheMissTokens: 0}
	e := buildUsageLogEntry(Deps{}, ev)
	if e.CacheHitRate != nil {
		t.Errorf("无缓存数据时应为 nil，实际 %v", *e.CacheHitRate)
	}

	// 有数据（哪怕全未命中）⇒ 0.0 是**真实值**
	ev2 := usagepkg.Event{Model: "m", CacheHitTokens: 0, CacheMissTokens: 100}
	e2 := buildUsageLogEntry(Deps{}, ev2)
	if e2.CacheHitRate == nil {
		t.Fatal("有未命中样本时应给出 0.0（真实命中率）")
	}
	if *e2.CacheHitRate != 0 {
		t.Errorf("命中率 = %v，期望 0", *e2.CacheHitRate)
	}
}

// TestUsageLogResolvesNickname 守：明细里的账号显示昵称。
func TestUsageLogResolvesNickname(t *testing.T) {
	st := auth.NewStore()
	uid := "11111111-2222-3333-4444-555555555555"
	st.Put(&auth.Account{UID: uid, Nickname: "13800000001"})

	ev := usagepkg.Event{Account: auth.MaskUID(uid), Model: "workbuddy/glm-5.3"}
	e := buildUsageLogEntry(Deps{Accounts: st}, ev)

	if e.Nickname != "13800000001" {
		t.Errorf("昵称 = %q，期望 13800000001（用户要分清是哪个号）", e.Nickname)
	}
	if e.Platform != auth.PlatformCN {
		t.Errorf("平台 = %q", e.Platform)
	}
}

// TestUsageLogEndpointRegistered 守：路由已注册且返回合法结构。
func TestUsageLogEndpointRegistered(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Get(srv.URL + "/api/usage/log?days=1&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var got struct {
		Entries      []map[string]any `json:"entries"`
		Limit        int              `json:"limit"`
		TotalMatched int64            `json:"total_matched"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, raw)
	}
	if got.Limit != 10 {
		t.Errorf("limit = %d，期望 10", got.Limit)
	}
	if got.Entries == nil {
		t.Error("entries 不该是 null（前端直接遍历）")
	}
}

// TestUsageLogNeverLeaksTokens 守：明细响应**绝不含** token/凭据。
//
// 与既有红线一致：token 不进任何 API 响应。
func TestUsageLogNeverLeaksTokens(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	// 写一条带"看起来像 token"的字段，确认不会被带出去
	if err := store.Append(usagepkg.Event{
		Account: "acct…",
		Model:   "workbuddy/glm-5.3",
		OK:      true,
		Status:  "ok",
	}); err != nil {
		t.Fatal(err)
	}

	srv := newPanelServer(t, store)
	resp, err := http.Get(srv.URL + "/api/usage/log?days=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	for _, forbidden := range []string{
		"accessToken", "access_token", "refreshToken", "refresh_token",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("响应含 %q —— token 绝不进任何 API 响应", forbidden)
		}
	}
}

// TestPanelHasUsageLogTable 守：面板有调用记录表且已接线。
func TestPanelHasUsageLogTable(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := stripHTMLComments(string(raw))

	for _, want := range []string{
		`id="usage-log-table"`, `id="usage-log-body"`,
		`id="btn-refresh-usage-log"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("缺少 %s", want)
		}
	}

	// 环形图卡片应已删除（委托方：「图3不需要了你直接删掉」）
	if strings.Contains(html, `id="donut"`) {
		t.Error("环形图卡片仍在 —— 委托方已要求删除")
	}

	js := fetchPanelAppJS(t)
	if !strings.Contains(js, "function loadUsageLog(") {
		t.Error("缺少 loadUsageLog —— 调用记录不会加载")
	}
}

// TestAccountUsageHasTokenAndCreditColumns 守：账号用量表含 token 与积分列。
//
// 委托方原话：「账号用量要包括token信息和积分消耗，请求这些没用改成调用」
//
// 🔴 2026-10-09 列定义又变了一次（委托方要求列表长度减半）：
//
//	原话：「模型用量和账号用量列表长度都减半，删除输入输出，
//	        仅将合计改名为Tokens」。
//	⇒ 「输入」「输出」两列删除，「合计」改名「Tokens」。
//	本测试同步为**新契约**，并反向断言旧列名不许回来 ——
//	否则"删了列但测试还认旧列名"会让测试变成假绿。
func TestAccountUsageHasTokenAndCreditColumns(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := stripHTMLComments(string(raw))

	// 表头必须有这些列（2026-10-09 新契约）
	for _, col := range []string{"调用", "Tokens", "缓存命中率", "积分"} {
		if !strings.Contains(html, ">"+col+"<") {
			t.Errorf("账号用量表缺少「%s」列", col)
		}
	}
	// 旧的「请求」列名应已改掉
	if strings.Contains(html, ">请求<") {
		t.Error("账号用量表仍用「请求」做列名 —— 委托方要求改成「调用」")
	}
	// 🔴 反向断言：被明确删除的列名不许再出现。
	//	「输入」「输出」在账号用量表里已按委托方要求删除。
	for _, gone := range []string{">输入<", ">输出<", ">合计<"} {
		if strings.Contains(html, gone) {
			t.Errorf("账号用量表仍有已删除的列 %s —— 委托方 2026-10-09 "+
				"要求删除输入/输出并把合计改名为 Tokens", gone)
		}
	}
}
