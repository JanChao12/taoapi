package workbuddy

import (
	"context"
	"net/http"
	"os"
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

// TestV3FailureLosesSupplementalModelsHonestly 守：/v3 不可用时
// **如实少列**那 5 个目录外模型（不再用静态表兜底）。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 本条测试替代了原来的 TestV3FailureFallsBackToStaticTable
// ═══════════════════════════════════════════════════════════════════
//
//	原测试断言"静态表把 5 个模型补回来"。委托方 2026-10-09 要求
//	删除静态表（原话见 supplement.go 的包注释），理由是那张表是
//	一次性抄录的倍率快照，**只在上游拉取失败时启用** ——
//	而那个时刻恰恰最可能是"上游改了定价/下架了模型"，
//	它会用过期数据冒充权威值。
//
//	⇒ 新语义：/v3 挂了就是少列，**不补假数据**。
//	  本测试同时守住"整体不失败"（/v2 的数据仍要正常返回）——
//	  删兜底不等于允许一挂就全空。
func TestV3FailureLosesSupplementalModelsHonestly(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing-intl.json")
	// 让 /v3/config 返回 500
	fake.SetStatusForPath("/v3/config", http.StatusInternalServerError)

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取失败（/v3 挂了不应让整体失败）: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("/v3 挂了就一个模型都不给 —— 应仍返回 /v2 的数据，" +
			"不能因为删了静态兜底就整体失败")
	}

	byID := map[string]provider.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}

	// 🔴 反向断言：这 5 个目录外模型**不许**出现。
	//	它们只能来自上游 /v3；/v3 挂了就没有可信来源，
	//	此时出现任何一个都说明静态表被"好心"加回来了。
	for _, gone := range []string{
		"workbuddyai/gpt-6-astra",
		"workbuddyai/kimi-k2.8-preview",
		"workbuddyai/glm-5.3-flash",
		"workbuddyai/deepseek-v4.1-flash-sg",
	} {
		if _, ok := byID[gone]; ok {
			t.Errorf("/v3 失败时出现了 %s —— 说明静态兜底表被恢复了。\n"+
				"  委托方 2026-10-09 明确要求删除该表：它是过期快照，"+
				"在上游拉取失败时冒充权威数据（详见 supplement.go 包注释）", gone)
		}
	}
}

// TestStaticTableIsGone 守：静态兜底表**确实已被删除**，不许复活。
//
// 🔴 这是结构性断言（审源码），不是行为断言 —— 因为"兜底"这种行为
//
//	只在 /v3 失败时才生效，而失败路径的测试很容易被后来者顺手改掉。
//	直接盯住"那张表的数据是否又出现在源码里"更可靠。
//
// 反向对照：把任何一条旧的静态条目（如 gpt-6-astra 的 6.67）
// 写回 supplement.go，本条立刻红。
func TestStaticTableIsGone(t *testing.T) {
	// 读取生产源码（不是测试文件）—— 只看 supplement.go 与 models.go。
	for _, f := range []string{"supplement.go", "models.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		text := string(src)
		// 注释里可以提到这些名字（解释为什么删），但**不许**有
		// 赋值形态的静态倍率条目 —— 那种形态只可能来自兜底表。
		for _, bad := range []string{
			"intlSupplementalModels",
			"applySupplementalModels",
			"supplementalModel{",
		} {
			if strings.Contains(text, bad) {
				t.Errorf("%s 里仍存在静态兜底表符号 %q —— "+
					"委托方 2026-10-09 要求删除（理由见 supplement.go 包注释）",
					f, bad)
			}
		}
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
