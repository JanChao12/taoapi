package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件实现流式出站：canonical 事件 → Anthropic SSE 序列。
//
// 官方事件序列：
//
//	message_start
//	content_block_start (thinking)          ← 有思考时
//	content_block_delta (thinking_delta)    ×N
//	content_block_stop
//	content_block_start (text)              ← 有正文时
//	content_block_delta (text_delta)        ×N
//	content_block_stop
//	content_block_start (tool_use)          ← 每个工具调用一个块
//	content_block_delta (input_json_delta, partial_json) ×N
//	content_block_stop
//	message_delta (stop_reason + usage)
//	message_stop
//
// 🔴 本文件照搬参考实现（rockswang/wild-work）修掉的三个真实缺陷 ——
// 它们都是"单测能过、生产会坏"的类型：
//
//  1. 工具参数必须在 content_block_start 里【留空】，靠 input_json_delta
//     增量下发。若在 content_block_start 里一次性给完整 input，
//     **等 partial_json 的客户端会永远卡住**（它按"块开了但参数还没来"等待）。
//  2. stop_reason 必须按 finish_reason **如实映射**
//     （length→max_tokens、tool_calls→tool_use），不能恒为 end_turn ——
//     否则客户端不知道模型要调工具，agent 循环直接断掉。
//  3. usage 必须用上游真实值，不能恒为 0 —— 否则客户端上下文预算失效。
//
// 另加两条本项目自己的硬约束：
//
//  4. **流中途失败时绝不发 message_stop**。message_stop 在 Anthropic 协议里
//     等同"正常结束"，客户端会把半截回答当成完整回答继续跑
//     （例如 agent 拿着被截断的 tool_call arguments 去执行）。
//     必须发 error 事件终止，且 error.type 只能用规范枚举，不得自造。
//  5. 内容块必须按 index 递增且成对出现：思考 0、文本 1、工具从 2 起；
//     思考必须先于文本（上游顺序异常时**丢弃乱序的思考增量**）。

// FrameWriter 写出一帧 SSE 字节（调用方负责 Flush）。
type FrameWriter func([]byte) error

// StreamWriter 是有状态的 Anthropic SSE 生成器。
//
// 独立成结构体（而不是一组函数）是必要的：块的开/闭必须跨事件保持状态，
// 且流中途失败与正常收尾要走不同的路径。
type StreamWriter struct {
	write FrameWriter

	id    string
	model string

	// started 表示 message_start 已发出。
	started bool

	thinkingOpen   bool
	thinkingClosed bool
	textOpen       bool
	textClosed     bool

	tools *toolAccumulator

	finish string
	usage  *provider.Usage

	// failed 表示已进入失败收尾，避免重复发 error 或误发 message_stop。
	failed bool

	// finished 表示已正常收尾（message_stop 已发出）。
	//
	// 🔴 为什么需要它（本包测试抓到）：收尾必须**幂等**。
	//
	//	真实调用路径里，流错误后调用方仍可能走到收尾分支
	//	（见 app/protocol_codec.go 的 streamChat：fail 之后仍会检查 finish）。
	//	若不幂等，客户端会收到**两个 message_stop** —— 协议违规，
	//	部分客户端会把它当成两轮回复的边界。
	//
	//	同理，已正常收尾后再 Fail 也不能补发 error：那会让客户端
	//	把一次成功的请求判为失败。
	finished bool
}

// NewStreamWriter 创建流式生成器。
//
// upstreamID 用于派生 msg_ id（保留上游随机部分，便于与上游日志对上）。
func NewStreamWriter(w FrameWriter, upstreamID, model string) *StreamWriter {
	return &StreamWriter{
		write: w,
		id:    MessageID(upstreamID),
		model: model,
		tools: &toolAccumulator{},
	}
}

// ─────────────────────────────────────────────────────────────
// SSE 帧构造（纯函数，便于逐字节断言）
// ─────────────────────────────────────────────────────────────

// frame 构造一帧 Anthropic SSE。
//
// ⚠️ 与 OpenAI 的差别：Anthropic 用**具名事件**，格式是
//
//	event: <name>\n
//	data: <json>\n
//	\n
//
// 只写 data: 不带 event: 会被严格客户端忽略（它按 event 名分派）。
func frame(event string, payload any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化 %s 事件失败: %w", event, err)
	}
	out := make([]byte, 0, len(event)+len(b)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, '\n', '\n')
	return out, nil
}

// ─────────────────────────────────────────────────────────────
// 各事件构造
// ─────────────────────────────────────────────────────────────

// startFrame 构造 message_start。
//
// ⚠️ 这里的 usage 填 0 是**已知且有意的偏差**：Anthropic 规范把
// input_tokens 放在 message_start 里，但上游要到最后才返回用量。
// 真实值改在 message_delta 里下发（比规范多给 input_tokens，客户端会忽略
// 不认识的字段）。若为了"严格合规"而缓冲整条流，就彻底失去流式意义了。
func startFrame(id, model string) ([]byte, error) {
	return frame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// blockStartFrame 构造 content_block_start。
func blockStartFrame(index int, block map[string]any) ([]byte, error) {
	return frame("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": block,
	})
}

// blockStopFrame 构造 content_block_stop。
func blockStopFrame(index int) ([]byte, error) {
	return frame("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// deltaFrame 构造 content_block_delta。
func deltaFrame(index int, delta map[string]any) ([]byte, error) {
	return frame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index, "delta": delta,
	})
}

// ErrorFrame 构造 error 事件（供调用方在流中途失败时使用）。
//
// 🔴 error.type 必须落在规范枚举内，**不得自造**。
func ErrorFrame(errType, message string) ([]byte, error) {
	if errType == "" {
		errType = ErrAPI
	}
	return frame("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// ErrorTypeFor 依据 provider 层的错误分类接口选出 Anthropic 错误类型。
//
// 与 app 层 writeUpstreamError 用同一套接口判据（IsAuthFailure /
// IsRateLimited），避免两处各判一套导致同一错误在流式与非流式下分类不同。
func ErrorTypeFor(err error) string {
	if err == nil {
		return ErrAPI
	}
	var cls interface {
		IsAuthFailure() bool
		IsRateLimited() bool
	}
	if errors.As(err, &cls) {
		switch {
		case cls.IsAuthFailure():
			return ErrAuthentication
		case cls.IsRateLimited():
			return ErrRateLimit
		}
	}
	return ErrAPI
}

// ─────────────────────────────────────────────────────────────
// 流式状态机
// ─────────────────────────────────────────────────────────────

// Start 发出 message_start。必须最先调用，且只调用一次。
func (s *StreamWriter) Start() error {
	if s.started {
		return nil
	}
	s.started = true
	b, err := startFrame(s.id, s.model)
	if err != nil {
		return err
	}
	return s.write(b)
}

// Handle 吸收一个 canonical 事件并写出对应 SSE。
//
// 事件顺序异常（文本已开始又来思考）时**丢弃**该思考增量 ——
// 保证块顺序合法（思考必须 index 0 且先于文本 index 1）。
func (s *StreamWriter) Handle(ev provider.Event) error {
	switch ev.Type {
	case provider.EventReasoning:
		if !s.thinkingOpen && s.textOpen {
			// 上游顺序异常：文本块已开，思考块再开就会违反 index 递增。
			// 丢弃这一段（宁可少显示思考，也不能产生非法块序列）。
			return nil
		}
		if !s.thinkingOpen {
			b, err := blockStartFrame(IndexThinking, map[string]any{
				"type": "thinking", "thinking": "",
			})
			if err != nil {
				return err
			}
			if err := s.write(b); err != nil {
				return err
			}
			s.thinkingOpen = true
		}
		b, err := deltaFrame(IndexThinking, map[string]any{
			"type": "thinking_delta", "thinking": ev.Text,
		})
		if err != nil {
			return err
		}
		return s.write(b)

	case provider.EventContent:
		if !s.textOpen {
			if err := s.closeThinking(); err != nil {
				return err
			}
			b, err := blockStartFrame(IndexText, map[string]any{
				"type": "text", "text": "",
			})
			if err != nil {
				return err
			}
			if err := s.write(b); err != nil {
				return err
			}
			s.textOpen = true
		}
		b, err := deltaFrame(IndexText, map[string]any{
			"type": "text_delta", "text": ev.Text,
		})
		if err != nil {
			return err
		}
		return s.write(b)

	case provider.EventToolCall:
		// 只累积，收尾时统一下发 —— 见 Finish 的说明。
		s.tools.Add(ev.ToolCall)
		return nil

	case provider.EventUsage:
		if ev.Usage != nil {
			s.usage = ev.Usage
		}
		return nil

	case provider.EventDone:
		if ev.FinishReason != "" {
			s.finish = ev.FinishReason
		}
		return nil
	}
	return nil
}

// closeThinking 关闭思考块（幂等）。
func (s *StreamWriter) closeThinking() error {
	if !s.thinkingOpen || s.thinkingClosed {
		return nil
	}
	s.thinkingClosed = true
	b, err := blockStopFrame(IndexThinking)
	if err != nil {
		return err
	}
	return s.write(b)
}

// closeText 关闭文本块（幂等）。
func (s *StreamWriter) closeText() error {
	if !s.textOpen || s.textClosed {
		return nil
	}
	s.textClosed = true
	b, err := blockStopFrame(IndexText)
	if err != nil {
		return err
	}
	return s.write(b)
}

// Finish 正常收尾：关块 → 下发工具块 → message_delta → message_stop。
//
// 🔴 为什么工具块在**收尾时**统一下发，而不是边收边开：
//
//	块 index 必须递增（思考 0、文本 1、工具 2+）。若在工具块（index≥2）
//	打开后又来了正文，正文就得开一个 index≥3 的新文本块 —— 那是合法但
//	非常规的序列，部分客户端只认"一个文本块"。
//	缓冲到收尾下发可以**无条件保证**序列合法，代价只是工具参数少了
//	逐字增量（仍走 input_json_delta，等 partial_json 的客户端照样正常）。
//
//	// ponytail: 工具块收尾统一下发；若将来客户端明确要求参数逐字增量，
//	// 再改成"文本块关闭后即时开工具块"的状态机。
func (s *StreamWriter) Finish() error {
	if s.failed || s.finished {
		return nil
	}
	// 先置位再写帧：若写帧中途失败，也不该再重试一遍
	// （重试会发出第二个 message_delta，同样违规）。
	s.finished = true

	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.closeText(); err != nil {
		return err
	}

	// ── 工具块 ──
	tools := s.tools.List()
	idx := IndexToolBase
	for _, tc := range tools {
		callID := firstNonEmpty(tc.id, "toolu_"+randSuffix())

		// 🔴 缺陷 1 的修法：input 留空对象，参数走 input_json_delta。
		b, err := blockStartFrame(idx, map[string]any{
			"type": "tool_use", "id": callID, "name": tc.name,
			"input": map[string]any{},
		})
		if err != nil {
			return err
		}
		if err := s.write(b); err != nil {
			return err
		}

		if args := strings.TrimSpace(tc.args); args != "" {
			b, err := deltaFrame(idx, map[string]any{
				"type": "input_json_delta", "partial_json": tc.args,
			})
			if err != nil {
				return err
			}
			if err := s.write(b); err != nil {
				return err
			}
		}

		b, err = blockStopFrame(idx)
		if err != nil {
			return err
		}
		if err := s.write(b); err != nil {
			return err
		}
		idx++
	}

	// ── message_delta：stop_reason + 真实 usage ──
	// 🔴 缺陷 2 与 3 的修法都在这一帧。
	md := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": StopReason(s.finish, len(tools) > 0), "stop_sequence": nil},
	}
	if u := s.usagePayload(); u != nil {
		md["usage"] = u
	}
	b, err := frame("message_delta", md)
	if err != nil {
		return err
	}
	if err := s.write(b); err != nil {
		return err
	}

	b, err = frame("message_stop", map[string]any{"type": "message_stop"})
	if err != nil {
		return err
	}
	return s.write(b)
}

// Fail 异常收尾：关块 → error 事件。**不发 message_stop**。
//
// 🔴 这是本项目自己加的硬约束（参考实现也这么做）：
//
//	message_stop 在 Anthropic 协议里等同"正常结束"。半截回答配上
//	message_stop，客户端会把它当完整回答继续跑 —— 例如 agent 拿着
//	被截断的 tool_call arguments 去执行，或把半句话当最终答案。
//	error 事件本身就是终止信号，客户端据此判定本次请求失败。
//
//	块仍要关闭（协议要求成对），关闭动作不会让客户端误判成功。
func (s *StreamWriter) Fail(err error) error {
	// 已正常收尾 → 不再补发 error：那会让客户端把一次**成功**的请求判为失败。
	if s.failed || s.finished {
		return nil
	}
	s.failed = true

	// 关块失败不改变"已经失败"的事实，忽略错误继续发 error 帧 ——
	// 否则客户端会卡在"块开着"的状态里等超时。
	_ = s.closeThinking()
	_ = s.closeText()

	b, ferr := ErrorFrame(ErrorTypeFor(err), err.Error())
	if ferr != nil {
		return ferr
	}
	return s.write(b)
}

// usagePayload 构造 message_delta 里的 usage。
//
// 上游没给 usage 时返回 nil（不输出该字段）——
// 输出 0 会被客户端当成"确实消耗 0 token"，那是伪造数据。
func (s *StreamWriter) usagePayload() map[string]any {
	if s.usage == nil {
		return nil
	}
	u := map[string]any{
		"input_tokens":  s.usage.PromptTokens,
		"output_tokens": s.usage.CompletionTokens,
	}
	if s.usage.PromptCacheHitTokens > 0 {
		u["cache_read_input_tokens"] = s.usage.PromptCacheHitTokens
	}
	return u
}

// Usage 返回流里累积的用量；从未收到时为 nil。
//
// 🔴 返回 nil 而不是零值有明确含义：上游没给 usage（例如客户端中途断开）。
// 调用方【不得】把 nil 当成 0 —— 那会把"未知"记成"确实消耗 0"
// （见 usage.Event.UsageKnown 的说明）。
func (s *StreamWriter) Usage() *provider.Usage { return s.usage }
