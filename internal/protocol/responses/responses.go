// Package responses 实现 OpenAI Responses API（POST /v1/responses）的
// 入站/出站翻译，让本服务能同时服务三种协议的客户端。
//
// 🔴 架构与 anthropic 包**完全一致**（第 57 轮交接文档 §6.2 定的）：
//
//	Responses 客户端 ──▶ 本包（翻译）──▶ OpenAI Chat 形状 ──▶ provider（零改动）
//
// 上游 WorkBuddy 只接受 /v2/chat/completions（Chat 形状）——硬约束改不了，
// 因此本层只能"翻译"，不能"再写一套代理"。反过来 canonical 事件层
// （provider.Event 只有 5 种）与协议无关 ⇒ provider、换号调度、额度、
// 记账【一行都不用改】。
//
// ⚠️ 与 anthropic 包的一个诚实差异：**本项目没有 Responses 的 Go 参考实现**。
//
//	参考实现 rockswang/wild-work 只提供了 anthropic.go / anthropic_stream.go，
//	没有 responses.go。因此本包是照 **OpenAI 官方 Responses 规范** +
//	**DSH 客户端真实契约**（从 app.asar 提取）写的，没有第三方代码可对照。
//	⇒ 它比 anthropic 包更依赖"真实客户端端到端验证"，不能只靠单测。
//
// 委托方明确的三条边界（与 anthropic 一致）：
//  1. 只新增 /v1/responses，/v1/chat/completions 完全不变
//  2. 鉴权同时接受 x-api-key 与 Authorization: Bearer
//  3. 做不到的特性**明确报错**，绝不静默丢弃
package responses

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ─────────────────────────────────────────────────────────────
// 输出 item 的 index 分配
// ─────────────────────────────────────────────────────────────

// Responses 用 output_index 标识输出 item 的顺序，与 Anthropic 的
// content block index 是同一个概念（协议要求严格递增）。
//
// 思考 item 必须先于正文（同上：上游顺序异常时丢弃乱序的思考增量）。
const (
	IndexReasoning = 0
	IndexMessage   = 1
	IndexItemBase  = 2
)

// ─────────────────────────────────────────────────────────────
// item 类型
// ─────────────────────────────────────────────────────────────

const (
	// ItemReasoning 思考项（出站）。
	ItemReasoning = "reasoning"

	// ItemMessage 助手消息项（出站）。
	ItemMessage = "message"

	// ItemFunctionCall 函数调用项（出站 / 入站）。
	ItemFunctionCall = "function_call"

	// ItemFunctionCallOutput 函数结果项（入站）。
	ItemFunctionCallOutput = "function_call_output"

	// ItemInputText 入站消息里的文本分片。
	ItemInputText = "input_text"

	// ItemOutputText 出站消息里的文本分片。
	ItemOutputText = "output_text"

	// ItemInputImage 入站消息里的图片分片。
	ItemInputImage = "input_image"
)

// ─────────────────────────────────────────────────────────────
// status
// ─────────────────────────────────────────────────────────────

const (
	// StatusCompleted 正常完成。
	StatusCompleted = "completed"

	// StatusIncomplete 未完成（如被 max_output_tokens 截断）。
	//
	// 🔴 与 completed 的区别是**语义性**的：客户端据 incomplete_details
	//	决定是否继续续写。把截断的响应标成 completed 会让 agent 把
	//	半截回答当完整回答（与 Anthropic 的 message_stop 陷阱同类）。
	StatusIncomplete = "incomplete"

	// StatusFailed 失败。
	StatusFailed = "failed"

	// StatusInProgress 进行中（仅用于流式开头的占位）。
	StatusInProgress = "in_progress"
)

// IncompleteReason 把 Chat 的 finish_reason 映射为
// Responses 的 incomplete_details.reason。
//
//	length / max_tokens → max_output_tokens
//	content_filter      → content_filter
//	其它                → ""（表示正常完成，不写 incomplete_details）
//
// 🔴 为什么要如实映射（与 anthropic 的 stop_reason 缺陷同类）：
//
//	被截断的响应若标成 status=completed，客户端会把它当完整回答继续跑。
func IncompleteReason(finish string) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	}
	return ""
}

// ─────────────────────────────────────────────────────────────
// 错误
// ─────────────────────────────────────────────────────────────

// 错误类型（与 OpenAI 约定一致；Responses 的错误形状与 Chat 相同）。
const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuth           = "authentication_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeServer         = "server_error"
)

// WriteError 按 OpenAI 兼容形状写错误响应。
//
// ⚠️ Responses 的错误体与 Chat Completions **形状相同**
//
//	（`{"error":{"message","type","code"}}`），因此这里不另造一套。
func WriteError(w http.ResponseWriter, status int, errType, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
			"code":    code,
		},
	})
}

// ─────────────────────────────────────────────────────────────
// id 生成
// ─────────────────────────────────────────────────────────────

// ResponseID 由上游响应 id 生成 Responses 风格的 resp_ id。
//
// 上游 id 形如 chatcmpl-1730000000000000000 ⇒ resp_1730000000000000000。
// 保留上游的随机部分便于把一次对话与上游日志对上。
func ResponseID(upstreamID string) string {
	return prefixID("resp_", upstreamID)
}

// MessageItemID 生成消息 item 的 id。
func MessageItemID(upstreamID string) string {
	return prefixID("msg_", upstreamID)
}

// ReasoningItemID 生成思考 item 的 id。
func ReasoningItemID(upstreamID string) string {
	return prefixID("rs_", upstreamID)
}

// FunctionCallItemID 生成函数调用 item 的 id。
func FunctionCallItemID(upstreamID string) string {
	return prefixID("fc_", upstreamID)
}

// CallID 生成函数调用的 call_id。
//
// ⚠️ 与 item id 是**两个不同字段**（Responses 规范）：
//
//	item id 标识"这个输出项"，call_id 用于把 function_call_output
//	关联回它的 function_call。用错会导致客户端关联不上工具结果。
func CallID(upstreamID string) string {
	return prefixID("call_", upstreamID)
}

// prefixID 取上游 id 的随机部分加上协议前缀。
func prefixID(prefix, upstreamID string) string {
	s := strings.TrimSpace(upstreamID)
	if s == "" {
		return prefix + randSuffix()
	}
	if i := strings.Index(s, "-"); i >= 0 && i+1 < len(s) {
		return prefix + s[i+1:]
	}
	return prefix + s
}
