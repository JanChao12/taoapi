// Package anthropic 实现 Anthropic Messages API 的入站/出站翻译，
// 让本服务能同时服务 OpenAI 与 Anthropic 两类客户端。
//
// 🔴 架构（委托方 2026-10-09 拍板）：**客户端重写外壳，上游只能翻译**。
//
//	Anthropic 客户端 ──▶ 本包（翻译）──▶ OpenAI Chat 形状 ──▶ provider（零改动）
//
// 依据：
//   - 上游 WorkBuddy **只接受** /v2/chat/completions（OpenAI 形状）——
//     这是硬约束，改不了 ⇒ 本层只能"翻译"，不能"再写一套代理"。
//   - 反过来 canonical 事件层（provider.Event 只有 5 种：
//     reasoning/content/tool_call/usage/done）与协议无关 ⇒
//     provider 层、换号调度、额度、记账【一行都不用改】。
//
// 委托方明确的三条边界：
//  1. 只新增 /v1/messages，/v1/chat/completions 完全不变
//  2. x-api-key 与 Authorization: Bearer 都接受
//  3. 做不到的特性**明确报错**，绝不静默丢弃
//
// 第 3 条在本包的落点：服务端工具（web_search_20250305 等）、document
// （PDF）块、未知 content block 类型 —— 一律返回 invalid_request_error
// 并在 message 里点名，而不是丢掉它们假装成功。
package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ─────────────────────────────────────────────────────────────
// 协议常量
// ─────────────────────────────────────────────────────────────

// DefaultMaxTokens 是 max_tokens 缺失时的保守默认值。
//
// 🔴 Anthropic 规范要求 max_tokens **必填**，但实测客户端会漏发
// （某些 SDK 包装层只在显式设置时才带上）。这里给默认值而**不是报错**：
// 用户发的是普通对话请求，不该因为少一个字段被拒。
const DefaultMaxTokens = 4096

// 内容块 index 分配（Anthropic 严格要求块按 index 递增且成对出现）。
//
// 思考块必须先于文本块 ⇒ 固定占 0，文本占 1，工具块从 2 起。
const (
	IndexThinking = 0
	IndexText     = 1
	IndexToolBase = 2
)

// 错误类型枚举。
//
// 🔴 **只能用这 9 个**（SDK 的 ErrorObject 是 9 元判别联合，type 为
// Literal 标签）。自造类型（如把内层私有码 upstream_rate_limited 填进去）
// 会让严格客户端判为未知类型。官方文档说"客户端应优雅处理未知 type"——
// 反过来说**服务端不得自造**。内层码保留在 message 里，信息不丢。
const (
	ErrInvalidRequest = "invalid_request_error"
	ErrAuthentication = "authentication_error"
	ErrBilling        = "billing_error"
	ErrPermission     = "permission_error"
	ErrNotFound       = "not_found_error"
	ErrRateLimit      = "rate_limit_error"
	ErrGatewayTimeout = "gateway_timeout_error"
	ErrAPI            = "api_error"
	ErrOverloaded     = "overloaded_error"
)

// ErrorStatus 把错误类型映射为 HTTP 状态码。
//
// 依据 Anthropic 官方错误码表：billing_error 是 400（不是 402），
// overloaded_error 是 529（非标准状态码，但官方就这么用）。
func ErrorStatus(errType string) int {
	switch errType {
	case ErrInvalidRequest, ErrBilling:
		return http.StatusBadRequest
	case ErrAuthentication:
		return http.StatusUnauthorized
	case ErrPermission:
		return http.StatusForbidden
	case ErrNotFound:
		return http.StatusNotFound
	case ErrRateLimit:
		return http.StatusTooManyRequests
	case ErrGatewayTimeout:
		return http.StatusGatewayTimeout
	case ErrOverloaded:
		return 529 // Anthropic 专有
	case ErrAPI:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// ErrorResponse 是 Anthropic 形状的错误响应。
//
// ⚠️ 与 OpenAI 形状的区别：外层多一个 "type":"error"，
// 且没有 code/param 字段。客户端按这个形状解。
type ErrorResponse struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是错误详情。
type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// NewError 构造 Anthropic 错误响应。
func NewError(errType, message string) ErrorResponse {
	if errType == "" {
		errType = ErrAPI
	}
	return ErrorResponse{Type: "error", Error: ErrorDetail{Type: errType, Message: message}}
}

// WriteError 按 Anthropic 形状写错误响应。
//
// status 传 0 时按 errType 推导。
func WriteError(w http.ResponseWriter, status int, errType, message string) {
	if status == 0 {
		status = ErrorStatus(errType)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(NewError(errType, message))
}

// ─────────────────────────────────────────────────────────────
// stop_reason 映射
// ─────────────────────────────────────────────────────────────

// StopReason 把 Chat 的 finish_reason 映射为 Anthropic 的 stop_reason。
//
// 🔴 参考实现（rockswang/wild-work）在此修掉了一个真实缺陷：
//
//	tokligence-gateway 把 stop_reason **恒为 end_turn**，
//	于是客户端不知道模型要调工具，agent 循环直接断掉。
//
//	stop           → end_turn
//	length         → max_tokens
//	tool_calls     → tool_use
//	content_filter → end_turn（Anthropic 无对应值，交由客户端按内容判定）
//
// ⚠️ 已知的**无法精确**之处：Anthropic 的 stop_sequence 表示"命中停止序列"，
// 而上游的 finish_reason=stop 无法区分"自然结束"与"命中 stop 序列"
// ⇒ 一律映射为 end_turn。这不影响客户端正确性（两者都表示正常结束）。
func StopReason(finish string, hasToolCalls bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop", "":
		if hasToolCalls {
			return "tool_use"
		}
		return "end_turn"
	case "content_filter":
		return "end_turn"
	}
	if hasToolCalls {
		return "tool_use"
	}
	return "end_turn"
}

// ─────────────────────────────────────────────────────────────
// 用量映射
// ─────────────────────────────────────────────────────────────

// NewUsage 由上游真实用量构造 Anthropic 用量。
//
// 🔴 input/output 都必须是**上游真实值，不能填 0** ——
//
//	参考实现（rockswang/wild-work）修掉的第三个缺陷就是
//	tokligence-gateway 把 usage 恒为 0，那会让客户端的上下文预算
//	彻底失效（客户端以为上下文还空着，于是继续往里塞）。
//
// 定义在 outbound.go（Usage 类型要与 Message 放一起）。
func NewUsage(inputTokens, outputTokens int64) Usage {
	return Usage{InputTokens: inputTokens, OutputTokens: outputTokens}
}
