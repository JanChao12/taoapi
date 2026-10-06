// sighting.go：登录失败原因的**可观测性**。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要这个文件（2026-10-05 排障的真实教训）
// ═══════════════════════════════════════════════════════════════════
//
// 凭据捕获这条链路的**每一种失败都是静默的**：
//
//   - URL 匹配不上        → `return`（什么都不说）
//   - 取 body 失败        → `return`（什么都不说）
//   - loadingFinished 对不上 requestId → `return`
//
// 外部**只能看到**"等待登录超时（10m0s）"。
//
// 后果：我在排障时连续误判了三次 —— 把 `http` 被 `https` 校验拒绝、
// 把自己诊断工具的竞态，都当成了生产缺陷。每次都要重新抓包、
// 重新猜，代价是几十轮往返。
//
// ⇒ 所以这里把"见过什么、为什么拒绝"记成一个**极小的**诊断摘要，
// 只用于让超时错误能说清原因。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 安全：只记原因，绝不记内容
// ═══════════════════════════════════════════════════════════════════
//
// 明确**不记录**：
//   - 完整 URL（含 state —— 虽然 state 非秘密，但没有必要留）
//   - 任何响应体内容
//   - 任何 token（连长度都不记）
//
// 只记录：拒绝的原因分类 + 次数。这些是**常量字符串**，
// 因此可以安全地进入错误信息与 API 响应。
package login

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// sightingLog 汇总"见到过但被拒绝"的情形，用于诊断。
//
// 并发安全：事件回调与取 body 的 goroutine 都会写它。
type sightingLog struct {
	mu sync.Mutex

	// rejectedReasons 是"像凭据端点但被拒绝"的原因 -> 次数。
	// 键是常量字符串（见 rejectReason*）。
	rejectedReasons map[string]int

	// fetchFailed 是"匹配成功但取 body 失败"的原因 -> 次数。
	fetchFailed map[string]int

	// eventCounts 是"收到过哪些 CDP 事件类型" -> 次数（纯诊断）。
	eventCounts map[string]int

	// seenPaths 是"responseReceived 的 path 部分" -> 次数（纯诊断）。
	// 🔴 只存 path，不存 host/query。
	seenPaths map[string]int

	// unmarshalFails 是"某类事件的 params 解析失败"-> 次数（纯诊断）。
	unmarshalFails map[string]int

	// methodRejects 是"方法校验未通过时的**实际方法**"-> 次数（纯诊断）。
	// Codex 第 31 轮要求：必须能区分"元数据缺失"与"真实方法不符"。
	methodRejects map[string]int

	// accepted 是成功进入候选登记的次数。
	accepted int
}

// 拒绝原因（**常量**，可安全进入错误信息）。
//
// ⚠️ 必须与 `credMatcher.rejectReason` 的返回**一一对应**，
// 否则诊断会指向错误方向 —— 有元测试（TestRejectReasonAgreesWithMatcher）钉住。
const (
	// rejectUnparsable URL 无法解析。
	rejectUnparsable = "URL 无法解析"
	// rejectScheme 不是 https。
	// **这是实测中最常见的一种**：本地假服务端是 http，被生产校验正确拒绝。
	rejectScheme = "非 https"
	// rejectHost host 不匹配。
	rejectHost = "host 不匹配"
	// rejectPath 路径不匹配。
	rejectPath = "路径不匹配"
	// rejectMethod 方法不是 POST（Codex 第 29 轮要求校验方法）。
	rejectMethod = "方法不是 POST"
	// rejectState state 与本次不一致（或缺失/重复）。
	rejectState = "state 不匹配"
)

func newSightingLog() *sightingLog {
	return &sightingLog{
		rejectedReasons: map[string]int{},
		fetchFailed:     map[string]int{},
		eventCounts:     map[string]int{},
		seenPaths:       map[string]int{},
		unmarshalFails:  map[string]int{},
		methodRejects:   map[string]int{},
	}
}

// urlPathOnly 取出 URL 的 path 部分；解析失败时返回一个占位符。
//
// 🔴 刻意不返回原始串 —— 避免把 query（可能含 state）带进诊断。
func urlPathOnly(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return "(无法解析)"
	}
	return u.Path
}

// observe 检查一条响应，若"形似凭据端点但被拒绝"则记录原因。
//
// ⚠️ 判定必须与真实匹配**共用同一份规则**（credMatcher），
// 否则诊断会指向错误方向 —— 那比没有诊断更糟（已踩过）。
func (s *sightingLog) observe(m *credMatcher, rawURL, method, wantState string) {
	// 只关心"路径像凭据端点"的请求，其它请求（CDN/埋点）统统忽略，
	// 避免噪音淹没真正的原因。
	if !pathLooksLikeCredential(rawURL) {
		return
	}

	reason := m.rejectReason(rawURL, method, wantState)
	if reason == "" {
		s.observeAccepted()
		return
	}
	s.observeRejected(reason)
}

// observeRejected 记一次"形似凭据端点但被拒绝"（原因须是常量）。
//
// Codex 第 31 轮要求把两种情况**分开**：
//
//	request_metadata_missing —— 还没收到 requestWillBeSent，方法未知
//	unexpected_method        —— 收到了方法，但不是 POST（要记录**实际**方法）
//
// 原先两者都报成"方法不是 POST"，无法区分（正是这一点让排障卡住）。
func (s *sightingLog) observeRejected(reason string) {
	s.mu.Lock()
	s.rejectedReasons[reason]++
	s.mu.Unlock()
}

// observeRejectedMethod 专门记录"方法校验未通过"的**实际方法值**。
//
// 🔴 只记方法名（常量集合：GET/POST/OPTIONS/…），不记 URL、不记 header。
// 空串显式记为 "(缺失)" —— 这正是 Codex 要求的区分点。
func (s *sightingLog) observeRejectedMethod(actual string) {
	key := actual
	if key == "" {
		key = "(缺失)"
	}
	s.mu.Lock()
	if s.methodRejects == nil {
		s.methodRejects = map[string]int{}
	}
	s.methodRejects[key]++
	s.mu.Unlock()
}

// noteEvent 记录**收到过哪些事件类型**（纯诊断，用于区分"事件没到"与"过滤没中"）。
//
// 为什么需要：2026-10-05 排查时，"sighting 摘要为空"这一现象
// 既可能是"事件根本没到"，也可能是"到了但被更早的分支丢弃"，
// 而这两种的修法完全不同。只统计计数，不含任何内容。
func (s *sightingLog) noteEvent(method string) {
	s.mu.Lock()
	if s.eventCounts == nil {
		s.eventCounts = map[string]int{}
	}
	s.eventCounts[method]++
	s.mu.Unlock()
}

// noteRespURL 记录**每一条 responseReceived 的路径**（纯诊断）。
//
// 🔴 只记录 URL 的 **path 部分**，不记 host/端口/query ——
// 既够用来判断"是不是我们的端点"，又不留任何可能含敏感值的部分。
//
// 为什么需要：诊断曾显示"收到 responseReceived×1 但未见到目标端点"，
// 却无法区分"路径确实不同"与"我们的识别函数写错了"。
// 记下实际路径，这个二义性立刻消失（已踩过一次）。
func (s *sightingLog) noteRespURL(rawURL string) {
	s.mu.Lock()
	if s.seenPaths == nil {
		s.seenPaths = map[string]int{}
	}
	p := urlPathOnly(rawURL)
	// 空 URL 也要记 —— "收到了 responseReceived 但 url 字段是空的"
	// 本身就是一种必须看得见的异常。
	if p == "" {
		p = "(空)"
	}
	s.seenPaths[p]++
	s.mu.Unlock()
}

// noteUnmarshalFail 记录"某类事件的 params 解析失败"（纯诊断）。
//
// 记下**错误文本**（截断）—— 否则只知道"失败了"，不知道"为什么失败"，
// 又得重跑一轮（2026-10-05 已因此多绕一圈）。
// ⚠️ 错误文本不含凭据（JSON 语法错误的提示只涉及结构，不含字段值）。
func (s *sightingLog) noteUnmarshalFail(method string, err error) {
	s.mu.Lock()
	if s.unmarshalFails == nil {
		s.unmarshalFails = map[string]int{}
	}
	key := method
	if err != nil {
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		key = method + ": " + msg
	}
	s.unmarshalFails[key]++
	s.mu.Unlock()
}

// observeAccepted 记一次匹配成功。
func (s *sightingLog) observeAccepted() {
	s.mu.Lock()
	s.accepted++
	s.mu.Unlock()
}

// noteFetchFailed 记录一次"匹配成功但取 body 失败"。
func (s *sightingLog) noteFetchFailed(reason string) {
	s.mu.Lock()
	s.fetchFailed[reason]++
	s.mu.Unlock()
}

// summary 返回一句**可安全外传**的诊断摘要；无异常时返回空串。
func (s *sightingLog) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.rejectedReasons) == 0 && len(s.fetchFailed) == 0 {
		// 无拒绝也无取 body 失败 —— 但若连事件都没收到，
		// 那就是"监听本身没工作"，必须说出来（否则只看到"超时"）。
		if len(s.eventCounts) == 0 {
			return "未收到任何 CDP 事件（Network 域可能未生效）"
		}
		out := "收到 CDP 事件但未见到目标端点（" + formatCounts(s.eventCounts) + "）"
		if len(s.seenPaths) > 0 {
			// 把实际见过的 path 列出来 —— 用于区分"路径确实不同"
			// 与"识别函数写错了"（这两种的修法完全不同）。
			out += "；responseReceived 的 path: " + formatCounts(s.seenPaths)
		}
		if len(s.unmarshalFails) > 0 {
			// params 解析失败会让"看起来收到了事件、实际什么都没处理"，
			// 必须显式暴露，否则又是静默失败。
			out += "；params 解析失败: " + formatCounts(s.unmarshalFails)
		}
		return out
	}

	out := ""
	if len(s.rejectedReasons) > 0 {
		out += "见到凭据端点形态的请求但被拒绝（"
		out += formatCounts(s.rejectedReasons)
		out += "）"
	}
	if len(s.methodRejects) > 0 {
		// Codex 第 31 轮要求：把**实际方法**说出来，
		// 以便区分"元数据缺失（值为 (缺失)）"与"真实方法不符"。
		if out != "" {
			out += "；"
		}
		out += "方法校验未通过，实际方法: " + formatCounts(s.methodRejects)
	}
	if len(s.fetchFailed) > 0 {
		if out != "" {
			out += "；"
		}
		out += "已匹配但取响应体失败（"
		out += formatCounts(s.fetchFailed)
		out += "）"
	}
	if s.accepted > 0 {
		out += "；另有 " + strconv.Itoa(s.accepted) + " 次匹配成功"
	}
	return out
}

// pathLooksLikeCredential 判断 URL 是否"像"凭据端点（宽松）。
//
// 只用于决定"要不要记录诊断"，**不参与任何接受/拒绝判定**。
func pathLooksLikeCredential(rawURL string) bool {
	return strings.Contains(rawURL, "login/enterprise")
}

// classifyFetchErr 把取 body 的错误归成短原因（不含 URL/token）。
func classifyFetchErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "超时"):
		return "CDP 调用超时"
	case strings.Contains(msg, "会话已关闭"):
		return "CDP 会话已关闭"
	case strings.Contains(msg, "base64"):
		return "base64 解码失败"
	default:
		return "其它 CDP 错误"
	}
}

// lastCaptureSighting 返回最近一次捕获的诊断摘要（**只读旁路**）。
//
// 用途：让测试能在失败时打印"为什么没抓到"，而不必把 sighting 暴露成
// 生产 API。生产代码**不读**它（它的值只由 captureCredentials 写入）。
//
// ⚠️ 这是观察通道，不是控制通道 —— 读它不会改变任何判定。
func lastCaptureSighting() string {
	lastSightingMu.Lock()
	defer lastSightingMu.Unlock()
	if lastSighting == nil {
		return ""
	}
	return lastSighting.summary()
}

var (
	lastSightingMu sync.Mutex
	lastSighting   *sightingLog
)

// publishSighting 让捕获流程发布自己的 diagnostics（供测试读取）。
func publishSighting(s *sightingLog) {
	lastSightingMu.Lock()
	lastSighting = s
	lastSightingMu.Unlock()
}

// formatCounts 把 "原因->次数" 拼成 "原因×N、原因×M"。
//
// 排序固定（按原因名的字典序）以保证输出稳定、便于比对。
func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := ""
	for i, k := range keys {
		if i > 0 {
			out += "、"
		}
		out += k + "×" + strconv.Itoa(m[k])
	}
	return out
}
