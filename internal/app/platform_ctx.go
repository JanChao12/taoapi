package app

import (
	"context"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// ─────────────────────────────────────────────────────────────
// 请求级平台上下文（2026-10-06 接入国际版时加）
//
// 🔴 要解决的问题：
//
//	本服务同时代理国内版与国际版。一个请求进来时，必须知道
//	它要打哪个平台 —— 因为：
//	  · 两版是**独立的账号体系**，凭据不能跨域名使用
//	  · 同名模型（glm-5.3）在两版的倍率/能力都不同
//
//	平台在**解析模型时**就能确定（模型 ID 带平台前缀：
//	`workbuddy/` 或 `workbuddyai/`），而选号发生在更下游
//	（TryChat → pool selector）。
//
// 🔴 为什么用 context 而不是加函数参数：
//
//	从 handleChat 到 TryChat 之间要穿过 streamChat / aggregateChat
//	两层，且 TryChat 的签名是既有的调度入口。加参数会波及
//	所有调用方（含测试），而平台是**请求级**的固有属性 ——
//	正是 context 的用途。
//
// ⚠️ 缺省值：context 里没有平台时返回空串（= 不过滤）。
//	这保证既有调用（CLI、测试）行为不变；
//	但**面板/HTTP 路径必须显式注入**，否则多平台下会选错号。
// ─────────────────────────────────────────────────────────────

// platformCtxKey 是本包私有的 context key 类型。
//
// 用自定义类型而不是字符串：避免与其他包的 key 撞车
// （Go 官方建议，`go vet` 也会警告字符串 key）。
type platformCtxKey struct{}

// withPlatform 把"本次请求属于哪个平台"放进 context。
func withPlatform(ctx context.Context, plat string) context.Context {
	if plat == "" {
		return ctx
	}
	return context.WithValue(ctx, platformCtxKey{}, plat)
}

// platformFromContext 取出请求所属平台；未设置时返回空串（不过滤）。
func platformFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(platformCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// authPlatformOfProviderID 把渠道前缀转成 auth 包的平台取值。
//
// 🔴 为什么需要这个转换（2026-10-06，真实踩过）：
//
//	两个概念**取值不同**，很容易混：
//	  · 渠道前缀（provider ID）："workbuddy" / "workbuddyai"
//	  · 账号平台（auth.Account.Platform）："cn" / "intl"
//
//	我第一版把 providerID 直接当平台放进 context，
//	而账号过滤用的是 NormalizePlatform("workbuddy") == "cn" ——
//	两边永不相等，**所有请求都变成"没有可用账号"**。
//
// 未知前缀 ⇒ 返回空串（不过滤）。这比猜一个平台安全：
// 猜错会让请求被拒（可见），而返回空串至少行为与单平台时代一致。
func authPlatformOfProviderID(providerID string) string {
	switch providerID {
	case workbuddy.ProviderID:
		return auth.PlatformCN
	case workbuddy.ProviderIDIntl:
		return auth.PlatformIntl
	}
	return ""
}
