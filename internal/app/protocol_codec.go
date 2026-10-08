// protocol_codec.go：把「协议差异」收敛到一处，让换号/记账/限额逻辑共用。
//
// 🔴 为什么需要这层（2026-10-09 加 Anthropic 支持）：
//
//	加第二种协议时有两条路：
//	  A. 复制一份 chat.go 改成 anthropic.go —— 换号、记账、限额、错误映射
//	     全部要再写一遍，两边必然逐渐漂移（改了一边忘另一边）。
//	  B. 把"协议相关"的四个点抽出来，其余全部共用 —— 本文件。
//
//	选了 B。协议差异**只有四处**：
//	  1. 请求体形状（OpenAI 结构体 / Anthropic 翻译）
//	  2. 流式编码（SSE 帧格式与状态机不同）
//	  3. 非流式聚合（产出形状不同）
//	  4. 错误响应形状（OpenAI {error:{type,code}} / Anthropic {type:error,error:{type}}）
//
//	换号重试（failover.go）、用量记账（usage_record.go）、并发与体积限额
//	（limits.go）、平台过滤（platform_ctx.go）**一行都不用改** ——
//	这正是第 57 轮交接文档 §6.2 定的架构：canonical 事件层与协议无关。
package app

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/protocol/anthropic"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/protocol/responses"
	"workbuddy.local/workbuddy-api/internal/provider"
)

// decodedRequest 是协议无关的入站请求解析结果。
type decodedRequest struct {
	// clientModel 客户端请求的模型名（原始，可能命中别名）。
	clientModel string

	// rawBody 是已归一化为 OpenAI Chat 形状的请求体
	// （model 仍为客户端原名，由 handleChatWith 用 router 改写）。
	//
	// ⚠️ OpenAI 路径下这是**客户端原始字节**（原样透传，只改 model）——
	// 这是刻意的：有损往返会丢多模态数组与不认识的字段（2026-10-05 故障）。
	rawBody []byte

	stream          bool
	reasoningEffort string
	hasMessages     bool
}

// streamEncoder 是协议无关的流式编码器。
type streamEncoder interface {
	// start 写流首帧（Anthropic 的 message_start；OpenAI 无此概念）。
	start() error

	// write 吸收一个 canonical 事件并写出对应帧。
	write(provider.Event) error

	// finish 正常收尾（OpenAI 的 [DONE]；Anthropic 的 message_delta+message_stop）。
	finish() error

	// fail 异常收尾。🔴 Anthropic 路径下**绝不能**在这里发 message_stop。
	fail(error) error

	// usage 返回本次流的用量（供记账）；上游没给时返回 nil。
	usage() *openai.EventUsage
}

// aggregator 是协议无关的非流式聚合器。
type aggregator interface {
	add(provider.Event)
	usage() *openai.EventUsage
	result() any
}

// chatCodec 描述一种入站协议。
type chatCodec struct {
	// protocol 是记进用量事件的协议名（chat / responses / messages）。
	protocol string

	decodeRequest func([]byte) (*decodedRequest, error)
	writeError    func(http.ResponseWriter, int, string, string, string)
	newEncoder    func(http.ResponseWriter, http.Flusher, string, string, *chatSession) streamEncoder
	newAggregator func(upstreamModel, clientModel string) aggregator
}

// ─────────────────────────────────────────────────────────────
// OpenAI Chat Completions
// ─────────────────────────────────────────────────────────────

// defaultCodec 返回 OpenAI Chat 协议的编解码器。
//
// 零值 chatCodec{} 就是它 —— 保留零值可用是刻意的：
// handleChat 直接传 chatCodec{}，不给已有路径引入任何构造步骤。
func defaultCodec() chatCodec {
	return chatCodec{
		protocol:      "chat",
		decodeRequest: decodeOpenAIChat,
		writeError: func(w http.ResponseWriter, status int, errType, code, msg string) {
			writeError(w, status, errType, code, msg)
		},
		newEncoder: func(w http.ResponseWriter, flusher http.Flusher, upstreamModel, clientModel string,
			sess *chatSession) streamEncoder {
			return &openaiStreamEncoder{
				w: w, flusher: flusher, sess: sess,
				id: newResponseID(), created: nowFunc().Unix(),
				model: clientModel,
			}
		},
		newAggregator: func(upstreamModel, clientModel string) aggregator {
			return &openaiAggregator{
				agg: openai.NewAggregator(newResponseID(), clientModel, nowFunc().Unix()),
			}
		},
	}
}

// protocol 为空的 codec 视为 OpenAI Chat（零值可用）。
func (c chatCodec) normalized() chatCodec {
	if c.protocol == "" {
		return defaultCodec()
	}
	return c
}

// decodeOpenAIChat 解析 OpenAI Chat 请求。
func decodeOpenAIChat(body []byte) (*decodedRequest, error) {
	var req openai.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return &decodedRequest{
		clientModel:     req.Model,
		rawBody:         body, // 原样透传，只改 model
		stream:          req.Stream,
		reasoningEffort: req.ReasoningEffort,
		hasMessages:     len(req.Messages) > 0,
	}, nil
}

// openaiStreamEncoder 是 OpenAI 的流式编码器。
//
// 🔴 它必须**逐字节复现**重构前的输出（含错误帧后仍发 [DONE] 这个行为）——
// 那是已上线并被客户端依赖的契约，不能在加协议时被顺手"修正"。
// 有 TestOpenAIStreamWireFormatUnchanged 守着。
type openaiStreamEncoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	sess    *chatSession

	id      string
	created int64
	model   string
}

func (e *openaiStreamEncoder) start() error { return nil } // OpenAI 无首帧概念

func (e *openaiStreamEncoder) write(ev provider.Event) error {
	c := toOpenAIEvent(ev)
	if c == nil {
		return nil
	}
	// 记账：usage 只出现在流的最后一个事件，这里顺手收下。
	// 放在协议转换之后，复用同一份转换结果，不重复解析。
	if c.Type == openai.EvUsage {
		e.sess.noteUsage(c.Usage)
	}
	b, err := openai.MarshalChunk(openai.BuildChunk(e.id, e.model, e.created, *c))
	if err != nil {
		return err
	}
	return e.emit(b)
}

// finish 正常收尾：发 [DONE]。
func (e *openaiStreamEncoder) finish() error {
	return e.emit(openai.DoneMarker)
}

// fail 异常收尾：错误帧 + [DONE]。
//
// ⚠️ 错误帧后**仍然**发 [DONE] 是重构前就有的行为，这里刻意保留：
//
//	OpenAI 客户端把 [DONE] 当作"SSE 传输结束"的标记，不发它会让
//	部分客户端一直等。半截回答的问题在 OpenAI 形状下由 finish_reason
//	缺失体现（客户端自行判定），与 Anthropic 的 message_stop 语义不同。
func (e *openaiStreamEncoder) fail(err error) error {
	if werr := e.emit(mustMarshal(map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    openai.ErrTypeUpstream,
		},
	})); werr != nil {
		return werr
	}
	return e.emit(openai.DoneMarker)
}

func (e *openaiStreamEncoder) emit(b []byte) error {
	if _, err := e.w.Write(b); err != nil {
		return err // 客户端断开
	}
	e.flusher.Flush()
	return nil
}

// usage 返回流里最后出现的用量。
func (e *openaiStreamEncoder) usage() *openai.EventUsage { return e.sess.Usage() }

// openaiAggregator 包装 openai.Aggregator。
type openaiAggregator struct{ agg *openai.Aggregator }

func (a *openaiAggregator) add(ev provider.Event) {
	if c := toOpenAIEvent(ev); c != nil {
		a.agg.Add(*c)
	}
}
func (a *openaiAggregator) usage() *openai.EventUsage { return a.agg.Usage() }
func (a *openaiAggregator) result() any               { return a.agg.Result() }

// ─────────────────────────────────────────────────────────────
// Anthropic Messages
// ─────────────────────────────────────────────────────────────

// AnthropicProtocol 是 Anthropic 协议在用量记录里的名字。
const AnthropicProtocol = "messages"

// anthropicCodec 返回 Anthropic Messages 协议的编解码器。
func anthropicCodec() chatCodec {
	return chatCodec{
		protocol:      AnthropicProtocol,
		decodeRequest: decodeAnthropicMessages,
		writeError: func(w http.ResponseWriter, status int, errType, code, msg string) {
			anthropic.WriteError(w, status, anthropicErrType(errType), msg)
		},
		newEncoder: func(w http.ResponseWriter, flusher http.Flusher, upstreamModel, clientModel string,
			sess *chatSession) streamEncoder {
			sw := anthropic.NewStreamWriter(func(b []byte) error {
				if _, err := w.Write(b); err != nil {
					return err
				}
				flusher.Flush()
				return nil
			}, newResponseID(), clientModel)
			return &anthropicStreamEncoder{sw: sw}
		},
		newAggregator: func(upstreamModel, clientModel string) aggregator {
			return &anthropicAggregator{agg: anthropic.NewAggregator(newResponseID(), clientModel)}
		},
	}
}

// anthropicErrType 把 OpenAI 风格错误类型翻译为 Anthropic 规范枚举。
//
// 🔴 必须映射到那 9 个规范值之一，**不得自造**（见 anthropic 包注释）。
func anthropicErrType(errType string) string {
	switch errType {
	case openai.ErrTypeInvalidRequest:
		return anthropic.ErrInvalidRequest
	case openai.ErrTypeAuth:
		return anthropic.ErrAuthentication
	case openai.ErrTypeRateLimit:
		return anthropic.ErrRateLimit
	case openai.ErrTypeServer, openai.ErrTypeUpstream:
		return anthropic.ErrAPI
	}
	return anthropic.ErrAPI
}

// decodeAnthropicMessages 把 Anthropic 请求翻译为 OpenAI Chat 形状。
func decodeAnthropicMessages(body []byte) (*decodedRequest, error) {
	in, err := anthropic.Convert(body)
	if err != nil {
		return nil, err
	}
	return &decodedRequest{
		clientModel:     in.Model,
		rawBody:         in.RawBody,
		stream:          in.Stream,
		reasoningEffort: in.ReasoningEffort,
		hasMessages:     true, // Convert 已在缺失时报错
	}, nil
}

// anthropicStreamEncoder 是 Anthropic 的流式编码器。
type anthropicStreamEncoder struct {
	sw *anthropic.StreamWriter
}

func (e *anthropicStreamEncoder) start() error { return e.sw.Start() }
func (e *anthropicStreamEncoder) write(ev provider.Event) error {
	return e.sw.Handle(ev)
}
func (e *anthropicStreamEncoder) finish() error { return e.sw.Finish() }

// fail 异常收尾。
//
// 🔴 这里**只发 error 事件，绝不补 message_stop** ——
//
//	message_stop 在 Anthropic 协议里等同"正常结束"，客户端会把半截回答
//	当成完整回答继续跑（如 agent 拿着截断的 tool_call arguments 去执行）。
//	有 TestAnthropicStreamNeverStopsOnError 守着。
func (e *anthropicStreamEncoder) fail(err error) error { return e.sw.Fail(err) }

// usage 返回流里累积的用量，转换成记账用的形状。
//
// ⚠️ 为什么转换而不是让 usageRecorder 直接用 provider.Usage：
//
//	记账器要同时服务两种协议，保持单一形状（openai.EventUsage）
//	比让它认识两种结构体更简单，也避免记账字段随协议分叉。
func (e *anthropicStreamEncoder) usage() *openai.EventUsage {
	return providerUsageToEvent(e.sw.Usage())
}

// anthropicAggregator 包装 anthropic.Aggregator。
type anthropicAggregator struct{ agg *anthropic.Aggregator }

func (a *anthropicAggregator) add(ev provider.Event) { a.agg.Add(ev) }
func (a *anthropicAggregator) usage() *openai.EventUsage {
	return providerUsageToEvent(a.agg.Usage())
}
func (a *anthropicAggregator) result() any { return a.agg.Result() }

// providerUsageToEvent 把 provider 用量转成记账用的形状。
//
// 两种新协议（anthropic / responses）共用它 —— 记账器只认一种形状。
func providerUsageToEvent(u *provider.Usage) *openai.EventUsage {
	if u == nil {
		return nil
	}
	return &openai.EventUsage{
		PromptTokens:          u.PromptTokens,
		CompletionTokens:      u.CompletionTokens,
		TotalTokens:           u.TotalTokens,
		ReasoningTokens:       u.ReasoningTokens,
		Credit:                u.Credit,
		PromptCacheHitTokens:  u.PromptCacheHitTokens,
		PromptCacheMissTokens: u.PromptCacheMissTokens,
	}
}

// ─────────────────────────────────────────────────────────────
// OpenAI Responses API
// ─────────────────────────────────────────────────────────────

// ResponsesProtocol 是 Responses 协议在用量记录里的名字。
const ResponsesProtocol = "responses"

// responsesCodec 返回 OpenAI Responses 协议的编解码器。
//
// ⚠️ 与 anthropicCodec 的差异只在四处（正是 chatCodec 抽象的那四点）：
// 入站翻译、流式编码、非流式聚合、错误形状。换号/记账/限额/平台过滤
// 全部共用 —— 这正是第 57 轮定的架构。
func responsesCodec() chatCodec {
	return chatCodec{
		protocol:      ResponsesProtocol,
		decodeRequest: decodeResponses,
		writeError: func(w http.ResponseWriter, status int, errType, code, msg string) {
			// Responses 的错误体与 OpenAI Chat **形状相同**
			// （{"error":{"message","type","code"}}），因此直接复用。
			responses.WriteError(w, status, errType, code, msg)
		},
		newEncoder: func(w http.ResponseWriter, flusher http.Flusher, upstreamModel, clientModel string,
			sess *chatSession) streamEncoder {
			sw := responses.NewStreamWriter(func(b []byte) error {
				if _, err := w.Write(b); err != nil {
					return err
				}
				flusher.Flush()
				return nil
			}, newResponseID(), clientModel)
			return &responsesStreamEncoder{sw: sw}
		},
		newAggregator: func(upstreamModel, clientModel string) aggregator {
			return &responsesAggregator{agg: responses.NewAggregator(newResponseID(), clientModel)}
		},
	}
}

// decodeResponses 把 Responses 请求翻译为 OpenAI Chat 形状。
func decodeResponses(body []byte) (*decodedRequest, error) {
	in, err := responses.Convert(body)
	if err != nil {
		return nil, err
	}
	return &decodedRequest{
		clientModel:     in.Model,
		rawBody:         in.RawBody,
		stream:          in.Stream,
		reasoningEffort: in.ReasoningEffort,
		hasMessages:     true, // Convert 已在 input 为空时报错
	}, nil
}

// responsesStreamEncoder 是 Responses 的流式编码器。
type responsesStreamEncoder struct {
	sw *responses.StreamWriter
}

func (e *responsesStreamEncoder) start() error { return e.sw.Start() }
func (e *responsesStreamEncoder) write(ev provider.Event) error {
	return e.sw.Handle(ev)
}
func (e *responsesStreamEncoder) finish() error { return e.sw.Finish() }

// fail 异常收尾。
//
// 🔴 这里**只发 response.failed，绝不补 response.completed** ——
//
//	后者在 Responses 协议里等同"正常结束"，客户端会把半截回答当成
//	完整回答继续跑（与 Anthropic 的 message_stop 陷阱完全同类）。
//	有 TestResponsesStreamNeverCompletesOnError 守着。
func (e *responsesStreamEncoder) fail(err error) error { return e.sw.Fail(err) }

func (e *responsesStreamEncoder) usage() *openai.EventUsage {
	return providerUsageToEvent(e.sw.Usage())
}

// responsesAggregator 包装 responses.Aggregator。
type responsesAggregator struct{ agg *responses.Aggregator }

func (a *responsesAggregator) add(ev provider.Event) { a.agg.Add(ev) }
func (a *responsesAggregator) usage() *openai.EventUsage {
	return providerUsageToEvent(a.agg.Usage())
}
func (a *responsesAggregator) result() any { return a.agg.Result() }

// ─────────────────────────────────────────────────────────────
// 上游错误映射（协议可换）
// ─────────────────────────────────────────────────────────────

// writeUpstreamErrorWith 按协议写出上游错误。
//
// 判定逻辑与重构前的 writeUpstreamError 完全一致（含"没有可用账号 → 503"
// 与 provider 错误分类接口），只是错误的**序列化形状**交给 codec。
func writeUpstreamErrorWith(w http.ResponseWriter, err error, codec chatCodec) {
	codec = codec.normalized()

	status := http.StatusBadGateway
	errType := openai.ErrTypeUpstream
	code := "upstream_error"

	// 没有可用账号是【本服务】的状态，不是上游的错 —— 503 更准确
	if errors.Is(err, pool.ErrNoAvailable) || strings.Contains(err.Error(), "没有可用账号") {
		codec.writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"no_available_account", "没有可用账号（全部禁用/冷却/无额度）")
		return
	}

	// provider 层用接口暴露错误分类，避免 app 依赖具体渠道包
	var cls interface {
		IsAuthFailure() bool
		IsRateLimited() bool
	}
	if errors.As(err, &cls) {
		switch {
		case cls.IsAuthFailure():
			status, errType, code = http.StatusUnauthorized, openai.ErrTypeAuth, "auth_failed"
		case cls.IsRateLimited():
			status, errType, code = http.StatusTooManyRequests, openai.ErrTypeRateLimit, "rate_limited"
		}
	}
	if errors.Is(err, provider.ErrUnsupportedReasoning) {
		status, errType, code = http.StatusBadRequest, openai.ErrTypeInvalidRequest, "unsupported_reasoning_effort"
	}
	codec.writeError(w, status, errType, code, err.Error())
}

// ─────────────────────────────────────────────────────────────
// 鉴权：同时接受两种头
// ─────────────────────────────────────────────────────────────

// requireAPIKeyAny 包装 /v1/* 的处理器，同时接受两种鉴权头。
//
// 🔴 为什么 Anthropic 路径需要单独的包装（委托方 2026-10-09 拍板第 2 条）：
//
//	Anthropic 客户端用 `x-api-key` 头，OpenAI 客户端用
//	`Authorization: Bearer`。两者都要接受 —— 否则 DSH 切到
//	Anthropic 协议后立刻 401，而用户已经在设置里填了 key。
//
// ⚠️ 安全等价性：两条路径都只做"常量时间比较 + 不匹配即 401"，
//
//	不因为多一个入口而放松判定。仍只监听回环（红线 3 不变）。
func requireAPIKeyAny(deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var want string
		if deps.Settings != nil {
			want = deps.Settings.Get().APIKey
		}
		if want == "" {
			next(w, r)
			return
		}
		if matchAnyAPIKey(r, want) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="wbapi"`)
		writeError(w, http.StatusUnauthorized, openai.ErrTypeInvalidRequest,
			"invalid_api_key", "API key 不正确或缺失")
	}
}

// matchAnyAPIKey 判定请求是否带了正确的 key（Bearer 或 x-api-key）。
func matchAnyAPIKey(r *http.Request, want string) bool {
	if subtle.ConstantTimeCompare([]byte(bearerToken(r.Header.Get("Authorization"))), []byte(want)) == 1 {
		return true
	}
	// HTTP 头名本身不区分大小写，Go 的 Header.Get 已做归一
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("X-Api-Key"))), []byte(want)) == 1
}
