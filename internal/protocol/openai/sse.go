package openai

import (
	"encoding/json"
	"fmt"
)

// BuildChunk 把一个 canonical 事件转成对外 SSE 分片。
//
// 返回 nil 表示该事件不产生分片（如 usage 单独成帧时由调用方处理）。
//
// ⚠️ reasoning 走 delta.reasoning_content —— 这是 OpenAI 规范外的字段，
// 但必须原样透传，否则客户端看不到思考过程。
func BuildChunk(id, model string, created int64, ev Event) *ChatCompletionChunk {
	ch := &ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
	}
	delta := &ChatMessage{Role: ""}
	choice := ChunkChoice{Index: 0, Delta: delta}

	switch ev.Type {
	case EvReasoning:
		delta.ReasoningContent = ev.Text
	case EvContent:
		delta.Content = ev.Text
	case EvToolCall:
		if ev.ToolCall != nil {
			// index 是 OpenAI 流式规范的必需字段：客户端靠它把同一次调用的
			// 多个增量合并起来。漏掉会让客户端把每个增量当成独立调用 ——
			// 见 types.go 里 ToolCall.Index 的注释（2026-10-05 真实故障）。
			//
			// 这里必须取地址：索引 0 是合法值，值类型配 omitempty 会被吃掉。
			idx := ev.ToolCall.Index
			delta.ToolCalls = []ToolCall{{
				Index: &idx,
				ID:    ev.ToolCall.ID,
				Type:  "function",
				Function: ToolFunction{
					Name:      ev.ToolCall.Name,
					Arguments: ev.ToolCall.Arguments,
				},
			}}
		}
	case EvDone:
		fr := ev.FinishReason
		if fr == "" {
			fr = "stop"
		}
		choice.FinishReason = &fr
	case EvUsage:
		ch.Usage = toOpenAIUsage(ev.Usage)
		// usage 帧没有 choices 内容，但仍需一个空 delta 结构
		choice.Delta = nil
	default:
		return nil
	}

	ch.Choices = []ChunkChoice{choice}
	return ch
}

// toOpenAIUsage 转换用量。
func toOpenAIUsage(u *EventUsage) *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:             u.PromptTokens,
		CompletionTokens:         u.CompletionTokens,
		TotalTokens:              u.TotalTokens,
		CompletionThinkingTokens: u.ReasoningTokens,
		Credit:                   u.Credit,
	}
	if u.ReasoningTokens != 0 {
		out.CompletionTokensDetails = &TokenDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// MarshalChunk 把分片序列化为 SSE 行（含 data: 前缀与双换行）。
func MarshalChunk(chunk *ChatCompletionChunk) ([]byte, error) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil, fmt.Errorf("序列化分片失败: %w", err)
	}
	// SSE 帧格式：data: <json>\n\n
	out := make([]byte, 0, len(b)+10)
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, '\n', '\n')
	return out, nil
}

// DoneMarker 是流结束标记。
var DoneMarker = []byte("data: [DONE]\n\n")
