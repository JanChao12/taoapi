package responses

import (
	"encoding/json"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件实现非流式出站：canonical 事件 → 一条 Responses Response 对象。
//
// ⚠️ 与 anthropic 包同样的取舍：output item 用 map[string]any 而不是
//	带 omitempty 的结构体。理由：各 item 类型的**必填字段不同** ——
//	reasoning item 有 summary，message item 有 content，function_call item
//	有 call_id/name/arguments。用结构体 + omitempty 会陷入两难
//	（本项目已因同类陷阱踩过坑：见 openai.ToolCall.Index 的注释）。

// Usage 是 Responses 的用量形状。
//
// ⚠️ 字段名与 Chat 相同（input_tokens/output_tokens/total_tokens），
//
//	但多一层 details 结构。这里刻意只输出**上游真的给了**的字段。
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`

	// InputTokensDetails 输入明细（缓存命中）。
	InputTokensDetails *InputTokensDetails `json:"input_tokens_details,omitempty"`

	// OutputTokensDetails 输出明细（思考 token）。
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// InputTokensDetails 输入 token 明细。
type InputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// OutputTokensDetails 输出 token 明细。
type OutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// Response 是一条 Responses API 的响应对象（非流式）。
type Response struct {
	ID     string `json:"id"`
	Object string `json:"object"`

	// CreatedAt 创建时间（Unix 秒）。
	CreatedAt int64 `json:"created_at"`

	// Status 见 StatusCompleted / StatusIncomplete / StatusFailed。
	Status string `json:"status"`

	// Model 客户端请求的模型名（不是上游 ID）。
	Model string `json:"model"`

	// Output 输出 item 列表。
	Output []map[string]any `json:"output"`

	// IncompleteDetails 仅当 status=incomplete 时出现。
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`

	// Usage 用量；上游没给时为 nil（不输出该字段）。
	Usage *Usage `json:"usage,omitempty"`
}

// IncompleteDetails 说明为什么未完成。
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// accTool 是累积中的一个函数调用。
type accTool struct {
	id   string
	name string
	args string
}

// toolAccumulator 按 index 累积工具调用分片。
//
// 🔴 必须按 index 归并：上游把一次调用的 id/name/arguments 拆成多个分片，
//
//	index 是唯一的归并依据。本项目 OpenAI 路径曾因丢 index 让 DSH 收到
//	267 个"独立"工具调用（见 openai.ToolCall 的注释）。
type toolAccumulator struct {
	m     map[int]*accTool
	order []int
}

// Add 吸收一个工具调用分片。
func (t *toolAccumulator) Add(d *provider.ToolCallDelta) {
	if d == nil {
		return
	}
	if t.m == nil {
		t.m = make(map[int]*accTool)
	}
	e, ok := t.m[d.Index]
	if !ok {
		e = &accTool{}
		t.m[d.Index] = e
		t.order = append(t.order, d.Index)
	}
	if d.ID != "" {
		e.id = d.ID
	}
	e.name += d.Name
	e.args += d.Arguments
}

// List 按首次出现的 index 顺序返回。
func (t *toolAccumulator) List() []*accTool {
	if t == nil {
		return nil
	}
	out := make([]*accTool, 0, len(t.order))
	for _, idx := range t.order {
		out = append(out, t.m[idx])
	}
	return out
}

// Len 返回累积到的调用数。
func (t *toolAccumulator) Len() int {
	if t == nil {
		return 0
	}
	return len(t.order)
}

// Aggregator 把 canonical 事件聚合为一条 Responses Response。
type Aggregator struct {
	upstreamID string
	model      string

	reasoning strings.Builder
	content   strings.Builder
	tools     *toolAccumulator

	usage  *provider.Usage
	finish string
}

// NewAggregator 创建聚合器。
func NewAggregator(upstreamID, model string) *Aggregator {
	return &Aggregator{
		upstreamID: upstreamID,
		model:      model,
		tools:      &toolAccumulator{},
	}
}

// Add 吸收一个 canonical 事件。
func (a *Aggregator) Add(ev provider.Event) {
	switch ev.Type {
	case provider.EventReasoning:
		a.reasoning.WriteString(ev.Text)
	case provider.EventContent:
		a.content.WriteString(ev.Text)
	case provider.EventToolCall:
		a.tools.Add(ev.ToolCall)
	case provider.EventUsage:
		if ev.Usage != nil {
			a.usage = ev.Usage
		}
	case provider.EventDone:
		if ev.FinishReason != "" {
			a.finish = ev.FinishReason
		}
	}
}

// Usage 返回上游给的用量；从未收到时为 nil。
func (a *Aggregator) Usage() *provider.Usage { return a.usage }

// Result 产出非流式响应。
func (a *Aggregator) Result() Response {
	output := make([]map[string]any, 0, 3)

	// 思考项必须在消息项**之前**（顺序与流式路径一致）。
	//
	// 🔴 为什么非流式也要下发思考（与 anthropic 同一个理由）：
	//	部分思考模型只输出 reasoning_content 而无 content ——
	//	不下发思考项，客户端会收到一个**空的 output**。
	if rc := a.reasoning.String(); strings.TrimSpace(rc) != "" {
		output = append(output, map[string]any{
			"id":     ReasoningItemID(a.upstreamID),
			"type":   ItemReasoning,
			"status": StatusCompleted,
			"summary": []any{
				map[string]any{"type": "summary_text", "text": rc},
			},
		})
	}

	if text := a.content.String(); strings.TrimSpace(text) != "" {
		output = append(output, map[string]any{
			"id":     MessageItemID(a.upstreamID),
			"type":   ItemMessage,
			"status": StatusCompleted,
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": ItemOutputText, "text": text, "annotations": []any{}},
			},
		})
	}

	for _, tc := range a.tools.List() {
		output = append(output, map[string]any{
			"id":        FunctionCallItemID(a.upstreamID),
			"type":      ItemFunctionCall,
			"status":    StatusCompleted,
			"call_id":   firstNonEmpty(tc.id, CallID(a.upstreamID)),
			"name":      tc.name,
			"arguments": firstNonEmpty(tc.args, "{}"),
		})
	}

	// 🔴 status 必须如实反映是否被截断（与 anthropic 的 stop_reason 缺陷同类）。
	//
	//	被 max_output_tokens 截断的响应若标成 completed，客户端会把它
	//	当完整回答继续跑（agent 会拿着半截结果往下走）。
	status := StatusCompleted
	var incomplete *IncompleteDetails
	if reason := IncompleteReason(a.finish); reason != "" {
		status = StatusIncomplete
		incomplete = &IncompleteDetails{Reason: reason}
	}

	return Response{
		ID:                ResponseID(a.upstreamID),
		Object:            "response",
		CreatedAt:         nowUnix(),
		Status:            status,
		Model:             a.model,
		Output:            output,
		IncompleteDetails: incomplete,
		Usage:             usageOf(a.usage),
	}
}

// usageOf 把 canonical 用量转为 Responses 用量。
//
// 返回 nil 表示上游没给用量 —— 调用方据此不输出 usage 字段。
// 🔴 绝不填 0：0 在语义上表示"确实消耗 0 token"，那是伪造数据。
func usageOf(u *provider.Usage) *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	// 只在真的有值时才输出 details（避免给客户端一堆恒为 0 的字段）
	if u.PromptCacheHitTokens > 0 {
		out.InputTokensDetails = &InputTokensDetails{CachedTokens: u.PromptCacheHitTokens}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &OutputTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// ParseArgs 把函数参数字符串原样返回（Responses 的 arguments 就是字符串）。
//
// ⚠️ 与 Anthropic 的**关键差异**：Anthropic 的 tool_use.input 必须是
//
//	**对象**（所以那边要 ParseArgsObject 包一层），而 Responses 的
//	function_call.arguments 是 **JSON 字符串**，与 Chat 一致 —— 原样透传即可。
//	搞反了会让客户端解析失败。
func ParseArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// mustJSON 序列化（失败时返回固定错误串，供流式帧使用）。
func mustJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
