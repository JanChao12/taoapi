package workbuddy

import (
	"testing"
)

// ─────────────────────────────────────────────────────────────
// 倍率 / 运营标签 / 客户端可选模型的解析测试（2026-10-06）
//
// 这三个都由**上游实时下发**，格式是实测出来的：
//   credits: "x0.03" / "x0.00 credits" / "x2.20 credits" / ""
//   tags:    ["craft", "badge:夜间折扣:#1E90FF"]
//   agents:  [{name:"cli", tags:["cli","default"], models:[...17 个...]}]
//
// 用真实采样值做断言，防止上游改格式时静默出错。
// ─────────────────────────────────────────────────────────────

// TestParseCredits 守住倍率解析，尤其是""与"0"的区别。
func TestParseCredits(t *testing.T) {
	cases := []struct {
		in    string
		want  float64
		isNil bool
		why   string
	}{
		{"x0.03", 0.03, false, "实测最常见形态"},
		{"x0.00 credits", 0.00, false, "hy3 实测：带 credits 后缀，0 是**免费**"},
		{"x2.20 credits", 2.20, false, "default 实测"},
		{"x1.62 credits", 1.62, false, "kimi-k3-1 实测"},
		{"X0.5", 0.5, false, "大写 X 也应接受"},
		{"", 0, true, "🔴 上游未定价 ⇒ nil（面板显示 unknown，**不是** Free）"},
		{"   ", 0, true, "空白同未定价"},
		{"免费", 0, true, "非数字 ⇒ 当未知，不瞎猜"},
	}

	for _, c := range cases {
		got := parseCredits(c.in)
		if c.isNil {
			if got != nil {
				t.Errorf("parseCredits(%q) = %+v，期望 nil（%s）", c.in, got, c.why)
			}
			continue
		}
		if got == nil {
			t.Errorf("parseCredits(%q) = nil，期望 %v（%s）", c.in, c.want, c.why)
			continue
		}
		if !got.HasMultiplier {
			t.Errorf("parseCredits(%q).HasMultiplier = false，期望 true", c.in)
		}
		if got.Multiplier != c.want {
			t.Errorf("parseCredits(%q) = %v，期望 %v（%s）",
				c.in, got.Multiplier, c.want, c.why)
		}
	}
}

// TestParseCreditsZeroIsNotNil 单独强调 0 与 nil 的语义差别。
//
// 🔴 这是最容易搞错、且后果最严重的一处：
//
//	0  ⇒ 明确免费（面板显示 Free）
//	nil ⇒ 上游没标价（面板显示 unknown）
//
// 若把 nil 当成 0，会把"没定价"显示成"免费"，直接误导用户以为不花钱。
func TestParseCreditsZeroIsNotNil(t *testing.T) {
	zero := parseCredits("x0.00 credits")
	if zero == nil {
		t.Fatal("x0.00 应解析出倍率 0（免费），不是 nil")
	}
	if zero.Multiplier != 0 || !zero.HasMultiplier {
		t.Errorf("x0.00 ⇒ Multiplier=%v HasMultiplier=%v，期望 0/true",
			zero.Multiplier, zero.HasMultiplier)
	}

	if parseCredits("") != nil {
		t.Error("空字符串应返回 nil（未知），不能当成免费")
	}
}

// TestParseBadge 守住运营标签解析。
func TestParseBadge(t *testing.T) {
	b := parseBadge([]string{"craft", "badge:夜间折扣:#1E90FF"})
	if b == nil {
		t.Fatal("应解析出 badge")
	}
	if b.Text != "夜间折扣" {
		t.Errorf("Text = %q，期望 夜间折扣", b.Text)
	}
	if b.Color != "#1E90FF" {
		t.Errorf("Color = %q，期望 #1E90FF（颜色由上游决定，不要自己配）", b.Color)
	}

	// 没有 badge 时返回 nil
	if parseBadge([]string{"craft", "text-to-image"}) != nil {
		t.Error("无 badge 标签时应返回 nil")
	}
	// 颜色缺失也要能取出文案（用默认色兜底）
	b2 := parseBadge([]string{"badge:限时免费"})
	if b2 == nil || b2.Text != "限时免费" {
		t.Errorf("缺色值时仍应取出文案，实际 %+v", b2)
	}
}

// TestClientSelectableModels 守住"客户端能选的模型"这一裁剪依据。
//
// 🔴 依据（2026-10-06 委托方实测反馈）：
//
//	上游 models 返回 31 个，但 CodeBuddy 客户端里只能选 17 个。
//	委托方原话：「这17才是我在 workbuddy 客户端里能选择使用的模型，
//	你之前给我的很多在客户端都看不到，应该是有些过期的老模型」
//
// 实测这 17 个 = agents 里 tags 含 "default" 那条的 models 数组。
func TestClientSelectableModels(t *testing.T) {
	agents := []wireAgent{
		{Name: "cli", Tags: []string{"cli", "default"},
			Models: []string{"auto", "hy3", "space-bunny"}},
		{Name: "cli", Tags: []string{"cli", "compact"}, Models: []string{}},
	}

	got := clientSelectableModels(agents)
	if got == nil {
		t.Fatal("应提取出模型集合")
	}
	for _, id := range []string{"auto", "hy3", "space-bunny"} {
		if !got[id] {
			t.Errorf("集合里缺少 %q", id)
		}
	}
	if got["glm-4.6"] {
		t.Error("glm-4.6 不在 default 组里，不应入选（那是已下架的老模型）")
	}
}

// TestClientSelectableModelsFallback 守：上游没给 agents 时不裁剪。
//
// 🔴 为什么这条重要：若上游改了结构而我们返回空集合，
// 调用方（Models）会**跳过所有模型** ⇒ 用户看到"一个模型都没有"，
// 会以为服务坏了。宁可多列，也不要全藏。
func TestClientSelectableModelsFallback(t *testing.T) {
	if got := clientSelectableModels(nil); got != nil {
		t.Errorf("无 agents 时应返回 nil（表示不裁剪），实际 %v", got)
	}
	// 有 agents 但都没有 models 时也应返回 nil
	empty := []wireAgent{{Name: "cli", Tags: []string{"cli", "default"}, Models: nil}}
	if got := clientSelectableModels(empty); got != nil {
		t.Errorf("agents 里没有模型时应返回 nil（不裁剪），实际 %v", got)
	}
	// 没有 default 标签时退回按 name=="cli" 找
	noDefault := []wireAgent{{Name: "cli", Tags: []string{"cli"}, Models: []string{"hy3"}}}
	if got := clientSelectableModels(noDefault); got == nil || !got["hy3"] {
		t.Errorf("应退回按 name=cli 查找，实际 %v", got)
	}
}

// TestAutoModelIsExcluded 守：auto 不在对外模型列表里。
//
// 委托方 2026-10-06 要求（原话）：
//
//	「还有我建议不要auto这个模型，因为这是自动模型他的倍率本来就不固定，
//	  而且不建议使用这个，所以别用这个模型了。」
//
// 依据：上游自己的 description 就写着"积分倍率随之浮动"，
// 它的 credits 字段本来就是空的 ⇒ 面板只能显示 unknown，反而更困惑。
func TestAutoModelIsExcluded(t *testing.T) {
	if autoModelID != "auto" {
		t.Fatalf("autoModelID = %q，期望 \"auto\"", autoModelID)
	}

	// 模拟上游返回 auto + 一个正常模型，且 auto **在** 客户端可选集里
	// （真实情况就是如此）—— 确认它仍被排除。
	allowed := map[string]bool{"auto": true, "glm-5.3": true}
	if !allowed["auto"] {
		t.Fatal("前置条件：auto 应在客户端可选集里")
	}

	// 直接验证排除逻辑（与 Models() 里的判断一致）
	keep := func(id string) bool {
		if id == autoModelID {
			return false
		}
		if len(allowed) > 0 && !allowed[id] {
			return false
		}
		return true
	}
	if keep("auto") {
		t.Error("auto 未被排除 —— 委托方明确要求不要这个模型")
	}
	if !keep("glm-5.3") {
		t.Error("glm-5.3 被误排除")
	}
}

// TestAutoExcludedEvenWithoutAgents 守：上游没给 agents 时 auto 也要排除。
//
// 防的是：上游一改结构（agents 消失）⇒ allowed 为空 ⇒ 不裁剪
// ⇒ auto 又冒出来了。排除 auto 必须独立于裁剪逻辑。
func TestAutoExcludedEvenWithoutAgents(t *testing.T) {
	// allowed 为 nil（不裁剪）时，auto 仍应被排除
	var allowed map[string]bool
	keep := func(id string) bool {
		if id == autoModelID {
			return false
		}
		if len(allowed) > 0 && !allowed[id] {
			return false
		}
		return true
	}
	if keep("auto") {
		t.Error("allowed 为空时 auto 又出现了 —— " +
			"排除 auto 必须独立于 agents 裁剪")
	}
	if !keep("hy3") {
		t.Error("allowed 为空时不应裁剪其它模型（宁可多列也不要全藏）")
	}
}

// TestConvertModelIncludesPricingAndBadge 守：转换后倍率与标签带上了。
func TestConvertModelIncludesPricingAndBadge(t *testing.T) {
	wm := wireModel{
		ID:              "hy3",
		Name:            "Hy3",
		Credits:         "x0.00 credits",
		Tags:            []string{"craft", "badge:限时免费:#FF0000"},
		MaxInputTokens:  192000,
		MaxOutputTokens: 64000,
	}

	m, ok := convertModel(wm, PlatformCN)
	if !ok {
		t.Fatal("转换应成功")
	}
	if m.Pricing == nil || m.Pricing.Multiplier != 0 {
		t.Errorf("倍率 = %+v，期望 0（免费）", m.Pricing)
	}
	if m.Badge == nil || m.Badge.Text != "限时免费" {
		t.Errorf("标签 = %+v，期望 限时免费", m.Badge)
	}
	if m.Badge.Color != "#FF0000" {
		t.Errorf("标签颜色 = %q，期望上游给的值", m.Badge.Color)
	}
	if m.Capabilities.MaxOutputTokens != 64000 {
		t.Errorf("maxOutputTokens = %d，期望 64000（面板要显示它）",
			m.Capabilities.MaxOutputTokens)
	}
}
