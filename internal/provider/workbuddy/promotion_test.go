package workbuddy

import (
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// ─────────────────────────────────────────────────────────────
// 运营活动（modelPromotions）解析的护栏测试（2026-10-06）
//
// 🔴 背景（委托方质问："为什么你拿不到这些信息"）：
//
//	我们此前**完全没读**上游的 `modelPromotions` 字段，
//	只用了 tags 里的 `badge:` 简写。两者差别是**决定性的**：
//
//	  badge（简写）      ：静态，活动过期后仍留在 tags 里
//	  promotion（权威）  ：带 enabled 开关 + schedule 时间窗
//
//	⇒ 用简写会显示一个**已过期**的"免费"标签，误导用户以为不花钱。
//	  实测（2026-10-06）：国际版两条活动 validUntil 分别是
//	  2026-09-30 与 2026-09-08，**都已过期**，但 tags 里仍有 badge。
// ─────────────────────────────────────────────────────────────

// TestPromotionActiveRespectsTimeWindow 守：过期活动被判为不生效。
//
// 这是本轮最核心的一条 —— 它证明"用 promotion 而不是 badge"的价值。
func TestPromotionActiveRespectsTimeWindow(t *testing.T) {
	// 实测原样：国际版 hy3 的活动
	p := &wirePromotion{
		ID:      "hy3-free-trial-202608",
		Enabled: true,
		Schedule: &struct {
			Timezone   string `json:"timezone"`
			ValidFrom  string `json:"validFrom"`
			ValidUntil string `json:"validUntil"`
		}{
			Timezone:   "Asia/Shanghai",
			ValidFrom:  "2026-07-06T00:00:00+08:00",
			ValidUntil: "2026-09-30T00:00:00+08:00",
		},
	}

	// 活动期内
	inWindow := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !promotionActive(p, inWindow) {
		t.Error("2026-08-15 在活动期内（7/6–9/30），应判为生效")
	}

	// 活动**已过期** —— 实测的当前情况（今天 2026-10-06）
	after := time.Date(2026, 10, 6, 18, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if promotionActive(p, after) {
		t.Error("2026-10-06 已超过 validUntil(9/30)，应判为**不生效** —— " +
			"否则会显示一个已过期的「Free now」，误导用户以为现在免费")
	}

	// 活动尚未开始
	before := time.Date(2026, 6, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if promotionActive(p, before) {
		t.Error("2026-06-01 早于 validFrom(7/6)，应判为不生效")
	}
}

// TestPromotionActiveNoScheduleMeansLongRunning 守：没有时间窗视为长期有效。
//
// 上游确实可能不给 schedule（不是所有活动都限时）。
// 这种情况不该被误判为"过期"而全部隐藏。
func TestPromotionActiveNoScheduleMeansLongRunning(t *testing.T) {
	p := &wirePromotion{ID: "no-schedule", Enabled: true}
	if !promotionActive(p, time.Now()) {
		t.Error("无 schedule 应视为长期有效，不该一律隐藏")
	}
}

// TestPromotionActiveBadTimeSkipped 守：时间无法解析时**跳过**（保守）。
//
// 🔴 与上一条的区别：无 schedule = 长期有效（显示）；
//
//	有 schedule 但解析失败 = 数据有问题 ⇒ **不显示**。
//	宁可漏显示一个免费标签，也不要显示一个可能已过期的。
func TestPromotionActiveBadTimeSkipped(t *testing.T) {
	p := &wirePromotion{
		ID:      "bad-time",
		Enabled: true,
		Schedule: &struct {
			Timezone   string `json:"timezone"`
			ValidFrom  string `json:"validFrom"`
			ValidUntil string `json:"validUntil"`
		}{ValidUntil: "not-a-time"},
	}
	if promotionActive(p, time.Now()) {
		t.Error("时间解析失败时应跳过该活动（保守），而不是当作长期有效")
	}
}

// TestApplyPromotionsPicksHighestPriority 守：同模型多条活动取优先级最高的。
func TestApplyPromotionsPicksHighestPriority(t *testing.T) {
	now := time.Now()
	models := []provider.Model{
		{ID: "workbuddy/hy3", UpstreamID: "hy3"},
	}
	promos := []wirePromotion{
		{ID: "low", Enabled: true, Priority: 10, ModelIDs: []string{"hy3"},
			Badge: &struct {
				Label   string `json:"label"`
				Color   string `json:"color"`
				Display string `json:"display"`
			}{Label: "低优先级"}},
		{ID: "high", Enabled: true, Priority: 200, ModelIDs: []string{"hy3"},
			Badge: &struct {
				Label   string `json:"label"`
				Color   string `json:"color"`
				Display string `json:"display"`
			}{Label: "高优先级"}},
	}

	applyPromotions(models, promos, now)

	if models[0].Promotion == nil {
		t.Fatal("活动应被应用")
	}
	if models[0].Promotion.Label != "高优先级" {
		t.Errorf("选中了 %q，期望「高优先级」（priority 200 > 10）",
			models[0].Promotion.Label)
	}
}

// TestApplyPromotionsSkipsDisabled 守：enabled=false 的活动被跳过。
func TestApplyPromotionsSkipsDisabled(t *testing.T) {
	models := []provider.Model{{ID: "workbuddy/hy3", UpstreamID: "hy3"}}
	promos := []wirePromotion{
		{ID: "off", Enabled: false, ModelIDs: []string{"hy3"},
			Badge: &struct {
				Label   string `json:"label"`
				Color   string `json:"color"`
				Display string `json:"display"`
			}{Label: "已停"}},
	}
	applyPromotions(models, promos, time.Now())
	if models[0].Promotion != nil {
		t.Error("enabled=false 的活动不该被应用")
	}
}

// TestPromotionFreeFlagSemantics 守：Free 只由 factor==0 决定。
//
// 🔴 "免费"与"有折扣但不免费"完全不同，不能混：
//
//	factor=0   ⇒ Free 徽标
//	factor=0.5 ⇒ 五折（不是免费！）
func TestPromotionFreeFlagSemantics(t *testing.T) {
	free := promotionOf(&wirePromotion{
		Discount: &struct {
			Factor            float64 `json:"factor"`
			DiscountedCredits string  `json:"discountedCredits"`
			DisplayMode       string  `json:"displayMode"`
		}{Factor: 0},
	})
	if !free.Free {
		t.Error("factor=0 应标记为 Free")
	}

	half := promotionOf(&wirePromotion{
		Discount: &struct {
			Factor            float64 `json:"factor"`
			DiscountedCredits string  `json:"discountedCredits"`
			DisplayMode       string  `json:"displayMode"`
		}{Factor: 0.5},
	})
	if half.Free {
		t.Error("factor=0.5 是五折，**不是**免费 —— 标成 Free 会误导用户")
	}
}

// TestPromotionColorValidated 守：活动颜色同样做安全校验。
//
// 颜色会进内联 style，不能让上游塞任意 CSS。
func TestPromotionColorValidated(t *testing.T) {
	ok := promotionOf(&wirePromotion{
		Badge: &struct {
			Label   string `json:"label"`
			Color   string `json:"color"`
			Display string `json:"display"`
		}{Label: "x", Color: "#FF0000"},
	})
	if ok.Color != "#FF0000" {
		t.Errorf("合法颜色应保留，实际 %q", ok.Color)
	}

	bad := promotionOf(&wirePromotion{
		Badge: &struct {
			Label   string `json:"label"`
			Color   string `json:"color"`
			Display string `json:"display"`
		}{Label: "x", Color: "url(evil)"},
	})
	if bad.Color != "" {
		t.Errorf("非法颜色应被丢弃，实际 %q", bad.Color)
	}
}
