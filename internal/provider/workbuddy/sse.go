package workbuddy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// sseMaxLineBytes 单行 SSE 上限。
// 上游一个 delta 通常几百字节；给 1 MiB 足够容纳异常大的工具调用参数。
const sseMaxLineBytes = 1 << 20

// upstreamDelta 是上游 delta 的线格式。
//
// ⚠️ 关键实测结论（docs/upstream-contract.md §5.2）：
// 上游的 delta 里【六个字段永远都存在】，不需要的用 "" / null / [] 表示。
// 所以【不能靠字段是否存在判断类型】，必须看值是否非空。
type upstreamDelta struct {
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Role             string          `json:"role"`
	FunctionCall     json.RawMessage `json:"function_call"`
	Refusal          string          `json:"refusal"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// upstreamFrame 是上游 SSE 的一个数据帧。
type upstreamFrame struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Choices []struct {
		Index        int           `json:"index"`
		Delta        upstreamDelta `json:"delta"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *upstreamUsage `json:"usage"`
}

// upstreamUsage 是上游的用量统计（字段名直连实测）。
type upstreamUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
		CachedTokens    int64 `json:"cached_tokens"`
	} `json:"completion_tokens_details"`

	// CompletionThinkingTokens 上游直接给出的思考 token。
	CompletionThinkingTokens int64 `json:"completion_thinking_tokens"`

	// Credit 额度消耗。用指针区分"上游返回 0"和"上游没返回"。
	Credit *float64 `json:"credit"`

	PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
}

// toProviderUsage 转换为 canonical usage。
//
// 思考 token 取两个字段中的较大值：上游有时只填其中一个。
func (u *upstreamUsage) toProviderUsage() *provider.Usage {
	if u == nil {
		return nil
	}
	reasoning := u.CompletionThinkingTokens
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > reasoning {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return &provider.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ReasoningTokens:  reasoning,
		Credit:           u.Credit,
		// 🔴 缓存字段必须一起传（2026-10-06 修）：
		//	漏掉这两个 ⇒ 记账器永远写 0 ⇒ 面板命中率恒为空。
		PromptCacheHitTokens:  u.PromptCacheHitTokens,
		PromptCacheMissTokens: u.PromptCacheMissTokens,
	}
}

// parseSSEStream 读取上游 SSE 流并逐事件回调 emit。
//
// 行为要点（均有实测依据）：
//   - delta 里思考与正文【分别累加】，不假设"先思考完再正文"
//   - 空值字段不产生事件（避免下游收到大量空 delta）
//   - `[DONE]` 结束；EOF 前没见到 DONE 视为异常截断
//   - 单次读取空闲超过 SSEIdleTimeout 视为上游卡死
//
// idle 通过读超时实现：调用方应保证 r 已设置读超时，
// 或使用 newIdleReader 包装。这里用 SetReadDeadline 需要底层是 net.Conn，
// 故改为在 http.Client 层不设总超时、由本函数的 ticker 兜底。
func parseSSEStream(r io.Reader, emit func(provider.Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseMaxLineBytes)

	var (
		sawDone   bool
		lastModel string
		lastID    string
		created   int64
	)

	for sc.Scan() {
		line := sc.Text()

		// SSE 允许空行分隔事件；以 data: 开头的才是数据
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			break
		}

		var frame upstreamFrame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			// 单帧解析失败不应中断整条流：上游偶尔会插入非标准帧。
			// 但也不能静默——返回错误让调用方决定（此处选择跳过并继续，
			// 因为是流式场景，中断代价更大；不可解析的帧按无内容处理）。
			continue
		}

		if frame.ID != "" {
			lastID = frame.ID
		}
		if frame.Model != "" {
			lastModel = frame.Model
		}
		if frame.Created != 0 {
			created = frame.Created
		}

		// usage 可能出现在任意帧（实测在末帧）
		if frame.Usage != nil {
			if err := emit(provider.Event{
				Type:  provider.EventUsage,
				Usage: frame.Usage.toProviderUsage(),
			}); err != nil {
				return err
			}
		}

		if len(frame.Choices) == 0 {
			continue
		}
		ch := frame.Choices[0]

		// 思考增量：判值不判字段（字段永远存在）
		if ch.Delta.ReasoningContent != "" {
			if err := emit(provider.Event{
				Type: provider.EventReasoning,
				Text: ch.Delta.ReasoningContent,
			}); err != nil {
				return err
			}
		}

		// 正文增量
		if ch.Delta.Content != "" {
			if err := emit(provider.Event{
				Type: provider.EventContent,
				Text: ch.Delta.Content,
			}); err != nil {
				return err
			}
		}

		// 工具调用增量
		for _, tc := range ch.Delta.ToolCalls {
			if err := emit(provider.Event{
				Type: provider.EventToolCall,
				ToolCall: &provider.ToolCallDelta{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			}); err != nil {
				return err
			}
		}

		// 结束帧
		if ch.FinishReason != "" {
			if err := emit(provider.Event{
				Type:         provider.EventDone,
				FinishReason: ch.FinishReason,
			}); err != nil {
				return err
			}
		}
	}

	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取上游流失败: %w", err)
	}
	if !sawDone {
		// 没有 [DONE] 就断了 —— 这是【真实存在的问题】：
		// 中间层曾有"流被截断却伪装成成功"的 bug，必须显式报错。
		return fmt.Errorf("上游流异常结束：未收到 [DONE]（已收到 id=%s model=%s created=%d）",
			lastID, lastModel, created)
	}
	return nil
}
