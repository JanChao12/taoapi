package anthropic

import (
	"encoding/json"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件实现非流式出站：把 canonical 事件聚合为一条 Anthropic Message。
//
// ⚠️ 为什么 content block 用 map[string]any 而不是带 omitempty 的结构体：
//
//	Anthropic 各块类型的**必填字段不同** —— thinking 块必须有 signature
//	（可以为空串），text 块**不能**有 signature，tool_use 块必须有 input。
//	用带 omitempty 的结构体表达会陷入两难：给 signature 加 omitempty 会让
//	thinking 块丢掉必填的 signature；不加又会让 text 块多出一个空 signature。
//	本项目已因同类 omitempty 陷阱踩过坑（见 openai.ToolCall.Index 的注释：
//	值类型配 omitempty 会把合法的索引 0 一起吃掉）。
//	⇒ 用 map 精确控制每个块出现哪些键。

// Message 是一条 Anthropic 消息（非流式响应）。
type Message struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []map[string]any `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`

	// Usage 字段名与 OpenAI 不同（input_tokens / output_tokens）。
	Usage Usage `json:"usage"`
}

// Usage 是 Anthropic 形状的用量。
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	// CacheReadInputTokens 命中缓存的输入 token（Anthropic 正式字段）。
	//
	// ⚠️ 用 omitempty：上游没报告缓存数据时值为 0，此时**不输出该字段**
	//	比输出 0 更准确 —— 0 在 Anthropic 语义里表示"确实没有命中缓存"，
	//	而"上游没报告"是另一回事（见 provider.Usage 的说明）。
	CacheReadInputTokens int64 `json:"cache_read_input_tokens,omitempty"`
}

// MessageID 由上游响应 id 生成 Anthropic 风格的 msg_ id。
//
// 上游 id 形如 chatcmpl-1730000000000000000 ⇒ msg_1730000000000000000。
// 保留上游的随机部分便于把一次对话与上游日志对上。
func MessageID(upstreamID string) string {
	s := strings.TrimSpace(upstreamID)
	if s == "" {
		return "msg_" + randSuffix()
	}
	if i := strings.Index(s, "-"); i >= 0 && i+1 < len(s) {
		return "msg_" + s[i+1:]
	}
	return "msg_" + s
}

// accTool 是累积中的一个工具调用。
type accTool struct {
	id   string
	name string
	args string
}

// toolAccumulator 按 index 累积工具调用分片。
//
// 🔴 为什么必须按 index 累积（不是按到达顺序）：
//
//	上游把一次工具调用的 id/name/arguments 拆成多个分片下发，
//	index 是把它们归并成"同一次调用"的唯一依据。
//	本项目的 OpenAI 路径曾因丢 index 让 DSH 收到 267 个"独立"工具调用
//	（见 openai.ToolCall 的注释）—— 这里不能重犯。
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
	// 名称与参数都是分片下发的，必须拼接
	e.name += d.Name
	e.args += d.Arguments
}

// List 按首次出现的 index 顺序返回累积结果。
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

// Len 返回累积到的工具调用数。
func (t *toolAccumulator) Len() int {
	if t == nil {
		return 0
	}
	return len(t.order)
}

// ParseArgsObject 把工具参数 JSON 字符串解析为对象。
//
// 🔴 Anthropic 的 tool_use.input 必须是**对象**（不能是数组/标量/null）。
//
//	上游偶尔会下发非法 JSON（截断、空串）。此时用 _raw 包住原文 ——
//	既不丢信息，也保证形状合法，客户端不会因为解不出来而整体失败。
func ParseArgsObject(args string) map[string]any {
	s := strings.TrimSpace(args)
	if s == "" {
		return map[string]any{}
	}
	var obj any
	if json.Unmarshal([]byte(s), &obj) == nil {
		if m, ok := obj.(map[string]any); ok {
			return m
		}
		// 合法 JSON 但不是对象（如数组）→ 包一层保住内容
		return map[string]any{"value": obj}
	}
	return map[string]any{"_raw": s}
}

// Aggregator 把 canonical 事件聚合为一条 Anthropic Message。
//
// 上游强制 stream:true，所以【非流式响应必须自己聚合】——
// 与 OpenAI 路径同一个道理（见 openai.Aggregator）。
type Aggregator struct {
	id    string
	model string

	reasoning strings.Builder
	content   strings.Builder
	tools     *toolAccumulator

	usage  *provider.Usage
	finish string
}

// NewAggregator 创建聚合器。
func NewAggregator(upstreamID, model string) *Aggregator {
	return &Aggregator{
		id:    MessageID(upstreamID),
		model: model,
		tools: &toolAccumulator{},
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
//
// 返回 nil 而不是零值是有意的 —— 调用方据此区分"上游确实返回 0"
// 与"上游压根没给"，后者不能当 0 记账（见 usage.Event.UsageKnown）。
func (a *Aggregator) Usage() *provider.Usage { return a.usage }

// Result 产出非流式 Anthropic 消息。
func (a *Aggregator) Result() Message {
	content := make([]map[string]any, 0, 3)

	// 思考块必须在文本块【之前】（块顺序与流式路径一致）。
	//
	// 🔴 为什么非流式也必须下发思考块（参考实现提醒）：
	//	部分思考模型只输出 reasoning_content 而无 content ——
	//	不下发 thinking 块，客户端会收到一条**完全空**的消息。
	if rc := a.reasoning.String(); strings.TrimSpace(rc) != "" {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": rc, "signature": "",
		})
	}

	if text := a.content.String(); strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}

	tools := a.tools.List()
	for _, tc := range tools {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(tc.id, "toolu_"+randSuffix()),
			"name":  tc.name,
			"input": ParseArgsObject(tc.args),
		})
	}

	u := Usage{}
	if a.usage != nil {
		u.InputTokens = a.usage.PromptTokens
		u.OutputTokens = a.usage.CompletionTokens
		u.CacheReadInputTokens = a.usage.PromptCacheHitTokens
	}

	return Message{
		ID:           a.id,
		Type:         "message",
		Role:         "assistant",
		Model:        a.model,
		Content:      content,
		StopReason:   StopReason(a.finish, len(tools) > 0),
		StopSequence: nil,
		Usage:        u,
	}
}
