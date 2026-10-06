package openai

import (
	"strings"
)

// EventType 是 canonical 事件类型。
//
// 与 provider.EventType 对应，但定义在这里是为了让协议层
// 不依赖具体 provider 包（避免 provider ← protocol 反向依赖）。
type EventType string

const (
	EvReasoning EventType = "reasoning"
	EvContent   EventType = "content"
	EvToolCall  EventType = "tool_call"
	EvUsage     EventType = "usage"
	EvDone      EventType = "done"
)

// Event 是协议层看到的 canonical 事件。
type Event struct {
	Type         EventType
	Text         string
	ToolCall     *ToolCallDelta
	Usage        *EventUsage
	FinishReason string
}

// ToolCallDelta 是工具调用增量。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// EventUsage 是事件载荷里的用量（内部形式）。
//
// 单独命名是为了与 types.go 里对外序列化的 Usage 区分：
// 前者是 canonical，后者要按 OpenAI 字段名输出。
type EventUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ReasoningTokens  int64
	Credit           *float64

	// 缓存命中/未命中（2026-10-06 补）。
	//
	// 🔴 这两项之前**整条链路都缺**，导致面板缓存命中率恒为空：
	//	上游 SSE 解析器读到了 → canonical Usage 没字段 →
	//	这里没字段 → 记账器拿不到 → JSONL 永远写 0。
	//	补的时候要一次补齐三处，缺一处就还是空。
	PromptCacheHitTokens  int64
	PromptCacheMissTokens int64
}

// Aggregator 把流事件聚合为一次非流式响应。
//
// 上游强制 stream:true，所以【非流式响应必须自己聚合】。
// 这是本项目最关键的适配之一。
type Aggregator struct {
	id      string
	model   string
	created int64

	reasoning strings.Builder
	content   strings.Builder

	toolCalls map[int]*ToolCall
	toolOrder []int

	usage        *EventUsage
	finishReason string
}

// NewAggregator 创建聚合器。
func NewAggregator(id, model string, created int64) *Aggregator {
	return &Aggregator{
		id:        id,
		model:     model,
		created:   created,
		toolCalls: make(map[int]*ToolCall),
	}
}

// Add 吸收一个事件。
func (a *Aggregator) Add(ev Event) {
	switch ev.Type {
	case EvReasoning:
		a.reasoning.WriteString(ev.Text)
	case EvContent:
		a.content.WriteString(ev.Text)
	case EvToolCall:
		if ev.ToolCall == nil {
			return
		}
		idx := ev.ToolCall.Index
		tc, ok := a.toolCalls[idx]
		if !ok {
			tc = &ToolCall{Type: "function"}
			a.toolCalls[idx] = tc
			a.toolOrder = append(a.toolOrder, idx)
		}
		if ev.ToolCall.ID != "" {
			tc.ID = ev.ToolCall.ID
		}
		// 名称与参数都是分片下发的，必须拼接
		tc.Function.Name += ev.ToolCall.Name
		tc.Function.Arguments += ev.ToolCall.Arguments
	case EvUsage:
		a.usage = ev.Usage
	case EvDone:
		if ev.FinishReason != "" {
			a.finishReason = ev.FinishReason
		}
	}
}

// Usage 返回已吸收的上游用量；从未收到 usage 时返回 nil。
//
// 为什么需要这个访问器（2026-10-05 第 9 轮）：用量统计要写入的真实 token 数
// 只有这里知道。返回 nil 而不是零值是有意的 —— 调用方据此区分
// "上游确实返回了 0"与"上游压根没给 usage"，后者不能当 0 记账
// （见 usage.Event.UsageKnown 的说明）。
//
// 注意与 Result().Usage 的区别：后者是对外序列化形式，本方法返回内部形式。
func (a *Aggregator) Usage() *EventUsage {
	return a.usage
}

// Result 产出非流式响应。
//
// ⚠️ 注意 reasoning_content 必须保留 —— 它是 OpenAI 规范外字段，
// 但丢掉就等于让用户看不到思考过程。
func (a *Aggregator) Result() ChatCompletionResponse {
	msg := &ChatMessage{
		Role:             "assistant",
		Content:          a.content.String(),
		ReasoningContent: a.reasoning.String(),
	}
	if len(a.toolOrder) > 0 {
		for _, idx := range a.toolOrder {
			msg.ToolCalls = append(msg.ToolCalls, *a.toolCalls[idx])
		}
	}

	fr := a.finishReason
	if fr == "" {
		fr = "stop"
	}

	return ChatCompletionResponse{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: fr}},
		Usage:   toOpenAIUsage(a.usage),
	}
}
