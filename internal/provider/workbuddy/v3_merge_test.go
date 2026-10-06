package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// newIntlBothSources 构造"两个端点都有"的客户端（国际版合并场景）。
func newIntlBothSources(t *testing.T) (*Client, *testutil.FakeUpstream) {
	t.Helper()
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing-intl.json")
	fake.WithJSONForPath("/v3/config", "v3-config-intl.json")
	return c, fake
}

// TestV3OverridesV2ForOverlappingModel 守：重叠模型以 **/v3 为准**。
//
// 🔴 这是本改动的核心语义，也是委托方拍板的点。
//
//	实测背景：hy4-preview 的免费活动 hy4-free-trial-202608 已于
//	2026-09-08 过期，但 /v2 的 credits 里**把活动期的 0.00 写死了**，
//	于是面板一直显示"免费" —— 而实测调用它**实收 2.13 credits**。
//	/v3 给的是真实基础倍率 0.29。
//
//	⇒ 若哪天有人把合并顺序反过来（/v2 优先），这条会立刻红。
func TestV3OverridesV2ForOverlappingModel(t *testing.T) {
	c, _ := newIntlBothSources(t)

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}

	var hy4 *provider.Model
	for i := range models {
		if models[i].UpstreamID == "hy4-preview" {
			hy4 = &models[i]
		}
	}
	if hy4 == nil {
		t.Fatal("缺少 hy4-preview")
	}
	if hy4.Pricing == nil || !hy4.Pricing.HasMultiplier {
		t.Fatal("hy4-preview 应有倍率")
	}
	if hy4.Pricing.Multiplier != 0.29 {
		t.Errorf("hy4-preview 倍率 = %v，期望 0.29（/v3 的真实基础倍率）—— "+
			"若为 0 说明用了 /v2 那份把过期活动写死的值，会让用户以为免费却实际扣费",
			hy4.Pricing.Multiplier)
	}
}

// TestV3KeepsV2OnlyModel 守：**/v2 独有的模型不能丢**。
//
//	gpt-5.3-codex 只在 /v2 的目录里（/v3 的 22 条中没有它）。
//	合并若写成"用 /v3 覆盖 /v2"，它就会消失。
func TestV3KeepsV2OnlyModel(t *testing.T) {
	c, _ := newIntlBothSources(t)

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	found := false
	for _, m := range models {
		if m.UpstreamID == "gpt-5.3-codex" {
			found = true
		}
	}
	if !found {
		t.Error("gpt-5.3-codex 消失了 —— 它只存在于 /v2，合并必须保留两边的并集")
	}
}

// TestV3ModelsAreNotTrimmedByAgents 守：**/v3 绝不套用 agents 裁剪**。
//
// 🔴 依据（实测）：/v3/config 有 22 条模型，但它的 agents.cli 只列 21 条
// ——**漏掉 deepseek-v4.1-flash-sg**。若给它套上 agents 裁剪，
// 该模型会被静默砍掉（它是委托方要的 5 个之一）。
//
// 样本 v3-config-intl.json 刻意复刻了这个缺口（agents 里不含 sg 版），
// 所以这条测试真的会抓到"顺手补个 allowed 过滤"的改动。
func TestV3ModelsAreNotTrimmedByAgents(t *testing.T) {
	c, _ := newIntlBothSources(t)

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	for _, m := range models {
		if m.UpstreamID == "deepseek-v4.1-flash-sg" {
			return // 在，符合预期
		}
	}
	t.Error("deepseek-v4.1-flash-sg 不见了 —— /v3 的模型不得套用 agents 裁剪" +
		"（它的 agents.cli 实测漏掉这个 ID）")
}

// TestV3FailureFallsBackToStaticTable 守：/v3 不可用时**退回静态表**。
//
// 委托方明确要求保留静态表作为兜底：/v3 挂了不该让那 5 个模型消失。
func TestV3FailureFallsBackToStaticTable(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing-intl.json")
	// 让 /v3/config 返回 500
	fake.SetStatusForPath("/v3/config", http.StatusInternalServerError)

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取失败（/v3 挂了不应让整体失败）: %v", err)
	}

	byID := map[string]provider.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	// 5 个都应在（由静态表兜底）
	want := []string{
		"workbuddyai/deepseek-v4.1-flash",
		"workbuddyai/deepseek-v4.1-flash-sg",
		"workbuddyai/glm-5.3-flash",
		"workbuddyai/kimi-k2.8-preview",
		"workbuddyai/gpt-6-astra",
	}
	for _, id := range want {
		if _, ok := byID[id]; !ok {
			t.Errorf("/v3 失败时应由静态表兜底，但缺少 %s", id)
		}
	}
	// 兜底值来自静态表（gpt-6-astra 6.67）
	if p := byID["workbuddyai/gpt-6-astra"].Pricing; p == nil || p.Multiplier != 6.67 {
		t.Errorf("兜底倍率不对: %+v", p)
	}
}

// TestStaticTableNotUsedWhenV3Works 守：/v3 正常时**不启用静态表**。
//
// 🔴 为什么重要：静态表是"上游没给"时的猜测性兜底。
//
//	若 /v3 正常还去补，会把上游**已下架**的模型重新复活
//	（用户选到调不通的模型 —— 正是本项目一直在防的坏体验）。
//
// 做法：样本里 /v3 给 glm-5.3-flash = 0.06；把静态表里该条改成 0.99，
// 断言最终结果是 0.06（说明走的是 /v3，不是静态表）。
func TestStaticTableNotUsedWhenV3Works(t *testing.T) {
	c, _ := newIntlBothSources(t)

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	for _, m := range models {
		if m.UpstreamID == "glm-5.3-flash" {
			if m.Pricing == nil || m.Pricing.Multiplier != 0.06 {
				t.Errorf("glm-5.3-flash 倍率 = %+v，期望 0.06（/v3 的值）", m.Pricing)
			}
			return
		}
	}
	t.Error("缺少 glm-5.3-flash")
}

// TestV3EmptyModelsTreatedAsFailure 守：/v3 返回 0 个模型按**失败**处理。
//
// 理由：若上游改结构导致解析成空，静默接受会让 5 个补充模型无声消失
// （而且不会走静态兜底）。所以空响应必须走兜底路径。
func TestV3EmptyModelsTreatedAsFailure(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing-intl.json")
	fake.WithJSONBodyForPath("/v3/config", []byte(`{"code":0,"msg":"ok","data":{"models":[]}}`))

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("不应失败: %v", err)
	}
	byID := map[string]provider.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if _, ok := byID["workbuddyai/gpt-6-astra"]; !ok {
		t.Error("/v3 返回空时应按失败处理并走静态兜底，但 gpt-6-astra 不在结果里")
	}
}

// TestV3NotFetchedForCN 守：**国内版不拉 /v3/config**。
//
// 理由：国内版的 17 个模型是委托方明确冻结的决策（老模型不要）。
// 若给国内版也拉 /v3，可能凭空多出模型 —— 那就越过了已冻结的产品决策。
// 同时也能验证"少发一个请求"（省一次上游调用）。
func TestV3NotFetchedForCN(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing.json")
	fake.WithJSONForPath("/v3/config", "v3-config-intl.json")

	if _, err := c.Models(context.Background(), testCred(), PlatformCN); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}

	for _, r := range fake.Requests() {
		if strings.Contains(r.Path, "/v3/config") {
			t.Error("国内版不应请求 /v3/config（国际版专属端点）")
		}
	}
}
