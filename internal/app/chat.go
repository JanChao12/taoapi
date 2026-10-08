package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/router"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// rewriteModel 把原始请求体里的 model 字段替换为 upstreamID，
// 其余字段【逐字节保持原样】。
//
// 为什么不用 struct 往返（`json.Unmarshal` → 改字段 → `json.Marshal`）：
// 结构体是有损的中间表示。实测 DSH 会发 content 多模态数组与 tools 数组，
// 一旦经有损往返，不认识的字段会丢、形状会变形，上游可能因此报错或行为改变。
// 只替换一个顶层标量字段，用最保守的方式做。
//
// 失败时返回错误（调用方按 invalid_json 处理）——不静默放过。
func rewriteModel(body []byte, upstreamID string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(upstreamID)
	if err != nil {
		return nil, err
	}
	obj["model"] = json.RawMessage(encoded)
	return json.Marshal(obj)
}

// handleChat 处理 /v1/chat/completions。
//
// 双路径：
//   - 客户端要流式 → 逐事件转 SSE 并 Flush
//   - 客户端要非流式 → 消费同一批事件，聚合后返回一个 JSON
//
// 之所以两条路都走上游流式：上游强制 stream:true。
//
// 🔴 协议可换（2026-10-09 加 Anthropic）：本函数只依赖
// [chatCodec] 三个能力（事件转换 / 流式写出 / 错误写出），
// 因此同一套换号、记账、限额逻辑可以同时服务两种协议。
func handleChat(deps Deps, w http.ResponseWriter, r *http.Request) {
	handleChatWith(deps, w, r, chatCodec{})
}

// handleChatWith 是 handleChat 的协议可换版本。
func handleChatWith(deps Deps, w http.ResponseWriter, r *http.Request, codec chatCodec) {
	// 🔴 必须归一化：零值 chatCodec{} 的四个函数字段都是 nil，
	//	直接调用会 panic（实测：TestValidationNoProvider 在 Router==nil
	//	分支上触发 nil 解引用，整个 httptest 服务崩掉）。
	//	normalized() 把零值补齐为 OpenAI Chat 协议 —— 这也让
	//	handleChat 传 chatCodec{} 是安全的。
	codec = codec.normalized()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		codec.writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Router == nil {
		codec.writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"no_provider", "尚未配置任何渠道")
		return
	}

	// 限制请求体大小
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes+1))
	if err != nil {
		codec.writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"read_body_failed", "读取请求体失败: "+err.Error())
		return
	}
	if len(body) > MaxRequestBodyBytes {
		codec.writeError(w, http.StatusRequestEntityTooLarge, openai.ErrTypeInvalidRequest,
			"body_too_large", fmt.Sprintf("请求体超过上限 %d 字节", MaxRequestBodyBytes))
		return
	}

	// 协议差异点 1：请求体形状。OpenAI 走结构体解析（保留原有校验语义），
	// Anthropic 走翻译（形状不同，必须真正转换）。
	req, err := codec.decodeRequest(body)
	if err != nil {
		codec.writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.clientModel) == "" {
		codec.writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"missing_model", "缺少 model 字段")
		return
	}
	if !req.hasMessages {
		codec.writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"missing_messages", "缺少 messages 字段")
		return
	}

	// 解析模型 → 上游 ID。
	// ⚠️ provider 实例本身不在这里用：账号选择与调用由 TryChat 负责，
	// 这样换号逻辑才不用依赖具体渠道包。
	//
	// 🔴 但要把 p.ID() 取出来记进用量（Codex 第 40 轮建议）：
	//	请求**此刻**已经知道走了哪个渠道，必须当场记下。
	//	事后再靠"当前注册表回推"会随平台增减而改写历史统计 ——
	//	接入第二个平台后就再也无法可靠还原了。
	resolvedProvider, upstreamID, _, err := deps.Router.Resolve(req.clientModel)
	if err != nil {
		codec.writeError(w, http.StatusNotFound, openai.ErrTypeInvalidRequest,
			"model_not_found", err.Error())
		return
	}
	providerID := ""
	if resolvedProvider != nil {
		providerID = resolvedProvider.ID()
	}

	// 🔴 把平台放进 context，供下游选号时过滤账号（2026-10-06）。
	//
	//	平台在这里**已经确定**（providerID 就是平台标识：
	//	"workbuddy" 或 "workbuddyai"），必须传给 TryChat ——
	//	否则多平台下可能选出另一个平台的账号，
	//	把凭据发到错误的上游域名。
	//
	//	⚠️ 必须归一成 auth 包的平台取值（"cn"/"intl"）：
	//	  providerID 是渠道前缀（"workbuddy"），与账号的 Platform
	//	  字段（"cn"）**取值不同** —— 直接传会让过滤条件永不匹配，
	//	  表现为"没有可用账号"。
	ctx := withPlatform(r.Context(), authPlatformOfProviderID(providerID))

	// 把 model 改写成上游 ID。
	//
	// 🔴 必须基于【原始字节】改写，不能用 json.Marshal(req) 回写
	// （2026-10-05 真实故障）：结构体是有损表示，content 多模态数组、
	// tools 里我们不认识的字段都会在"解成 struct 再序列化"的往返中丢失或变形。
	// 上游请求体只需要改 model 一个字段，其余原样透传最安全。
	//
	// ⚠️ 先把客户端原始模型名存下来再改写。
	// 曾直接传 req.Model 给下游函数，结果是【改写之后】的值 ——
	// 导致两处错误（2026-10-05 由 TestUsageRecordedOnAggregatePath 抓到）：
	//   1. 用量记录里 Model 变成上游 ID，用户对不上自己请求的名字；
	//   2. 流式响应的 model 字段回给客户端的是上游 ID 而非用户请求的名字。
	clientModel := req.clientModel
	raw, err := rewriteModel(req.rawBody, upstreamID)
	if err != nil {
		codec.writeError(w, http.StatusBadRequest, openai.ErrTypeInvalidRequest,
			"invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}

	chatReq := provider.ChatRequest{
		Model:           upstreamID,
		RawBody:         raw,
		Stream:          req.stream,
		ReasoningEffort: req.reasoningEffort,
	}

	if req.stream {
		streamChat(deps, ctx, w, r, chatReq, clientModel, providerID, codec)
		return
	}
	aggregateChat(deps, ctx, w, r, chatReq, clientModel, providerID, codec)
}

// streamChat 流式路径：把 canonical 事件转成 SSE 并立即 Flush。
//
// 🔴 关键顺序（Codex 第 8 轮指出，必须严格遵守）：
//
//	先 TryChat 拿到首个事件 → 确认上游可用 → 才写 HTTP 200 与 SSE 帧
//
// 绝不能在调用上游【之前】就 WriteHeader —— 那样一旦上游报错，
// 客户端已经收到 200，就无法再换号重试了（这正是重构前的缺陷）。
func streamChat(deps Deps, ctx context.Context, w http.ResponseWriter, r *http.Request,
	chatReq provider.ChatRequest, clientModel, providerID string, codec chatCodec) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		codec.writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
			"no_flusher", "服务器不支持流式响应")
		return
	}

	// ── 阶段一：建立会话（此时一个字节都还没写给客户端）──
	sess, err := TryChat(ctx, deps, deps.Chatter, chatReq)
	if err != nil {
		// 还没写任何东西 → 可以正常返回错误状态码，也可换号
		writeUpstreamErrorWith(w, err, codec)
		return
	}
	defer sess.Close()

	// 记账器：在写出响应之前建好，确保后续每条分支都能记上账。
	rec := newUsageRecorderFor(deps, clientModel, chatReq.Model, true,
		sess.AccountID(), providerID, codec.protocol)

	// ── 阶段二：确认上游可用，开始写响应 ──
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 协议差异点 2：流式编码器。OpenAI 每事件一帧；Anthropic 需要
	// 开场的 message_start 与收尾的 message_delta/message_stop，
	// 因此把"状态"交给编码器持有。
	enc := codec.newEncoder(w, flusher, chatReq.Model, clientModel, sess)
	if err := enc.start(); err != nil {
		deps.logf("写入流首帧失败（客户端可能已断开）: %v", err)
		rec.recordClientDisconnected()
		return
	}

	// 首个事件已被 TryChat 从流里取出，必须先写它 ——
	// 否则会丢掉第一个 token（通常是 role 帧）。
	if err := enc.write(sess.FirstEvent); err != nil {
		deps.logf("写入首个事件失败（客户端可能已断开）: %v", err)
		rec.recordClientDisconnected()
		return
	}

	// ── 阶段三：消费剩余事件（此时已无法换号）──
	streamErr := sess.Rest(enc.write)
	if streamErr != nil {
		// 流已经开始，改不了状态码；用协议自己的错误帧告知客户端。
		//
		// 🔴 Anthropic 路径下这里【绝不能】补 message_stop ——
		//	那等同"正常结束"，客户端会把半截回答当完整回答继续跑。
		//	Anthropic 的 Fail 只发 error 事件（见 anthropic.StreamWriter.Fail）。
		if err := enc.fail(streamErr); err != nil {
			deps.logf("写入流错误帧失败: %v", err)
		}
		deps.logf("对话流中断: %v", streamErr)
	} else if err := enc.finish(); err != nil {
		deps.logf("写入流收尾帧失败: %v", err)
	}

	// ── 记账（在流结束之后，不影响首字节）──
	//
	// 🔴 语义（Codex 第 9 轮）：
	//   - 拿到最终 usage → 记 token 数
	//   - 客户端断开     → 单独分类，token 留空（不伪造）
	//   - 其他流错误     → 记失败
	switch {
	case streamErr != nil:
		if isClientDisconnect(streamErr) {
			rec.recordClientDisconnected()
		} else {
			rec.recordError(streamErr.Error())
		}
	default:
		rec.recordOK(sess.Usage())
	}
}

// aggregateChat 非流式路径：消费上游流，聚合后返回一个 JSON。
//
// 非流式天然容易换号 —— 反正要收齐全部事件才写出。
func aggregateChat(deps Deps, ctx context.Context, w http.ResponseWriter, r *http.Request,
	chatReq provider.ChatRequest, clientModel, providerID string, codec chatCodec) {

	sess, err := TryChat(ctx, deps, deps.Chatter, chatReq)
	if err != nil {
		writeUpstreamErrorWith(w, err, codec)
		return
	}
	defer sess.Close()

	rec := newUsageRecorderFor(deps, clientModel, chatReq.Model, false,
		sess.AccountID(), providerID, codec.protocol)

	// 协议差异点 3：非流式聚合器。两者都要自己聚合（上游强制流式），
	// 但产出形状不同（OpenAI choices / Anthropic content blocks）。
	agg := codec.newAggregator(chatReq.Model, clientModel)

	// 首个事件（被 TryChat 取出的那个）也要喂给聚合器，否则会少 content
	agg.add(sess.FirstEvent)

	if err := sess.Rest(func(ev provider.Event) error {
		agg.add(ev)
		return nil
	}); err != nil {
		writeUpstreamErrorWith(w, err, codec)
		// 非流式：此时还没写任何响应体，可以正常返回错误状态码。
		// 记账仍然要写 —— 失败的请求也该在用量页留痕。
		if isClientDisconnect(err) {
			rec.recordClientDisconnected()
		} else {
			rec.recordError(err.Error())
		}
		return
	}

	// 聚合器内部持有 usage；用它记账（可能为 nil = 上游没给）
	rec.recordOK(agg.usage())

	writeJSON(w, http.StatusOK, agg.result())
}

// toOpenAIEvent 把 provider 事件转成协议层事件。
func toOpenAIEvent(ev provider.Event) *openai.Event {
	switch ev.Type {
	case provider.EventReasoning:
		return &openai.Event{Type: openai.EvReasoning, Text: ev.Text}
	case provider.EventContent:
		return &openai.Event{Type: openai.EvContent, Text: ev.Text}
	case provider.EventToolCall:
		if ev.ToolCall == nil {
			return nil
		}
		return &openai.Event{Type: openai.EvToolCall, ToolCall: &openai.ToolCallDelta{
			Index:     ev.ToolCall.Index,
			ID:        ev.ToolCall.ID,
			Name:      ev.ToolCall.Name,
			Arguments: ev.ToolCall.Arguments,
		}}
	case provider.EventUsage:
		if ev.Usage == nil {
			return nil
		}
		return &openai.Event{Type: openai.EvUsage, Usage: &openai.EventUsage{
			PromptTokens:     ev.Usage.PromptTokens,
			CompletionTokens: ev.Usage.CompletionTokens,
			TotalTokens:      ev.Usage.TotalTokens,
			ReasoningTokens:  ev.Usage.ReasoningTokens,
			Credit:           ev.Usage.Credit,
			// 🔴 缓存字段也必须传（2026-10-06 修）。
			//	这条链路有三处会漏：上游解析 → canonical → 记账。
			//	漏任何一处，面板的缓存命中率就恒为空。
			PromptCacheHitTokens:  ev.Usage.PromptCacheHitTokens,
			PromptCacheMissTokens: ev.Usage.PromptCacheMissTokens,
		}}
	case provider.EventDone:
		return &openai.Event{Type: openai.EvDone, FinishReason: ev.FinishReason}
	}
	return nil
}

// 🔴 writeUpstreamError 已迁到 protocol_codec.go 的 writeUpstreamErrorWith ——
//	那里按协议选择错误序列化形状。此处**不要**再留一份实现：
//	两份必然漂移，而错误分类（"没有可用账号 → 503"、"403 不判封号"等）
//	是踩过坑才定下的，必须只有一个权威版本。

// mustMarshal 序列化，失败时返回一个固定错误串。
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":{"message":"internal marshal error"}}`)
	}
	return append(b, '\n', '\n')
}

// newResponseID 生成响应 ID。
func newResponseID() string {
	return "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano())
}

// logf 便捷日志（deps.Logger 可能为 nil）。
func (d Deps) logf(format string, args ...any) {
	if d.Logger != nil {
		d.Logger.Printf(format, args...)
	}
}

// canonicalModelID 把用量记录里的模型名归一到对外标准 ID。
//
// 🔴 为什么要它（2026-10-06 委托方实测反馈）：同一个模型会因为客户端
// 写法不同被记成多条 —— "dsf"（别名）、"deepseek-v4.1-flash"（裸 ID）、
// "workbuddy/deepseek-v4.1-flash"（标准形态），被迫分成三行显示。
// 委托方原话：「为什么三种显示，都显示 workbuddy/deepseek-v4.1-flash 就行」。
//
// Router 为 nil 时（测试/未装配）原样返回：宁可显示原文，
// 也不要在没有别名表的情况下瞎猜前缀。
func (d Deps) canonicalModelID(id string) string {
	if d.Router == nil {
		return id
	}
	return d.Router.CanonicalModelID(id)
}

// resolveModelKey 算出用量记录该归到哪个模型 ID 下（聚合用的 key）。
//
// 两级策略（Codex 第 40 轮建议）：
//
//  1. **优先用记录里的真实路由身份**（ProviderID + ResolvedModel）。
//     这是请求当时就确定的答案，与注册表/别名表的**当前状态无关**。
//     ⇒ 接第二个平台后，这些记录的归属不会被改写。
//
//  2. 老记录（这两个字段为空，本轮之前写的）回退到 CanonicalModelID ——
//     按当前注册表回推。⚠️ 该回退只对历史数据有效；将来接入
//     同名裸 ID 的第二平台时，这些老记录可能变得有歧义。
//     这是无法完全避免的（当时确实没记），但**新记录不再有此问题**。
func (d Deps) resolveModelKey(ev usagepkg.Event) string {
	if ev.ProviderID != "" && ev.ResolvedModel != "" {
		return ev.ProviderID + "/" + ev.ResolvedModel
	}
	return d.canonicalModelID(ev.Model)
}

// 供 router 使用（避免未使用导入警告）。
var _ = router.ErrModelNotFound
