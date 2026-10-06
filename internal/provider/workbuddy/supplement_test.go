package workbuddy

import (
	"context"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// TestSupplementalModelsIntlOnly 守：补充表**只对国际版生效**。
//
// 🔴 这是一条"断言消极行为"的测试（按评审纪律：对方指出某判定过于激进，
// 就加一个测试守住）。理由：
//
//	国内版的 17 个模型是委托方**明确冻结**的决策
//	（原话「我只用客户端能选的那 17 个，老模型明确不要」）。
//	若有人把补充表错用到国内版，就会凭空多出 5 个模型，
//	**违反已冻结的产品决策** —— 这个测试会立刻抓住。
func TestSupplementalModelsIntlOnly(t *testing.T) {
	base := []provider.Model{{ID: "workbuddy/glm-5.3", UpstreamID: "glm-5.3"}}

	got := applySupplementalModels(base, PlatformCN)

	if len(got) != len(base) {
		t.Fatalf("国内版被补进了模型：%d → %d 个（国内版必须保持原样）",
			len(base), len(got))
	}
	for _, m := range got {
		if isSupplementalModel(m.UpstreamID) {
			t.Errorf("国内版出现了补充表模型 %q —— 国内版必须只用客户端可选的 17 个",
				m.ID)
		}
	}
}

// TestSupplementalModelsAddedForIntl 守：国际版确实补进了那 5 个。
func TestSupplementalModelsAddedForIntl(t *testing.T) {
	got := applySupplementalModels(nil, PlatformIntl)

	if len(got) != len(intlSupplementalModels) {
		t.Fatalf("补进 %d 个，期望 %d 个", len(got), len(intlSupplementalModels))
	}

	byID := map[string]provider.Model{}
	for _, m := range got {
		byID[m.ID] = m
	}

	// 逐个核对 ID 前缀与倍率（倍率值来自交叉验证过的 cache，见 supplement.go）
	want := map[string]float64{
		"workbuddyai/deepseek-v4.1-flash":    0,
		"workbuddyai/deepseek-v4.1-flash-sg": 0.03,
		"workbuddyai/glm-5.3-flash":          0.06,
		"workbuddyai/kimi-k2.8-preview":      0.77,
		"workbuddyai/gpt-6-astra":            6.67,
	}
	for id, rate := range want {
		m, ok := byID[id]
		if !ok {
			t.Errorf("缺少 %s；实际有 %v", id, keysOf(byID))
			continue
		}
		if m.Pricing == nil || !m.Pricing.HasMultiplier {
			t.Errorf("%s 没有倍率", id)
			continue
		}
		if m.Pricing.Multiplier != rate {
			t.Errorf("%s 倍率 = %v，期望 %v", id, m.Pricing.Multiplier, rate)
		}
		// 前缀必须是国际版前缀，不能是 workbuddy/
		if !strings.HasPrefix(m.ID, ProviderIDIntl+"/") {
			t.Errorf("%s 前缀错误（应为 %s/）", id, ProviderIDIntl)
		}
		// UpstreamID 必须是裸 ID（不带前缀），否则请求会发错
		if strings.Contains(m.UpstreamID, "/") {
			t.Errorf("%s 的 UpstreamID = %q，不应含前缀", id, m.UpstreamID)
		}
	}
}

// TestSupplementalDoesNotOverwriteUpstream 守：上游已收录时**以权威数据为准**。
//
// 🔴 为什么这条重要：若上游将来把某个模型收进目录并**改了倍率**，
// 我们的静态表如果覆盖它，用户就会看到过期的价格。
// 静态表只应做"上游没有时"的兜底。
func TestSupplementalDoesNotOverwriteUpstream(t *testing.T) {
	// 模拟上游已收录 glm-5.3-flash，且倍率与我们的静态表(0.06)不同
	const upstreamRate = 0.99
	base := []provider.Model{{
		ID:         "workbuddyai/glm-5.3-flash",
		UpstreamID: "glm-5.3-flash",
		Name:       "上游给的名字",
		Pricing:    &provider.Pricing{Multiplier: upstreamRate, HasMultiplier: true},
	}}

	got := applySupplementalModels(base, PlatformIntl)

	var found int
	for _, m := range got {
		if m.UpstreamID == "glm-5.3-flash" {
			found++
			if m.Pricing.Multiplier != upstreamRate {
				t.Errorf("上游倍率被静态表覆盖了：%v（应为上游的 %v）",
					m.Pricing.Multiplier, upstreamRate)
			}
			if m.Name != "上游给的名字" {
				t.Errorf("上游名字被覆盖了：%q", m.Name)
			}
		}
	}
	if found != 1 {
		t.Errorf("glm-5.3-flash 出现 %d 次，期望 1 次（不能重复）", found)
	}
}

// TestSupplementalOnlyOnce 守：重复调用不会累加出重复模型。
//
// 上游若某天收录了部分模型，两次补充不应产生重复 ID。
func TestSupplementalOnlyOnce(t *testing.T) {
	first := applySupplementalModels(nil, PlatformIntl)
	second := applySupplementalModels(first, PlatformIntl)

	if len(first) != len(second) {
		t.Fatalf("二次补充改变了数量：%d → %d（应幂等）", len(first), len(second))
	}
	seen := map[string]int{}
	for _, m := range second {
		seen[m.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("%s 出现 %d 次（应只 1 次）", id, n)
		}
	}
}

// TestSupplementalOutputSorted 守：补充后列表仍按 ID 有序。
//
// 顺序会影响 /v1/models 的可复现性（原测试 TestModelsSorted 依赖这点）。
func TestSupplementalOutputSorted(t *testing.T) {
	base := []provider.Model{
		{ID: "workbuddyai/zzz-last", UpstreamID: "zzz-last"},
		{ID: "workbuddyai/aaa-first", UpstreamID: "aaa-first"},
	}
	got := applySupplementalModels(base, PlatformIntl)

	for i := 1; i < len(got); i++ {
		if got[i-1].ID > got[i].ID {
			t.Errorf("未排序：%q 在 %q 之前", got[i-1].ID, got[i].ID)
		}
	}
}

// TestSupplementalUnknownCapabilitiesNotFabricated 守：不臆造上下文/能力。
//
// 🔴 按纪律「宁缺勿假」：上游目录没有这些模型的字段，
// 我们无从得知上下文窗口与是否支持图片/工具。
// 必须留零值，**不能**填一个看起来合理的猜测值
// （那会让用户以为支持图片，实际调用报错）。
func TestSupplementalUnknownCapabilitiesNotFabricated(t *testing.T) {
	got := applySupplementalModels(nil, PlatformIntl)
	for _, m := range got {
		if m.Capabilities.ContextWindow != 0 {
			t.Errorf("%s 上下文 = %d，应从缺（未知），不得臆造",
				m.ID, m.Capabilities.ContextWindow)
		}
		if m.Capabilities.SupportsImages {
			t.Errorf("%s 声称支持图片，但没有证据", m.ID)
		}
		if m.Capabilities.SupportsTools {
			t.Errorf("%s 声称支持工具，但没有证据", m.ID)
		}
		if m.Capabilities.Reasoning != nil {
			t.Errorf("%s 声称有思考档位，但没有证据", m.ID)
		}
	}
}

// TestSupplementalDeepseekSGIsDistinct 守：同名 SG 版本不被合并。
//
// `deepseek-v4.1-flash` 与 `deepseek-v4.1-flash-sg` 是**两个不同模型**
// （不同节点、不同倍率 0 vs 0.03）。若哪天有人"去重"掉一个，这条会抓到。
func TestSupplementalDeepseekSGIsDistinct(t *testing.T) {
	got := applySupplementalModels(nil, PlatformIntl)
	plain, sg := false, false
	for _, m := range got {
		switch m.UpstreamID {
		case "deepseek-v4.1-flash":
			plain = true
		case "deepseek-v4.1-flash-sg":
			sg = true
		}
	}
	if !plain || !sg {
		t.Fatalf("两个 deepseek 变体必须都存在（plain=%v sg=%v）", plain, sg)
	}
}

// TestModelsIntlIncludesSupplemental 是端到端验证：
// 国际版经完整 Models() 流程后应含补充模型，且总数为 13+5。
//
// ⚠️ 2026-10-07 更新：国际版现在会拉**两个**端点
// （/v2/.../models 与 /v3/config），所以这里必须给两个不同的样本
// —— 只给一个会让 /v3 误拿到 /v2 的数据，测不出合并逻辑。
func TestModelsIntlIncludesSupplemental(t *testing.T) {
	c, fake := newFakeClient(t)
	fake.WithJSON("models-listing-intl.json")
	fake.WithJSONForPath("/v3/config", "v3-config-intl.json")

	models, err := c.Models(context.Background(), testCred(), PlatformIntl)
	if err != nil {
		t.Fatalf("拉取国际版模型失败: %v", err)
	}

	byID := map[string]provider.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}

	// 补充的 5 个都应在
	for _, want := range []string{
		"workbuddyai/deepseek-v4.1-flash",
		"workbuddyai/deepseek-v4.1-flash-sg",
		"workbuddyai/glm-5.3-flash",
		"workbuddyai/kimi-k2.8-preview",
		"workbuddyai/gpt-6-astra",
	} {
		if _, ok := byID[want]; !ok {
			t.Errorf("国际版缺少补充模型 %s", want)
		}
	}

	// 5 个抽象档位仍必须被砍掉（委托方要求）
	for _, bad := range []string{
		"workbuddyai/default-model",
		"workbuddyai/fast-model",
		"workbuddyai/balanced-model",
		"workbuddyai/primary-model",
		"workbuddyai/deep-model",
	} {
		if _, ok := byID[bad]; ok {
			t.Errorf("抽象档位 %s 不应出现（委托方明确要求砍掉）", bad)
		}
	}
}

func keysOf(m map[string]provider.Model) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
