package app

import (
	"context"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// ─────────────────────────────────────────────────────────────
// 请求级平台上下文的护栏测试（2026-10-06）
//
// 🔴 守的是一个**真实踩过的 bug**：
//
//	两个概念取值不同，极易混：
//	  · 渠道前缀（provider ID）："workbuddy" / "workbuddyai"
//	  · 账号平台（auth.Account.Platform）："cn" / "intl"
//
//	第一版我把 providerID 直接当平台放进 context，
//	而账号过滤用的是 NormalizePlatform("workbuddy") == "cn" ——
//	两边**永不相等**，所有请求都变成"没有可用账号"。
//	表现为 503 no_available_account，而账号明明是可用的。
// ─────────────────────────────────────────────────────────────

// TestAuthPlatformOfProviderID 守：前缀 → 平台取值的映射。
func TestAuthPlatformOfProviderID(t *testing.T) {
	cases := []struct {
		providerID string
		want       string
	}{
		{workbuddy.ProviderID, auth.PlatformCN},       // "workbuddy"  → "cn"
		{workbuddy.ProviderIDIntl, auth.PlatformIntl}, // "workbuddyai" → "intl"
		{"unknown-provider", ""},                      // 未知 ⇒ 空串（不过滤）
		{"", ""},
	}
	for _, c := range cases {
		if got := authPlatformOfProviderID(c.providerID); got != c.want {
			t.Errorf("authPlatformOfProviderID(%q) = %q，期望 %q",
				c.providerID, got, c.want)
		}
	}
}

// TestPlatformValuesMatchAccountNormalization 守：映射结果能被账号过滤识别。
//
// 🔴 这条才是真正抓住那个 bug 的断言：
//
//	光测"映射对不对"不够 —— 还要确认映射出来的值
//	与账号实际比较时用的值**一致**。
//	第一版两边分别是 "workbuddy" 与 "cn"，各自"看起来都对"，
//	但**放在一起就不匹配**。
func TestPlatformValuesMatchAccountNormalization(t *testing.T) {
	// 国内账号
	cnAcct := &auth.Account{UID: "u1"}
	if cnAcct.PlatformOf() != auth.PlatformCN {
		t.Fatalf("空 Platform 的账号应归一为国内版，实际 %q", cnAcct.PlatformOf())
	}
	// 从 providerID 推出的平台必须与它相等
	if got := authPlatformOfProviderID(workbuddy.ProviderID); got != cnAcct.PlatformOf() {
		t.Errorf("providerID %q 推出平台 %q，但国内账号的平台是 %q —— "+
			"两者不一致会让账号过滤永不匹配（表现为「没有可用账号」）",
			workbuddy.ProviderID, got, cnAcct.PlatformOf())
	}

	// 国际账号
	intlAcct := &auth.Account{UID: "u2", Platform: auth.PlatformIntl}
	if got := authPlatformOfProviderID(workbuddy.ProviderIDIntl); got != intlAcct.PlatformOf() {
		t.Errorf("providerID %q 推出平台 %q，但国际账号的平台是 %q",
			workbuddy.ProviderIDIntl, got, intlAcct.PlatformOf())
	}
}

// TestWithPlatformRoundTrip 守：context 存取。
func TestWithPlatformRoundTrip(t *testing.T) {
	base := context.Background()

	if got := platformFromContext(base); got != "" {
		t.Errorf("未设置时应返回空串，实际 %q", got)
	}
	ctx := withPlatform(base, auth.PlatformIntl)
	if got := platformFromContext(ctx); got != auth.PlatformIntl {
		t.Errorf("取回 %q，期望 %q", got, auth.PlatformIntl)
	}
	// 空串不写入（保持原 ctx）
	if got := platformFromContext(withPlatform(base, "")); got != "" {
		t.Errorf("withPlatform 空串时不应写入，实际 %q", got)
	}
	// nil ctx 不 panic
	//nolint:staticcheck // 故意传 nil 验证健壮性
	if got := platformFromContext(nil); got != "" {
		t.Errorf("nil ctx 应返回空串，实际 %q", got)
	}
}
