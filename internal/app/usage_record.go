// usage_record.go：把每次请求的用量写进 JSONL。
//
// 🔴 本文件补的是一个【真实缺口】（2026-10-05 委托人问"用量统计实现了吗"）：
//
//	此前只有读的一端 —— stats.go 调 deps.Usage.Read(...)，
//	serve.go 也构造了 usagepkg.NewStore("")，但【全仓库没有任何地方调 Append】。
//	后果：用量页永远显示"暂无数据"，即使真的跑过对话。
//	读的一端一直是对的，缺的是写。
//
// 设计遵循 Codex 第 9 轮评审的四条要求：
//
//  1. 【不伪造 usage】客户端中途断开、没拿到上游最终 usage 时，
//     绝不能把中途累计的分片当最终值写进去。此时写 usageKnown=false
//     且 token 字段留 0，让查询方能把这些记录排除在 token 统计外。
//  2. 【同步写、每请求一次】放在上游终止、usage 已确定之后。
//     它不影响流式首字节（那时早就 flush 过了）。
//  3. 【写失败不能把成功的对话变成 500】只记日志。
//     用户拿到了回答，不能因为我们记账失败就告诉他请求失败。
//  4. 【并发锁】由 usage.Store.Append 内部的 mutex 保证，避免 JSONL 交错。
package app

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"time"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// usageRecorder 收集一次请求的记账信息，最后一次性写出。
//
// 为什么用结构体而不是在两条路径里各写一遍 Append 调用：
// 流式与非流式收集到的东西不同（流式只有事件、非流式还有聚合器），
// 但写出的字段必须一致 —— 否则用量页的同一个模型会出现两套口径。
// 这个结构体就是那个统一口径。
type usageRecorder struct {
	deps Deps

	// requestID 关联同一次请求（Codex 建议：便于将来做去重与排查）。
	requestID string

	// clientModel 是客户端请求的模型名（可能命中别名）。
	clientModel string

	// upstreamModel 是解析后真正转发给上游的模型名。
	//
	// Codex 第 9 轮要求"统计同时记录请求模型名与解析后的模型"：
	// 否则用了别名的请求在用量页里会显示成上游 ID，用户对不上自己填的别名。
	upstreamModel string

	// providerID 是实际处理该请求的渠道（Codex 第 40 轮要求）。
	//
	// 🔴 必须**当场**记下：路由此刻已经解析出答案了。
	// 若只存客户端写的模型名，事后再靠"当前注册表"回推渠道，
	// 一旦接入第二个平台（可能有同名裸 ID），历史统计就无法可靠还原。
	providerID string

	stream bool
	start  time.Time

	account string

	// protocol 入站协议名（chat / responses / messages）。
	//
	// 🔴 为什么要记（2026-10-09 加 Anthropic 支持）：用量页按模型聚合，
	//	而同一个模型可能被 OpenAI 客户端与 Anthropic 客户端分别调用。
	//	不区分协议的话，排查"是不是某个协议的转换有问题"时无从下手 ——
	//	usage.Event.Protocol 字段早就存在，只是此前恒为 "chat"。
	protocol string

	// ttftMS 首字耗时（毫秒）；nil = 没测到（非流式 / 首字前就失败）。
	//
	// 见 noteFirstToken 与 usage.Event.TTFTMS 的说明。
	ttftMS   *int64
	ttftSeen bool
}

// newUsageRecorder 开始一次记账（OpenAI Chat 协议）。
//
// 保留这个签名是为了不动已有的两条调用路径与测试；
// 新协议用 newUsageRecorderFor 显式传协议名。
func newUsageRecorder(deps Deps, clientModel, upstreamModel string,
	stream bool, account, providerID string) *usageRecorder {
	return newUsageRecorderFor(deps, clientModel, upstreamModel, stream, account, providerID, "chat")
}

// newUsageRecorderFor 开始一次记账（可指定协议名）。
//
// ⚠️ start 取**此刻**。仅适用于"创建时就是请求起点"的场景；
// 流式/非流式 chat 路径**必须**用 newUsageRecorderFrom 传入真正的起点
// （见那里的说明 —— 起点写错会让首字耗时恒为 0ms）。
func newUsageRecorderFor(deps Deps, clientModel, upstreamModel string,
	stream bool, account, providerID, protocol string) *usageRecorder {
	return newUsageRecorderFrom(deps, clientModel, upstreamModel, stream,
		account, providerID, protocol, time.Now())
}

// newUsageRecorderFrom 用**调用方指定的起点**建记账器。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么需要它（2026-10-09 委托方实测缺陷：首字全是 0ms）
// ═══════════════════════════════════════════════════════════════════
//
//	原实现里 chat.go 是这样写的：
//
//	    sess, err := TryChat(...)                    // ① 阻塞，直到上游首个事件到达
//	    rec := newUsageRecorderFor(...)              // ② 此刻才开始计时
//
//	而 TryChat **内部已经**把上游的首个事件取回来了（它的职责就是
//	"先确认上游可用，再写 HTTP 200"）。所以 ② 的 start 已经晚于
//	"首字到达"那个时刻 —— `noteFirstToken()` 量到的只是
//	「TryChat 返回 → 写首个事件」这几毫秒 ⇒ **恒等于 0ms**。
//
//	⚠️ 同一个起点错误还让 **DurationMS 也偏小**：TryChat 里那段
//	  （选号 + 建连 + 等上游首个事件）被排除在总耗时之外。
//	  委托方看到的 `480ms` 这类总耗时其实不含上游等待。
//
//	修法：把 `time.Now()` 提到 **TryChat 之前**，经本函数传进来。
//	这样两个指标才真正覆盖"用户等待的完整时间"。
//
// 为什么要新开一个函数而不是改 newUsageRecorderFor 的签名：
//
//	后者被 20+ 处测试直接调用（它们不关心起点，用"此刻"即可）。
//	新函数让**生产路径显式传起点**，测试路径保持简洁，
//	两边的意图都清楚，且不必改一堆无关测试。
func newUsageRecorderFrom(deps Deps, clientModel, upstreamModel string,
	stream bool, account, providerID, protocol string, start time.Time) *usageRecorder {
	if protocol == "" {
		protocol = "chat"
	}
	return &usageRecorder{
		deps:          deps,
		requestID:     newResponseID(),
		clientModel:   clientModel,
		upstreamModel: upstreamModel,
		providerID:    providerID,
		stream:        stream,
		start:         start,
		account:       account,
		protocol:      protocol,
	}
}

// RequestID 暴露给调用方（例如写进响应头便于排查）。
func (r *usageRecorder) RequestID() string { return r.requestID }

// noteFirstToken 记录首字到达时刻（只在流式路径调用，且只生效一次）。
//
// 🔴 为什么由调用方在**写出首个正文事件时**调用，而不是在
//
//	recordOK 里靠时间差算（2026-10-09 加首字耗时时定的）：
//
//	recordOK 只在请求**结束时**执行，那时已经无从知道"第一个 token
//	是何时到的"。首字时刻必须在它发生的当下就记下来 ——
//	事后无法重建。
//
// ⚠️ 只认第一次：上游首个事件通常是 role 帧（无正文），
//
//	真正的"首字"是第一个带内容的事件。多次调用只取最早那次，
//	保证语义是"第一次有东西给用户看"。
func (r *usageRecorder) noteFirstToken() {
	if r == nil || r.ttftSeen {
		return
	}
	r.ttftSeen = true
	ms := time.Since(r.start).Milliseconds()
	if ms < 0 {
		ms = 0 // 时钟回拨的兜底：宁可记 0 也不写负数
	}
	r.ttftMS = &ms
}

// recordOK 记一次成功的请求。
//
// u 为 nil 表示上游没给 usage —— 此时按"未知"处理，不写 token 数。
func (r *usageRecorder) recordOK(u *openai.EventUsage) {
	ev := r.base()
	ev.OK = true
	if u == nil {
		ev.Status = usagepkg.StatusNoUsage
		ev.UsageKnown = false
	} else {
		ev.Status = usagepkg.StatusOK
		ev.UsageKnown = true
		ev.PromptTokens = u.PromptTokens
		ev.CompletionTokens = u.CompletionTokens
		ev.TotalTokens = u.TotalTokens
		ev.ReasoningTokens = u.ReasoningTokens
		ev.Credit = u.Credit
		// 🔴 缓存命中/未命中（2026-10-06 修）。
		//
		//	原注释写的是"上游只给 prompt 总数，没有单独的命中字段，
		//	故不臆造" —— **那个判断是错的**：
		//	上游 SSE 里确实有 `prompt_cache_hit_tokens` /
		//	`prompt_cache_miss_tokens`（见 workbuddy/sse.go 的
		//	upstreamUsage），只是中间两层的结构体没带这两个字段，
		//	所以一路都是 0，面板命中率恒为空。
		//
		//	"宁缺勿假"的原则没变 —— 这里只是把**真实存在**的值传下来；
		//	上游真的没给时它们仍是 0，前端据"分母为 0"显示 —。
		ev.CacheHitTokens = u.PromptCacheHitTokens
		ev.CacheMissTokens = u.PromptCacheMissTokens
	}
	r.write(ev)
}

// recordClientDisconnected 记一次"客户端提前断开"。
//
// 🔴 这不是服务端故障，不该计入失败率（Codex 明确要求单独成一类）。
// 且此时【通常拿不到】最终 usage，所以 token 数留 0、usageKnown=false。
func (r *usageRecorder) recordClientDisconnected() {
	ev := r.base()
	ev.OK = false
	ev.Status = usagepkg.StatusClientDisconnected
	ev.UsageKnown = false
	ev.Error = "客户端提前断开"
	r.write(ev)
}

// recordError 记一次失败（上游报错、流异常等）。
func (r *usageRecorder) recordError(msg string) {
	ev := r.base()
	ev.OK = false
	ev.Status = usagepkg.StatusUpstreamError
	ev.UsageKnown = false
	ev.Error = truncateErr(msg)
	r.write(ev)
}

// base 填充两条路径共有的字段。
func (r *usageRecorder) base() usagepkg.Event {
	return usagepkg.Event{
		Time:       r.start,
		Account:    r.account,
		Model:      r.clientModel,
		Protocol:   r.protocol,
		Stream:     r.stream,
		DurationMS: time.Since(r.start).Milliseconds(),
		// 首字耗时：nil 表示没测到（非流式、或首字前就失败了）。
		// ⚠️ 直接传指针（不取值）—— "没测到"与"0ms"必须能区分。
		TTFTMS:    r.ttftMS,
		RequestID: r.requestID,
		// 🔴 记录**真实路由结果**（Codex 第 40 轮建议）。
		//   有了这两项，聚合时不必再靠注册表回推渠道 ——
		//   将来接入第二个平台也不会改写历史统计。
		ProviderID:    r.providerID,
		ResolvedModel: r.upstreamModel,
	}
}

// write 落盘。失败只记日志 —— 绝不因此改变已发出的响应。
func (r *usageRecorder) write(ev usagepkg.Event) {
	if r.deps.Usage == nil {
		return
	}
	if err := r.deps.Usage.Append(ev); err != nil {
		// 用户已经拿到回答，记账失败不能反过来让请求"失败"
		r.deps.logf("用量记录写入失败（不影响本次请求）: %v", err)
	}
}

// truncateErr 限制错误文本长度，避免把整篇上游响应写进 JSONL。
func truncateErr(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// isClientDisconnect 判断错误是否由"客户端提前断开"引起。
//
// 为什么要单独判（Codex 第 9 轮）：客户端主动断开【不是服务端故障】，
// 若把它计入失败率，用量页的成功率会被用户自己的行为污染
// （比如用户在 DSH 里点了停止，或切换了会话）。
//
// 判据取自 Go 网络栈的实际行为：
//   - 写响应失败时 http 包会给出 ErrAbortHandler 或 "broken pipe"
//   - 对端关闭读端常见 "connection reset by peer"
//   - 读上游时 contexts 被取消常表现为 context.Canceled
//
// 宁可少判（漏判只是让分类不够精细），不可多判（错判会掩盖真实故障）——
// 所以这里只匹配明确的特征串，不做宽泛的"包含 error 就算"。
func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, pat := range []string{
		"broken pipe",
		"connection reset by peer",
		"client disconnected",
		"http: abort",
		"context canceled",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}
