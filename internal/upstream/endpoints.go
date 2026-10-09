// Package upstream 存放上游端点的常量定义与连接超时。
//
// 单独成包的原因：provider 实现需要这些常量，但不应依赖 app 包
// （否则 app 的集成测试引入 provider 时会形成 import cycle）。
package upstream

import "time"

// 上游连接超时。
//
// ⚠️ 这里【没有】"整个请求总超时"：
// SSE 长连接的总时长取决于模型思考时间（实测 max 档单次可达 95 秒以上），
// 设总超时会把它中途杀掉。流内空闲超时由 SSE 读取层负责。
const (
	// DialTimeout 与上游建连超时。
	DialTimeout = 10 * time.Second

	// TLSHandshakeTimeout TLS 握手超时。
	TLSHandshakeTimeout = 10 * time.Second

	// ResponseHeaderTimeout 等待上游响应头的超时。
	//
	// 上游会先返回 200 + text/event-stream 头，然后才慢慢推数据，
	// 因此这个超时只覆盖"建连到出响应头"这一段。
	ResponseHeaderTimeout = 30 * time.Second

	// KeepAlive TCP keep-alive 间隔。
	KeepAlive = 30 * time.Second

	// IdleConnTimeout 空闲连接回收时间。
	IdleConnTimeout = 90 * time.Second
)

// 上游基址（直连，非中间层）。详见 docs/upstream-contract.md。
const (
	// ChatBase 对话与模型目录。
	ChatBase = "https://copilot.tencent.com"

	// BillingBase 额度与签到。
	BillingBase = "https://www.codebuddy.cn"
)

// 上游路径。
const (
	// PathModels 模型目录。
	PathModels = "/v2/enterprises/personal/models"

	// PathV3Config 国际版的**完整模型配置**端点（2026-10-07 发现）。
	//
	// 🔴 为什么需要它（这是补上 /v2 的一个真实缺口）：
	//
	//	/v2/enterprises/personal/models 对国际版只返回 18 条（13 真实 +
	//	5 抽象档位），**不含** deepseek-v4.1-flash、glm-5.3-flash、
	//	kimi-k2.8-preview、gpt-6-astra、deepseek-v4.1-flash-sg 这 5 个
	//	**能正常调用**的模型 —— 它们此前要靠一张手工抄录的静态表兜底
	//	（该表已于 2026-10-09 按委托方要求删除，见 provider/workbuddy/supplement.go）。
	//
	//	/v3/config 返回 22 条，**包含上述 5 个**，且另有 gpt-5.3-codex
	//	只在 /v2 里，所以正确做法是**两个端点合并**（见 models.go）。
	//
	//	来源：wild-work 的 app.log 失败行暴露了该 URL，
	//	再由本项目直连实测确认（HTTP 200 / 21853 字节 / code=0）。
	//
	// ⚠️ 实测差异（决定合并语义的关键）：
	//   · /v3 每条模型 15 个字段，比 /v2 的 13 个多出 contextWindow、
	//     maxInputTokens、maxOutputTokens、onlyReasoning、reasoning 等 ⇒
	//     信息**更全**，重叠模型的取值与 /v2 **15/15 一致**（仅
	//     hy4-preview 不同，见 models.go 的说明）。
	//   · /v3 **没有** modelPromotions ⇒ 活动信息仍只能取 /v2。
	//   · /v3 的 agents.cli 只列 21/22 条，**漏掉 deepseek-v4.1-flash-sg**
	//     ⇒ 对 /v3 **绝不能套用 agents 裁剪**，否则会把它砍掉。
	PathV3Config = "/v3/config"

	// PathChat 对话。
	//
	// ⚠️ 上游强制 stream:true；发 stream:false 会返回 400。
	PathChat = "/v2/chat/completions"

	// PathUserResource 额度查询。
	//
	// ⚠️ 是 POST 且必须带 body，否则 404。
	PathUserResource = "/v2/billing/meter/get-user-resource"

	// PathDailyCheckin 每日签到。
	//
	// ⚠️ 「已签到」返回 HTTP 400 + 空 body，属于成功幂等，不是错误。
	PathDailyCheckin = "/v2/billing/meter/daily-checkin"

	// PathTokenRefresh token 刷新。
	//
	// ⚠️ 这是【唯一】允许携带 X-Refresh-Token 的端点。
	PathTokenRefresh = "/v2/plugin/auth/token/refresh"

	// PathLoginAccount 查询"当前 token 是谁"。
	//
	// 用途：网页登录拿到的凭据**只有 token、没有 uid**，而 uid 是本项目
	// 账号结构的主键（且用于派生 X-User-Id）。落盘前必须用它换出 uid。
	//
	// 实测（2026-10-05，真实账号）：
	//   GET + Authorization: Bearer <accessToken>  → HTTP 200
	//   {"code":0,"data":{"uid":"…","nickname":"…","phoneNumber":"…",...}}
	//
	// ⚠️ 这是**实测的站点接口**，不是标准 OIDC userinfo 端点，
	//    也不能称为标准 token endpoint —— 它可能随上游变更。
	PathLoginAccount = "/v2/plugin/login/account"
)
