package responses

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件实现流式出站：canonical 事件 → Responses SSE 序列。
//
// 官方事件序列（一个"思考 + 正文 + 工具调用"的完整回合）：
//
//	response.created
//	response.in_progress
//	response.output_item.added          (reasoning)
//	response.reasoning_summary_part.added
//	response.reasoning_summary_text.delta  ×N
//	response.reasoning_summary_text.done
//	response.reasoning_summary_part.done
//	response.output_item.done           (reasoning)
//	response.output_item.added          (message)
//	response.content_part.added
//	response.output_text.delta          ×N
//	response.output_text.done
//	response.content_part.done
//	response.output_item.done           (message)
//	response.output_item.added          (function_call)   ← 每个调用一个 item
//	response.function_call_arguments.delta ×N
//	response.function_call_arguments.done
//	response.output_item.done           (function_call)
//	response.completed
//
// 🔴 三个必须照搬 anthropic 包修法的硬约束（同一类缺陷）：
//
//  1. **工具参数走 `.delta` 增量下发**，不在 output_item.added 里一次性给完整
//     arguments —— 后者会让等 `response.function_call_arguments.delta`
//     的客户端卡住（它按"item 开了但参数还没来"等待）。
//  2. **status 如实映射**：被 max_output_tokens 截断必须发 `response.incomplete`
//     而不是 `response.completed`，否则客户端把半截回答当完整回答。
//  3. **usage 用上游真实值**，上游没给就**不输出**该字段（0 是伪造数据）。
//
// 🔴 本项目自己的第 4 条硬约束（与 anthropic 一致）：
//
//	**流中途失败绝不发 `response.completed`**。它等同"正常结束"。
//	必须发 `response.failed`（携带 error 对象）终止。
//
// ⚠️ 诚实说明：本项目**没有** Responses 的 Go 参考实现可对照
//	（rockswang/wild-work 只有 anthropic）。因此本文件的正确性
//	依赖两件事：官方规范 + DSH 客户端的真实解析行为，
//	且**必须**用真实客户端端到端验证过才算数。

// FrameWriter 写出一帧 SSE 字节。
type FrameWriter func([]byte) error

// StreamWriter 是有状态的 Responses SSE 生成器。
type StreamWriter struct {
	write FrameWriter

	upstreamID string
	model      string

	started bool

	// seq 是 Responses 的 sequence_number，每个事件递增。
	//
	// ⚠️ 规范要求该字段存在且单调递增。漏掉它会让严格客户端
	//	无法检测事件乱序/丢失。
	seq int64

	reasoningOpen bool
	reasoningDone bool
	messageOpen   bool
	messageDone   bool

	// reasoningText / contentText 累积已下发的增量。
	//
	// 🔴 为什么必须累积（2026-10-09 从 DSH 的 app.asar 提取的真实契约）：
	//
	//	DSH 的解析器先按 `response.output_text.delta` 累加文本，然后在
	//	`response.output_item.done` 时**用 item.content[] 覆盖**累加结果
	//	（pi-ai/dist/api/openai-responses-shared.js:595-604）：
	//
	//	  slot.block.text =
	//	      item.content?.map(c => c.type === "output_text" ? c.text : c.refusal).join("") || "";
	//
	//	⇒ 若 done 事件里的 content[] 是**空数组**，累加好的正文会被**抹成空串**，
	//	  用户看到一条空白回答。思考项（summary[]）同理。
	//
	//	这是"事件都发对了、内容却丢了"的典型 —— 只看"事件名齐全"的测试
	//	抓不到。有 TestMessageDoneCarriesFullContent 守着。
	reasoningText strings.Builder
	contentText   strings.Builder

	tools *toolAccumulator

	finish string
	usage  *provider.Usage

	failed   bool
	finished bool
}

// NewStreamWriter 创建流式生成器。
func NewStreamWriter(w FrameWriter, upstreamID, model string) *StreamWriter {
	return &StreamWriter{
		write:      w,
		upstreamID: upstreamID,
		model:      model,
		tools:      &toolAccumulator{},
	}
}

// ─────────────────────────────────────────────────────────────
// 帧构造
// ─────────────────────────────────────────────────────────────

// frame 构造一帧 Responses SSE。
//
// 格式与 Anthropic 相同（具名事件 + data），但 data 里**多一个
// sequence_number** 字段（Responses 规范要求）。
func (s *StreamWriter) frame(event string, payload map[string]any) ([]byte, error) {
	// 自动补 type 与 sequence_number（调用方不必每次手写）
	payload["type"] = event
	payload["sequence_number"] = s.seq
	s.seq++

	b, err := mustJSON(payload)
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

// emit 构造并写出。
func (s *StreamWriter) emit(event string, payload map[string]any) error {
	b, err := s.frame(event, payload)
	if err != nil {
		return err
	}
	return s.write(b)
}

// responseSnapshot 构造 response 对象的快照（多个事件都要带它）。
func (s *StreamWriter) responseSnapshot(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":         ResponseID(s.upstreamID),
		"object":     "response",
		"created_at": nowUnix(),
		"status":     status,
		"model":      s.model,
		"output":     output,
	}
}

// ─────────────────────────────────────────────────────────────
// 流式状态机
// ─────────────────────────────────────────────────────────────

// Start 发出 response.created 与 response.in_progress。
func (s *StreamWriter) Start() error {
	if s.started {
		return nil
	}
	s.started = true

	if err := s.emit("response.created", map[string]any{
		"response": s.responseSnapshot(StatusInProgress, nil),
	}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{
		"response": s.responseSnapshot(StatusInProgress, nil),
	})
}

// Handle 吸收一个 canonical 事件并写出对应 SSE。
func (s *StreamWriter) Handle(ev provider.Event) error {
	switch ev.Type {
	case provider.EventReasoning:
		// 文本/消息项已开后再来的思考：丢弃（保证 output_index 递增）。
		if s.messageOpen {
			return nil
		}
		if !s.reasoningOpen {
			if err := s.openReasoning(); err != nil {
				return err
			}
		}
		s.reasoningText.WriteString(ev.Text)
		return s.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       ReasoningItemID(s.upstreamID),
			"output_index":  IndexReasoning,
			"summary_index": 0,
			"delta":         ev.Text,
		})

	case provider.EventContent:
		if !s.messageOpen {
			if err := s.closeReasoning(); err != nil {
				return err
			}
			if err := s.openMessage(); err != nil {
				return err
			}
		}
		s.contentText.WriteString(ev.Text)
		return s.emit("response.output_text.delta", map[string]any{
			"item_id":       MessageItemID(s.upstreamID),
			"output_index":  IndexMessage,
			"content_index": 0,
			"delta":         ev.Text,
		})

	case provider.EventToolCall:
		// 只累积，收尾时统一下发（与 anthropic 包同样的取舍，见 Finish）。
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

// openReasoning 开思考项（含 summary part）。
func (s *StreamWriter) openReasoning() error {
	s.reasoningOpen = true
	itemID := ReasoningItemID(s.upstreamID)
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": IndexReasoning,
		"item": map[string]any{
			"id": itemID, "type": ItemReasoning, "status": StatusInProgress,
			"summary": []any{},
		},
	}); err != nil {
		return err
	}
	return s.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       itemID,
		"output_index":  IndexReasoning,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

// closeReasoning 关思考项（幂等）。
//
// 🔴 summary[] 里必须带**完整**的思考文本，不能是空数组 ——
//
//	DSH 会用 item.summary[] 覆盖累加结果（见 StreamWriter.reasoningText 的说明）。
func (s *StreamWriter) closeReasoning() error {
	if !s.reasoningOpen || s.reasoningDone {
		return nil
	}
	s.reasoningDone = true
	itemID := ReasoningItemID(s.upstreamID)
	text := s.reasoningText.String()

	if err := s.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": itemID, "output_index": IndexReasoning,
		"summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := s.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": itemID, "output_index": IndexReasoning, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	}); err != nil {
		return err
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": IndexReasoning,
		"item": map[string]any{
			"id": itemID, "type": ItemReasoning, "status": StatusCompleted,
			"summary": []any{
				map[string]any{"type": "summary_text", "text": text},
			},
		},
	})
}

// openMessage 开消息项（含 content part）。
func (s *StreamWriter) openMessage() error {
	s.messageOpen = true
	itemID := MessageItemID(s.upstreamID)
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": IndexMessage,
		"item": map[string]any{
			"id": itemID, "type": ItemMessage, "status": StatusInProgress,
			"role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{
		"item_id":       itemID,
		"output_index":  IndexMessage,
		"content_index": 0,
		"part":          map[string]any{"type": ItemOutputText, "text": "", "annotations": []any{}},
	})
}

// closeMessage 关消息项（幂等）。
//
// 🔴 content[] 里必须带**完整**的正文，不能是空数组 ——
//
//	DSH 会用 item.content[] 覆盖累加结果（见 StreamWriter.contentText 的说明）。
//	发空数组 = 用户看到一条空白回答。
func (s *StreamWriter) closeMessage() error {
	if !s.messageOpen || s.messageDone {
		return nil
	}
	s.messageDone = true
	itemID := MessageItemID(s.upstreamID)
	text := s.contentText.String()

	if err := s.emit("response.output_text.done", map[string]any{
		"item_id": itemID, "output_index": IndexMessage,
		"content_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := s.emit("response.content_part.done", map[string]any{
		"item_id": itemID, "output_index": IndexMessage, "content_index": 0,
		"part": map[string]any{"type": ItemOutputText, "text": text, "annotations": []any{}},
	}); err != nil {
		return err
	}
	return s.emit("response.output_item.done", map[string]any{
		"output_index": IndexMessage,
		"item": map[string]any{
			"id": itemID, "type": ItemMessage, "status": StatusCompleted,
			"role": "assistant",
			"content": []any{
				map[string]any{"type": ItemOutputText, "text": text, "annotations": []any{}},
			},
		},
	})
}

// Finish 正常收尾：关项 → 工具项 → response.completed / incomplete。
func (s *StreamWriter) Finish() error {
	if s.failed || s.finished {
		return nil
	}
	s.finished = true

	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.closeMessage(); err != nil {
		return err
	}

	// ── 函数调用项 ──
	tools := s.tools.List()
	idx := IndexItemBase
	// outputItems 收集完整 item 列表，供收尾事件回填。
	//
	// 🔴 为什么要回填 output[]（DSH 真实契约）：
	//	DSH 会在 response.completed 时**重新扫描 response.output[]**
	//	来回填 reasoning 的 encrypted_content 等字段
	//	（pi-ai/dist/api/openai-responses-shared.js:417-432 backfillReasoningSignatures）。
	//	给空数组虽然不会崩，但客户端拿不到完整的最终输出，
	//	多轮重放（store:false）时会缺上下文。
	var outputItems []any

	if s.reasoningDone {
		outputItems = append(outputItems, map[string]any{
			"id": ReasoningItemID(s.upstreamID), "type": ItemReasoning,
			"status": StatusCompleted,
			"summary": []any{
				map[string]any{"type": "summary_text", "text": s.reasoningText.String()},
			},
		})
	}
	if s.messageDone {
		outputItems = append(outputItems, map[string]any{
			"id": MessageItemID(s.upstreamID), "type": ItemMessage,
			"status": StatusCompleted, "role": "assistant",
			"content": []any{
				map[string]any{
					"type": ItemOutputText, "text": s.contentText.String(),
					"annotations": []any{},
				},
			},
		})
	}

	for i, tc := range tools {
		// 🔴 每个 function_call item 必须有**唯一** id。
		//
		//	此前所有 item 共用 FunctionCallItemID(s.upstreamID)（由同一个
		//	上游 id 派生），多个工具调用时 id 会重复 —— 而 DSH 用
		//	`${item.call_id}|${item.id}` 作为块的标识（shared.js:368），
		//	id 重复会让多个调用被认成同一个。
		itemID := FunctionCallItemID(s.upstreamID)
		if len(tools) > 1 {
			itemID = fmt.Sprintf("%s_%d", itemID, i)
		}
		callID := firstNonEmpty(tc.id, CallID(s.upstreamID))
		if len(tools) > 1 && tc.id == "" {
			callID = fmt.Sprintf("%s_%d", callID, i)
		}

		// 🔴 约束 1 的修法：arguments 在 added 事件里给**空串**，
		// 参数走 .delta 增量下发。
		if err := s.emit("response.output_item.added", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id": itemID, "type": ItemFunctionCall, "status": StatusInProgress,
				"call_id": callID, "name": tc.name, "arguments": "",
			},
		}); err != nil {
			return err
		}

		if args := strings.TrimSpace(tc.args); args != "" {
			if err := s.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": itemID, "output_index": idx, "delta": tc.args,
			}); err != nil {
				return err
			}
		}

		if err := s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": itemID, "output_index": idx, "arguments": ParseArgs(tc.args),
		}); err != nil {
			return err
		}

		finalItem := map[string]any{
			"id": itemID, "type": ItemFunctionCall, "status": StatusCompleted,
			"call_id": callID, "name": tc.name, "arguments": ParseArgs(tc.args),
		}
		if err := s.emit("response.output_item.done", map[string]any{
			"output_index": idx, "item": finalItem,
		}); err != nil {
			return err
		}
		outputItems = append(outputItems, finalItem)
		idx++
	}

	// ── 收尾事件：status 如实映射 ──
	// 🔴 约束 2 的修法在这里。
	snap := s.responseSnapshot(StatusCompleted, outputItems)
	if s.usage != nil {
		snap["usage"] = usageOf(s.usage)
	}

	if reason := IncompleteReason(s.finish); reason != "" {
		snap["status"] = StatusIncomplete
		snap["incomplete_details"] = map[string]any{"reason": reason}
		return s.emit("response.incomplete", map[string]any{"response": snap})
	}
	return s.emit("response.completed", map[string]any{"response": snap})
}

// Fail 异常收尾：关项 → response.failed。**绝不发 response.completed**。
//
// 🔴 与 anthropic 的 message_stop 陷阱完全同类：
//
//	response.completed 在 Responses 协议里等同「正常结束」。半截回答配上
//	它，客户端会把它当完整回答继续跑（agent 拿着被截断的
//	function_call arguments 去执行）。
//	response.failed 携带 error 对象，本身就是终止信号。
func (s *StreamWriter) Fail(err error) error {
	// 已正常收尾 → 不补发 failed：那会让客户端把**成功**的请求判为失败。
	if s.failed || s.finished {
		return nil
	}
	s.failed = true

	// 关项失败不改变"已经失败"的事实，忽略错误继续发 failed 帧 ——
	// 否则客户端会卡在"item 开着"的状态里等超时。
	_ = s.closeReasoning()
	_ = s.closeMessage()

	code, msg := errorCode(err)
	snap := s.responseSnapshot(StatusFailed, nil)
	snap["error"] = map[string]any{"code": code, "message": msg}

	return s.emit("response.failed", map[string]any{"response": snap})
}

// errorCode 把错误映射为 Responses 的错误码与文案。
//
// 🔴 错误码必须落在规范枚举内（server_error / rate_limit_exceeded /
//
//	invalid_request_error / ...），不得自造。
func errorCode(err error) (string, string) {
	if err == nil {
		return "server_error", "未知错误"
	}
	var cls interface {
		IsAuthFailure() bool
		IsRateLimited() bool
	}
	if errors.As(err, &cls) {
		switch {
		case cls.IsAuthFailure():
			return "invalid_api_key", err.Error()
		case cls.IsRateLimited():
			return "rate_limit_exceeded", err.Error()
		}
	}
	return "server_error", err.Error()
}

// Usage 返回流里累积的用量；从未收到时为 nil。
//
// 🔴 返回 nil 而不是零值：上游没给 usage 时调用方**不得**当成 0
// （见 usage.Event.UsageKnown 的说明）。
func (s *StreamWriter) Usage() *provider.Usage { return s.usage }

// nowUnix 返回当前 Unix 秒。
//
// 抽成变量便于测试固定时间（本包没有其他时间依赖）。
var nowUnix = func() int64 { return time.Now().Unix() }
