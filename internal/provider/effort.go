package provider

import "strings"

// 本文件是「思考档位」的**唯一权威定义**。
//
// 🔴 为什么集中到 provider 层（2026-10-09，加 Responses 协议时）：
//
//	"哪些档位值上游真的接受"是一个**实测事实**，而它已经导致过真实缺陷：
//	`off` 曾被 /v1/models 当作合法档位对外声明，而直连实测上游对它返回
//	HTTP 400 code=11150。当时这个白名单在 workbuddy/models.go 里，
//	而翻译逻辑在 workbuddy/chat.go 里 —— 两处各说各话，修一处漏一处。
//
//	现在有三处需要这个事实（workbuddy 渠道、anthropic 协议、responses 协议），
//	因此定义在它们共同的依赖 provider 层，**只有一份**。
//
// 来源：docs/upstream-contract.md §八「合法档位白名单（实测被接受的）」。

// upstreamEfforts 是上游实测接受的档位值。
//
// ⚠️ 刻意不含 "off" 与 "none" —— 理由见 IsUpstreamEffort 的说明。
var upstreamEfforts = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
	"max":     true,
	"ultra":   true,
}

// IsUpstreamEffort 报告 e 是否是上游实测接受的档位值。
//
// 🔴 "off" 与 "none" 都**不在**白名单里，这是实测结论不是遗漏：
//
//	直连实测（2026-10-09，模型 deepseek-v4.1-flash，经本服务打上游）：
//	  reasoning_effort:"off"   → HTTP 400 code=11150
//	                             "the reasoning effort value is not
//	                              supported by the current model"
//	  reasoning_effort:"none"  → HTTP 200，但 reasoning_content **仍非空**
//	                             （也就是说"none"根本没关掉思考）
//	  【不传该字段】            → HTTP 200，思考分片 0（fixture 铁证）
//
//	⇒ 二者都不能作为 reasoning_effort 发给上游。
//	  唯一能关闭思考的方式是**不传该字段**，见 DisablesThinking。
func IsUpstreamEffort(e string) bool {
	return upstreamEfforts[strings.ToLower(strings.TrimSpace(e))]
}

// UpstreamEfforts 返回白名单（升序）。
//
// 供报错文案与"客户端给了非法档位"的诊断信息使用 ——
// 让用户一眼看到可用值，而不是收到一个上游的 code=11150。
func UpstreamEfforts() []string {
	return []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

// DisablesThinking 报告 e 是否表示「关闭思考」。
//
// 🔴 调用方**必须**把它翻译成「不传 reasoning_effort」，绝不能原样透传。
//
//	上游根本没有"关闭思考"的档位取值：
//	  "off"  → 400（不被支持）
//	  "none" → 200，但仍产出思考（等于没关）
//	  【不传】 → 真正关闭
//
//	DSH 侧的做法与此完全一致：它的配置写 `off: null`，即"留空什么都不发送"
//	（见 DSH 官方文档 providers.zh.md 第 134 行：
//	 「留空的 off 什么都不发送，这只能让「按请求才思考」的模型停下来」）。
//
// ⚠️ 语义边界：对"不明确关闭就会思考"的模型（如 space-bunny，
//
//	上游声明 canDisableThinking=false），不传只是回到默认档 ——
//	这是能做到的最好结果，比发一个必然 400 的值诚实得多。
func DisablesThinking(e string) bool {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "off", "none":
		return true
	}
	return false
}
