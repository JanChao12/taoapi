// Package openai 定义对外的 OpenAI 兼容协议形状。
//
// 设计要点（经 DSH × Codex 确认）：
//   - /v1/models 里 DSK【只读】id / name / contextWindow / maxTokens 四个字段，
//     档位必须靠 DSH 侧配置声明。因此我们在标准字段之外【额外】暴露
//     reasoning_* 扩展字段（DSH 忽略，其他客户端与我们的面板可用）。
//   - context_length 取上游最大值，不是默认档。
package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Model 是 /v1/models 里的单个模型条目。
//
// 字段分两组：
//
//	标准字段（DSH 会读）  : ID / Object / OwnedBy / Name / ContextLength / MaxOutputTokens / Architecture
//	扩展字段（DSH 忽略）  : ReasoningEfforts / ReasoningDefault / ReasoningVerified / ReasoningMode
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Name    string `json:"name,omitempty"`

	// ContextLength 上下文长度。
	// ⚠️ 取上游 maxInputTokens（最大值），不是 contextWindow.defaultLength（300000）。
	ContextLength int64 `json:"context_length,omitempty"`

	// MaxOutputTokens 输出上限。
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`

	// Architecture 多模态能力。DSH 用它判断能否发图片。
	Architecture *Architecture `json:"architecture,omitempty"`

	// ── 扩展字段（DSH 不读，但其他客户端/我们的面板会读）──

	// ReasoningEfforts 可用思考档位，升序；空表示不支持档位选择。
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`

	// ReasoningDefault 用户未指定时服务端注入的档位。
	ReasoningDefault string `json:"reasoning_default,omitempty"`

	// ReasoningVerified 档位是否经过实测验证（false = 仅上游声明）。
	ReasoningVerified bool `json:"reasoning_verified"`

	// ReasoningMode 档位类型：knob / knob-flat / switch / none。
	ReasoningMode string `json:"reasoning_mode,omitempty"`

	// ── 计费（面板展示用；标准 OpenAI 客户端会忽略未知字段）──

	// Credits 计费倍率，如 0.03 表示 x0.03。
	//
	// ⚠️ 用指针：nil = **上游未返回定价**（面板显示 unknown），
	// 0 = 明确免费（面板显示 Free）。两者含义相反，不可混用。
	Credits *float64 `json:"credits,omitempty"`

	// Badge 上游运营标签（如「限时免费」「夜间折扣」）。
	//
	// ⚠️ 这是 tags 里的**简写**。若 Promotion 存在，前端应优先用那个
	//	（它带时间窗，能避免显示已过期的活动）。
	Badge *ModelBadge `json:"badge,omitempty"`

	// Promotion 当前生效的运营活动（2026-10-06 加）。
	//
	// 🔴 与 Badge 的区别：Badge 是静态简写，Promotion 带
	//	enabled 开关、时间窗、折扣系数与说明文案 —— 权威来源。
	Promotion *ModelPromotion `json:"promotion,omitempty"`
}

// ModelPromotion 是一条当前生效的运营活动。
type ModelPromotion struct {
	// Label 徽标文案（如 "Free now"）。
	Label string `json:"label,omitempty"`

	// Color 上游给的颜色（已校验）。
	Color string `json:"color,omitempty"`

	// Free 是否完全免费（折扣系数为 0）。
	//
	// ⚠️ 与"没有折扣"完全不同，前端据此显示 Free 徽标。
	Free bool `json:"free,omitempty"`

	// Note 活动说明（上游 hover 文案）。
	Note string `json:"note,omitempty"`

	// ValidUntil 活动截止时间（RFC3339，可能为空）。
	ValidUntil string `json:"valid_until,omitempty"`
}

// ModelBadge 是上游下发的运营标签（文案 + 颜色都由上游决定）。
type ModelBadge struct {
	// Text 标签文案。
	Text string `json:"text"`

	// Color 上游指定的颜色（如 "#FF0000"）；可能为空。
	Color string `json:"color,omitempty"`
}

// Architecture 描述模型的模态能力。
type Architecture struct {
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	Modality         string   `json:"modality,omitempty"`
}

// ModelList 是 /v1/models 的响应。
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// NewModelList 构造模型列表响应。
func NewModelList(models []Model) ModelList {
	if models == nil {
		models = []Model{}
	}
	return ModelList{Object: "list", Data: models}
}

// ErrorResponse 是错误响应（OpenAI 兼容形状）。
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是错误详情。
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// 常见错误类型（与 OpenAI 约定一致）。
const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuth           = "authentication_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeServer         = "server_error"
	ErrTypeUpstream       = "upstream_error"
)

// NewError 构造错误响应。
func NewError(typ, code, message string) ErrorResponse {
	return ErrorResponse{Error: ErrorDetail{
		Message: message,
		Type:    typ,
		Code:    code,
	}}
}

// ─────────────────────────────────────────────────────────────
// Chat 协议
// ─────────────────────────────────────────────────────────────

// ChatMessage 是一条对话消息。
//
// 注意：ReasoningContent 是 OpenAI 规范外的字段（DeepSeek 系风格），
// 上游会返回，我们必须原样透传，否则客户端看不到思考过程。
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// ReasoningContent 思考内容（非标准字段，必须透传）。
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// Name 可选。
	Name string `json:"name,omitempty"`

	// ToolCalls 工具调用（助手消息）。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID 工具结果消息对应的调用 ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// InboundMessage 是【入站】消息，与 ChatMessage 的区别只在 content 的宽容度。
//
// 🔴 为什么需要它（2026-10-05 真实故障）：
// OpenAI 兼容客户端（DSH 即如此）会把 content 发成【多模态数组】：
//
//	{"role":"user","content":[{"type":"text","text":"hi"}]}
//
// 而 ChatMessage.Content 是 string，解数组直接报
// `cannot unmarshal array into Go struct field ...content of type string`。
// 出站仍用 ChatMessage（我们只产生字符串内容），入站用这个宽类型。
type InboundMessage struct {
	Role       string        `json:"role"`
	Content    StringOrParts `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

// StringOrParts 兼容 content 的两种合法形状：字符串，或多模态分片数组。
//
// 两种都接受，统一取出纯文本用于校验；原始形状由 app 层从原始字节透传，
// 不会被这里的有损转换影响（见 app/chat.go 的 RawBody 构造）。
type StringOrParts struct {
	// Text 是抽出来的纯文本（数组形态则拼接所有 text 分片）。
	Text string

	// OK 表示解析成功。content 缺失/为 null 时为 true 且 Text 为空。
	OK bool
}

// UnmarshalJSON 实现两种形状的兼容解析。
func (s *StringOrParts) UnmarshalJSON(b []byte) error {
	s.OK = true
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		s.Text = ""
		return nil
	}
	// 形状一：字符串
	if trimmed[0] == '"' {
		return json.Unmarshal(b, &s.Text)
	}
	// 形状二：数组（多模态分片）。只取 type=text 的 text 字段拼起来，
	// 其他分片（如 image_url）不影响请求合法性，忽略即可。
	if trimmed[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
		}
		s.Text = sb.String()
		return nil
	}
	// 其他形状：不报错，交给上游判定（我们不做比上游更严的校验）。
	s.Text = ""
	return nil
}

// ToolCall 是一次工具调用。
//
// 🔴 为什么 Index 必须存在（2026-10-05 真实故障）：
// index 是 OpenAI【流式】规范里 tool_calls 分片的必需字段 —— 客户端靠它
// 把同一次调用的多个增量拼成一条。缺了它，客户端只能把每个增量当成
// 一次【独立】工具调用。
//
// 实测后果：DSH 通过本代理要工具时，收到 267 个"独立"工具调用，其中 265 个
// id/name 为空（参数碎片全部散落进这些空调用里，连真调用的 arguments 都变空）。
// DSH 顺序执行到第一个空调用时，写出的 tool/result 因 toolCallId 为空被
// v4 格式校验拒绝，会话从此永久卡死：
//
//	format v4 tool/result at seq 1096 requires toolCallId matching its tool source
//
// 用【指针 + omitempty】而不是值类型：非流式响应与入站请求不该带 index，
// 而值类型配 omitempty 会把合法的索引 0 一起吃掉 —— 那恰恰是最常见的情形。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
	Index    *int         `json:"index,omitempty"`
}

// ToolFunction 是工具调用的函数部分。
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletionRequest 是入站的对话请求。
type ChatCompletionRequest struct {
	Model    string           `json:"model"`
	Messages []InboundMessage `json:"messages"`
	Stream   bool             `json:"stream"`

	// ReasoningEffort 思考档位（客户端可指定）。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// MaxTokens 输出上限。
	MaxTokens int64 `json:"max_tokens,omitempty"`

	// MaxCompletionTokens 是新版别名；上游只认 max_tokens，需归一。
	MaxCompletionTokens int64 `json:"max_completion_tokens,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	// Tools 工具定义（透传）。
	Tools json_RawMessage `json:"tools,omitempty"`

	// ToolChoice 工具选择。⚠️ 上游要求是 string，传对象会 400。
	ToolChoice json_RawMessage `json:"tool_choice,omitempty"`

	// Stop 停止序列。
	Stop []string `json:"stop,omitempty"`

	// StreamOptions 流式选项（上游据此在末帧返回 usage）。
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions 流式选项。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatCompletionResponse 是非流式对话响应。
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice 是一个候选回复。
type Choice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason"`
}

// Usage 用量统计。
//
// 保留上游的思考计量字段，便于 report 统计"思考花了多少"。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// CompletionTokensDetails 明细（含 reasoning_tokens）。
	CompletionTokensDetails *TokenDetails `json:"completion_tokens_details,omitempty"`

	// CompletionThinkingTokens 上游直接给出的思考 token（实测字段）。
	CompletionThinkingTokens int64 `json:"completion_thinking_tokens,omitempty"`

	// Credit 上游返回的额度消耗。nil 表示上游未提供（不要用 0 冒充）。
	Credit *float64 `json:"credit,omitempty"`

	// 缓存相关（实测字段，透传）。
	PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens,omitempty"`
}

// TokenDetails 是 token 明细。
type TokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
}

// ChatCompletionChunk 是流式响应的一个分片。
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice 是流式分片里的候选。
type ChunkChoice struct {
	Index        int          `json:"index"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// json_RawMessage 是 json.RawMessage 的别名，用于"解析但不定型"的透传字段。
//
// 🔴 曾写成 `type json_RawMessage = []byte`，那是【错的】（2026-10-05 真实故障）：
// []byte 不实现 json.Unmarshaler，编码/json 会把 JSON 数组当成
// base64 字符串或字节数组去解，于是 `tools: [...]` 直接报
// `cannot unmarshal array into Go struct field ...tools of type uint8`。
// 必须是 json.RawMessage —— 它虽底层也是 []byte，但带有 UnmarshalJSON，
// 能把原始 JSON 原文存下来。注释当时已写"是 json.RawMessage 的别名"，
// 与代码不符，说明是笔误而非有意设计。
type json_RawMessage = json.RawMessage
