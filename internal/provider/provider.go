// Package provider 定义渠道适配的最小接口。
//
// 设计原则（经 DSH × Codex 确认）：
//   - provider 层【不感知 HTTP 路径】；协议层【不嵌入厂商逻辑】。
//   - 能力不足时快速失败并返回明确错误，绝不静默丢字段。
//   - 新增渠道 = 新增一个实现包，不改公共层。
package provider

import (
	"context"
	"errors"
	"fmt"
)

// Capabilities 声明一个模型的能力。
//
// 这些值来自上游目录，但【档位是否为真旋钮】必须由实测校正确认
// （见 docs/upstream-contract.md 的"三模式"）。
type Capabilities struct {
	// ContextWindow 上下文长度（上游 maxInputTokens，取最大值而非默认档）。
	ContextWindow int64

	// MaxOutputTokens 输出上限（上游 maxOutputTokens）。
	MaxOutputTokens int64

	// SupportsImages 是否接受图片输入。
	SupportsImages bool

	// SupportsTools 是否支持工具调用。
	SupportsTools bool

	// Reasoning 思考档位信息；不支持思考时为 nil。
	Reasoning *ReasoningCapability
}

// ReasoningMode 描述某模型的档位是"真旋钮"还是"开关"。
//
// 实测结论（直连，n=6/档）：
//   - space-bunny          → ModeKnob，5 档单调递增
//   - glm-5.3              → ModeKnobFlat，max 档突变，低档几乎无差异
//   - deepseek-v4.1-flash  → ModeSwitch，各档无差异，但"不传"= 完全不思考
type ReasoningMode string

const (
	// ModeKnob 档位真实生效且单调（如 space-bunny）。
	ModeKnob ReasoningMode = "knob"

	// ModeKnobFlat 仅高档位有显著效果（如 glm-5.3 的 max）。
	ModeKnobFlat ReasoningMode = "knob-flat"

	// ModeSwitch 档位高低无差异，但传/不传决定是否思考（如 deepseek 系）。
	//
	// ⚠️ 对这类模型"不传档位"= 完全不思考，因此服务端【必须】
	// 在用户未指定时显式注入默认档位。
	ModeSwitch ReasoningMode = "switch"

	// ModeNone 不支持思考。
	ModeNone ReasoningMode = "none"
)

// ReasoningCapability 描述一个模型的思考档位。
type ReasoningCapability struct {
	// Mode 档位类型。
	Mode ReasoningMode

	// Levels 对外暴露的档位（已按实测裁剪，不是照抄上游声明）。
	// 顺序为升序。空切片表示不支持档位选择。
	Levels []string

	// Default 用户未指定时注入的档位。
	// ModeSwitch / ModeKnob / ModeKnobFlat 均应有值。
	Default string

	// Verified 是否经过实测验证。
	// false 表示 Levels 来自上游声明而非我们实测，仅供参考。
	Verified bool
}

// Model 是暴露给客户端的一个模型。
type Model struct {
	// ID 对外模型 ID（含渠道前缀，如 workbuddy/space-bunny）。
	ID string

	// UpstreamID 上游真实模型 ID（如 space-bunny）。
	UpstreamID string

	// Name 显示名。
	Name string

	// Capabilities 能力声明。
	Capabilities Capabilities

	// Pricing 计费信息（倍率）。
	//
	// ⚠️ 各平台口径不同，甚至可能不提供 —— 用指针表达"未知"，
	// 不要把"没返回"伪装成"免费"（那会误导用户以为不花钱）。
	Pricing *Pricing

	// Badge 上游给的运营标签（如「限时免费」「夜间折扣」）。
	//
	// ⚠️ 这是**简写**来源（tags 里的 `badge:文案:#色值`）。
	//	权威来源是 Promotion（见下）—— 它带开关与时间窗。
	//	有 Promotion 时前端应优先用它。
	Badge *Badge

	// Promotion 当前生效的运营活动（2026-10-06 加）。
	//
	// 🔴 为什么需要它（委托方质问"为什么你拿不到这些信息"）：
	//
	//	我们此前只读 tags 里的 badge 简写，**完全没读上游的
	//	`modelPromotions` 字段** —— 那才是权威来源，带：
	//	  · enabled 开关（活动可能被下掉）
	//	  · schedule 时间窗（活动会过期，简写不会）
	//	  · discount.factor（0 = 免费，0.5 = 五折）
	//	  · hover.textZh（活动说明）
	//
	//	⇒ 用简写会显示一个**已过期**的"免费"标签，误导用户。
	Promotion *Promotion
}

// Promotion 是一条**当前生效**的运营活动。
type Promotion struct {
	// ID 上游的活动 ID（如 "hy3-free-trial-202608"），便于排查。
	ID string

	// Kind 活动类型（实测 "discount"）。
	Kind string

	// Label 徽标文案（如 "Free now" / "限时免费"）。
	Label string

	// Color 上游给的颜色（已做安全校验，可能为空）。
	Color string

	// Free 是否**完全免费**（折扣系数为 0）。
	//
	// ⚠️ 与"没有折扣信息"完全不同：false 可能是"有折扣但不免费"
	//	（如五折），所以不能用 Factor==0 之外的方式推断。
	Free bool

	// Factor 折扣系数（0=免费，0.5=五折，1=无折扣）。
	Factor float64

	// Note 活动说明文案（上游的 hover.textZh）。
	Note string

	// ValidUntil 活动截止时间（RFC3339，可能为空）。
	ValidUntil string

	// Priority 同模型多条活动时的优先级。
	Priority int
}

// Pricing 描述模型的计费倍率。
type Pricing struct {
	// Multiplier 倍率，如 0.03 表示 x0.03。
	Multiplier float64

	// HasMultiplier 上游是否真的返回了倍率。
	//
	// 🔴 为什么要与 Multiplier 分开：0 是**合法倍率**（免费），
	// 而"没返回"是**未知**。两者含义完全相反，
	// 混用会把"上游没标价"显示成"免费"。
	HasMultiplier bool
}

// Badge 是上游下发的运营标签。
type Badge struct {
	// Text 标签文案（如「限时免费」）。
	Text string

	// Color 上游指定的颜色（如 "#FF0000"）。
	//
	// ⚠️ 直接用上游给的值：那是运营侧刻意选的颜色（红=促销、蓝=折扣），
	// 自己另配一套会和官方客户端不一致，用户会觉得"和客户端说的不一样"。
	Color string
}

// ChatRequest 是发往上游的一次对话请求（canonical 形式）。
type ChatRequest struct {
	// Model 上游模型 ID（不含前缀）。
	Model string

	// RawBody 已经过协议层规范化的请求体（OpenAI Chat 形状）。
	// provider 负责做厂商特有的改写（如强制 stream、role 归一）。
	RawBody []byte

	// Stream 客户端是否要求流式。
	// ⚠️ 注意：即使为 false，provider 也必须向上游发 stream:true
	// 并自行聚合——上游拒绝非流式。
	Stream bool

	// ReasoningEffort 客户端指定的思考档位；
	// 空字符串表示未指定，provider 按模型策略注入默认值。
	ReasoningEffort string
}

// Provider 是渠道适配的最小接口。
type Provider interface {
	// ID 渠道标识（如 "workbuddy"）。
	ID() string

	// Models 拉取可用模型列表。
	Models(ctx context.Context) ([]Model, error)

	// Chat 发起对话，逐事件回调 emit。
	//
	// emit 返回错误时应立即停止读取上游并返回该错误
	// （通常是客户端断开）。
	Chat(ctx context.Context, req ChatRequest, emit func(Event) error) error

	// Checkin 执行每日签到。
	Checkin(ctx context.Context, account string) (CheckinResult, error)

	// Credit 查询额度。
	Credit(ctx context.Context, account string) (CreditResult, error)
}

// EventType 流事件类型。
type EventType string

const (
	// EventReasoning 思考增量（reasoning_content）。
	EventReasoning EventType = "reasoning"

	// EventContent 正文增量（content）。
	EventContent EventType = "content"

	// EventToolCall 工具调用增量。
	EventToolCall EventType = "tool_call"

	// EventUsage 用量（通常出现在末帧）。
	EventUsage EventType = "usage"

	// EventDone 流正常结束。
	EventDone EventType = "done"
)

// Event 是从上游流解析出的一个事件（canonical 形式）。
type Event struct {
	Type EventType

	// Text 增量文本（EventReasoning / EventContent）。
	Text string

	// ToolCall 工具调用增量（EventToolCall）。
	ToolCall *ToolCallDelta

	// Usage 用量（EventUsage）。
	Usage *Usage

	// FinishReason 结束原因（EventDone 时可能有值）。
	FinishReason string
}

// ToolCallDelta 工具调用增量。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Usage 用量统计。字段来自上游，未知时保持零值/指针为 nil。
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64

	// ReasoningTokens 思考消耗（上游 completion_thinking_tokens
	// 或 completion_tokens_details.reasoning_tokens）。
	ReasoningTokens int64

	// Credit 上游返回的额度消耗。nil 表示上游未提供（不要用 0 冒充）。
	Credit *float64

	// ── 缓存（2026-10-06 补）──
	//
	// 🔴 之前**整条链路都缺这两个字段**，导致面板的缓存命中率恒为空：
	//	上游 SSE 解析器（workbuddy/sse.go）已经读到了它们，
	//	但 toProviderUsage 没往下传，canonical Usage 也没有这两个字段，
	//	于是记账器拿不到 → 写进 JSONL 的永远是 0。
	//
	//	这是"字段在两端都有、中间断了"的典型 —— 有测试从上游解析到
	//	JSONL 全链路都覆盖不到，所以一直没暴露。
	//
	// ⚠️ 语义：命中率 = hit ÷ (hit + miss)。两者都是 0 时表示
	//	**上游没报告缓存数据**，与"命中率 0%"含义不同（后者是 miss>0）。
	PromptCacheHitTokens  int64
	PromptCacheMissTokens int64
}

// CheckinResult 签到结果。
type CheckinResult struct {
	// AlreadyCheckedIn 今日已签到。
	//
	// ⚠️ 上游对"已签到"返回 HTTP 400 + 空 body。
	// 这不是失败，必须与本字段对应，且不计入错误冷却。
	AlreadyCheckedIn bool

	// Message 上游返回的说明。
	Message string

	// Credits 签到后额度（若上游返回）。
	Credits *int64
}

// CreditResult 额度查询结果。
type CreditResult struct {
	// Remaining 剩余额度（TotalDosage 语义）。
	Remaining *int64

	// Accounts 各套餐明细。
	Accounts []CreditAccount

	// Raw 上游原始响应（已脱敏），便于排障。
	Raw map[string]any
}

// CreditAccount 单个套餐额度。
type CreditAccount struct {
	PackageName string
	Remain      int64
	Used        int64
	Size        int64

	// ExpireAt 该额度包到期日（YYYY-MM-DD，UTC+8 墙钟）。
	//
	// ⭐ 取自上游 CycleEndTime —— 实测确认这是积分到期判据
	//（DeductionEndTime 是账单扣减窗口，可能是很多年后）。
	// 空串表示上游未下发，【不得】用零值冒充"永不过期"。
	ExpireAt string
}

// 常见错误。
var (
	// ErrNotImplemented 该 provider 尚未实现此能力。
	ErrNotImplemented = errors.New("provider: 能力未实现")

	// ErrUnsupportedReasoning 请求了该模型不支持的思考档位。
	//
	// 按约定：不能静默降档，必须返回明确错误。
	ErrUnsupportedReasoning = errors.New("provider: 该模型不支持所请求的思考档位")

	// ErrAuthExpired 凭证失效，需要重新授权。
	ErrAuthExpired = errors.New("provider: 凭证已失效，需要重新授权")
)

// UnsupportedReasoningError 带上下文的档位不支持错误。
type UnsupportedReasoningError struct {
	Model    string
	ModelID  string
	Asked    string
	Levels   []string
	WantMode ReasoningMode
}

func (e *UnsupportedReasoningError) Error() string {
	return fmt.Sprintf(
		"模型 %s 不支持思考档位 %q（该模型模式=%s，可用档位=%v）",
		e.ModelID, e.Asked, e.WantMode, e.Levels,
	)
}

func (e *UnsupportedReasoningError) Unwrap() error { return ErrUnsupportedReasoning }
