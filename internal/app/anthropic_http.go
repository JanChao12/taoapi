// anthropic_http.go：Anthropic Messages 的 HTTP 入口。
//
// 分工（与 OpenAI 路径保持一致的结构）：
//
//	server.go           路由 + 鉴权
//	anthropic_http.go   HTTP 层校验（方法/体积/JSON）→ 交给共用的 handleChatWith
//	protocol_codec.go   协议编解码器（入站翻译 / 流式 / 聚合 / 错误形状）
//	protocol/anthropic  纯协议逻辑（无 HTTP、无 app 依赖，可独立测试）
//
// 🔴 为什么复用 handleChatWith 而不是重写一遍：
//
//	换号重试、用量记账、并发限额、平台过滤、请求体上限 —— 这些逻辑
//	与协议无关，且都是踩过坑才修对的（见 failover.go 的 R1/按需续期、
//	usage_record.go 的不伪造 usage）。复制一份必然漂移。
package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"workbuddy.local/workbuddy-api/internal/protocol/anthropic"
	"workbuddy.local/workbuddy-api/internal/protocol/responses"
)

// handleAnthropicMessages 处理 POST /v1/messages。
func handleAnthropicMessages(deps Deps, w http.ResponseWriter, r *http.Request) {
	// 🔴 方法校验必须在最前，且用 Anthropic 形状回错误。
	//
	// ⚠️ 这里也顺带挡住了 /v1/messages/count_tokens 的误入：
	//	Go 的 ServeMux 会优先匹配更具体的模式，所以正常不会走到这里；
	//	但若将来有人注册了别的子路径，本函数会以 405 拒绝 ——
	//	而不是拿一个 count_tokens 的请求体去发对话。
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		anthropic.WriteError(w, http.StatusMethodNotAllowed,
			anthropic.ErrInvalidRequest, "只支持 POST")
		return
	}
	handleChatWith(deps, w, r, anthropicCodec())
}

// handleAnthropicCountTokens 处理 POST /v1/messages/count_tokens。
//
// 响应形状：{"input_tokens": N}
//
// ⚠️ 本端点**不需要** Router/账号：它是纯计算（字符启发式估算），
//
//	不发任何上游请求。因此未配置渠道时也应该能正常工作 ——
//	否则客户端在启动阶段探测能力时就会失败。
func handleAnthropicCountTokens(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		anthropic.WriteError(w, http.StatusMethodNotAllowed,
			anthropic.ErrInvalidRequest, "只支持 POST")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest,
			anthropic.ErrInvalidRequest, "读取请求体失败: "+err.Error())
		return
	}
	if len(body) > MaxRequestBodyBytes {
		anthropic.WriteError(w, http.StatusRequestEntityTooLarge,
			anthropic.ErrInvalidRequest, "请求体超过上限")
		return
	}

	// 🔴 先做形状校验再估算。
	//
	//	只把 body 丢给启发式会**静默接受**任何东西 ——
	//	连 `{"model":"x","messages":"不是数组"}` 都会"成功"返回一个数字，
	//	客户端据此以为请求合法，直到真正发对话才失败。
	//	用与 /v1/messages 完全相同的翻译做校验，保证两边判定一致。
	if _, err := anthropic.Convert(body); err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, err.Error())
		return
	}

	tokens, err := anthropic.CountTokens(body)
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, anthropic.ErrInvalidRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"input_tokens": tokens})
}

// handleResponses 处理 POST /v1/responses（OpenAI Responses API）。
//
// 分工与 anthropic 路径完全一致：本函数只做方法校验，
// 其余交给共用的 handleChatWith —— 换号重试、用量记账、并发限额、
// 平台过滤、请求体上限全部复用，不复制一份。
func handleResponses(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		responses.WriteError(w, http.StatusMethodNotAllowed,
			responses.ErrTypeInvalidRequest, "method_not_allowed", "只支持 POST")
		return
	}
	handleChatWith(deps, w, r, responsesCodec())
}

// 供将来扩展（避免未使用导入警告）。
var _ = strings.TrimSpace
