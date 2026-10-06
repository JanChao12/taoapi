package workbuddy

import (
	"context"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// ProviderID 是国内版的渠道标识（同时用作模型名前缀）。
//
// ⚠️ 这是**默认值**，不是唯一值 —— 国际版用 ProviderIDIntl。
// 历史代码引用了这个常量，保留它以免大规模改动。
const ProviderID = "workbuddy"

// ProviderIDIntl 是国际版（WorkBuddyAI）的渠道标识与模型名前缀。
//
// 🔴 为什么两版要用**不同前缀**（委托方 2026-10-06 定）：
//
//	两边的模型集**部分重叠**（如 `glm-5.3` 两边都有），
//	若都用 `workbuddy/` 前缀，同名模型会**互相覆盖** ——
//	路由表里只会留下一个，另一个永远调不到。
//	分开之后：`workbuddy/glm-5.3` 与 `workbuddyai/glm-5.3` 各走各的。
//
// ⚠️ 两版的同名模型**能力与倍率也可能不同**（实测：`glm-5.3` 的
// maxOutput 国内 64000 / 国际 48000），进一步说明不能混为一谈。
const ProviderIDIntl = "workbuddyai"

// Platform 标识一个上游平台。
type Platform string

const (
	// PlatformCN 国内版（copilot.tencent.com / www.codebuddy.cn）。
	PlatformCN Platform = "cn"

	// PlatformIntl 国际版（www.workbuddy.ai，chat 与 billing 同域名）。
	PlatformIntl Platform = "intl"
)

// 各平台基址（实测）。
const (
	cnChatBase    = "https://copilot.tencent.com"
	cnBillingBase = "https://www.codebuddy.cn"

	// 国际版 chat 与 billing **同一个域名**（与国内版不同）。
	intlBase = "https://www.workbuddy.ai"
)

// BaseURLs 返回该平台的 (chat, billing) 基址。
func (p Platform) BaseURLs() (string, string) {
	if p == PlatformIntl {
		return intlBase, intlBase
	}
	return cnChatBase, cnBillingBase
}

// ProviderID 返回该平台对应的渠道 ID / 模型前缀。
func (p Platform) ProviderID() string {
	if p == PlatformIntl {
		return ProviderIDIntl
	}
	return ProviderID
}

// NeedsSystemMessage 报告该平台是否要求首条消息是 system。
//
// 🔴 实测（2026-10-06）：国际版**强制要求**，否则返回
//
//	HTTP 400 {"code":11-128,"msg":"first message is not system prompt"}
//
// 国内版没有这个约束。加上 system 后同一个请求立刻成功。
func (p Platform) NeedsSystemMessage() bool {
	return p == PlatformIntl
}

// HasCheckin 报告该平台是否有签到活动。
//
// 🔴 实测（2026-10-06）：国际版的签到端点**调用不报错**，
// 但返回 "签到活动未开启或已过期" —— 即**没有这个活动**。
//
// ⚠️ 面板若仍显示签到按钮，用户点了会看到"签到成功"却毫无效果 ——
// 那是误导。所以按平台隐藏该功能。
//
// ⚠️ 局限：只测了一个国际版账号，无法区分"该账号没有"与
// "国际版整体没有"。按保守处理（不显示）。
func (p Platform) HasCheckin() bool {
	return p != PlatformIntl
}

// Provider 把 Client 适配成 provider.Provider。
//
// 之所以分两层：
//   - Client 只关心"怎么调上游"
//   - Provider 关心"如何满足 provider.Provider 接口"
//
// 这样认证方式变化（如换成 device-code）不会影响 Client 的请求构造。
type Provider struct {
	client *Client
	cred   Credential

	// platform 决定 ID()（即模型名前缀）与若干行为差异。
	//
	// zero value（""）按国内版处理 —— 兼容未显式指定平台的老代码。
	platform Platform
}

// NewProvider 构造国内版 provider（保持向后兼容）。
func NewProvider(c *Client, cred Credential) *Provider {
	return NewProviderFor(c, cred, PlatformCN)
}

// NewProviderFor 按指定平台构造 provider。
//
// 🔴 **不在这里调用 SetBases** —— 这是一个**刻意的**决定，别改回去：
//
//	第一版我在构造函数里顺手 `c.SetBases(平台基址)`，想防止调用方
//	忘记设基址。结果是**测试全线崩**：测试的写法是
//	  `c := NewClient(); c.SetBases(fakeURL, fakeURL); NewProvider(c, cred)`
//	构造函数再覆盖一次，请求就**打到了真实上游** ——
//	表现为大规模 401，而且**测试从"隔离"变成了"联网"**，
//	这是比单测失败严重得多的问题。
//
// 现在的约定：**基址由调用方显式设置**（生产在 serve.go、
// 测试在 testing_helpers_test.go）。谁改基址谁负责。
//
// ⚠️ 平台只决定 ID()（模型前缀）与行为差异，不隐式改网络目标。
func NewProviderFor(c *Client, cred Credential, p Platform) *Provider {
	if p == "" {
		p = PlatformCN
	}
	return &Provider{client: c, cred: cred, platform: p}
}

// ID 实现 provider.Provider（同时是模型名前缀）。
func (p *Provider) ID() string { return p.platform.ProviderID() }

// Platform 返回所属平台。
func (p *Provider) Platform() Platform { return p.platform }

// Models 实现 provider.Provider。
func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	return p.client.Models(ctx, p.cred, p.platform)
}

// Client 暴露底层客户端（测试与后续步骤用）。
func (p *Provider) Client() *Client { return p.client }

// Credential 返回当前凭证的副本。
func (p *Provider) Credential() Credential { return p.cred }
