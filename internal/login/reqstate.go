// reqstate.go：凭据请求的**状态机**（Codex 第 31 轮指定的关联方式）。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么不再用"多个互不关联的 map"
// ═══════════════════════════════════════════════════════════════════
//
// 原实现用三个独立容器：candidates / requestURLs / requestMethods。
// 诊断时暴露出它的根本问题：**无法区分两种完全不同的失败** ——
//
//	① 请求元数据缺失（还没收到 requestWillBeSent）→ 关联问题
//	② 真实方法不是 POST（重定向变 GET / OPTIONS 预检）→ 上游行为
//
// 两者都被报成"方法不是 POST"。Codex 第 31 轮原文：
//
//	「现在"不是 POST"把缺少关联信息和真正的方法不符混在一起了。」
//
// 所以改成**每个 requestId 一条完整状态**，条件齐了才判定。
//
// ═══════════════════════════════════════════════════════════════════
// Codex 指定的规则（逐条落实）
// ═══════════════════════════════════════════════════════════════════
//
//	✅ requestWillBeSent：登记 URL、method；发现目标时标记候选
//	✅ responseReceived ：登记响应 URL、status；**不取 body**
//	✅ loadingFinished  ：条件都满足才取 body（在独立 goroutine 里）
//	✅ loadingFailed    ：终止并清理**该 requestId**
//	✅ 响应到了但请求元数据缺失 → **保留为待关联状态**，
//	   不能把空方法当 POST，也不要立即定性为 GET；后续事件里重新检查
//	✅ 状态更新串行（同一把锁）；取 body 前设 bodyFetchStarted 防重复
//	✅ **锁外**调用 CDP（否则会重新引入"读循环等自己"的死锁）
//	✅ **重定向会复用 requestId**：新的 requestWillBeSent 必须覆盖
//	   上一跳的 method/URL，不能拿上一跳的 POST 去批准最终的 GET
//	✅ 有限的上限与清理，避免残留（本项目红线 60 MB）
package login

import (
	"sync"
)

// reqStateMax 是同时跟踪的 requestId 上限。
//
// ponytail: 只跟踪"路径像目标端点"的请求，正常远小于此值。
// 升级触发条件：若实测有站点在登录流程里产生大量同路径请求导致误清，
// 再改为按时间淘汰；当前上限足够且能防异常增长。
const reqStateMax = 64

// reqState 是一个 requestId 的完整跟踪状态。
//
// 🔴 各字段的可见性规则（Codex 要求）：
//   - method 为空 = **元数据尚未到达**，绝不可当成 POST，
//     也不可立即定性为 GET —— 要等后续事件补齐。
//   - 只有 method / responseURL / finished 三者齐备且校验通过，
//     才允许取 body。
type reqState struct {
	// 请求侧
	requestSeen bool
	method      string
	requestURL  string

	// 响应侧
	responseSeen bool
	responseURL  string
	status       int

	// 完成侧
	finished         bool
	failed           bool
	bodyFetchStarted bool
}

// readyForBody 报告"是否已满足取 body 的全部前置条件"。
//
// Codex 要求：方法、响应端点、完成条件**都**满足才取 body。
// ⚠️ 这里**不**做端点匹配（匹配需要 state，由调用方做），
// 只判断结构上的就绪。
func (s reqState) readyForBody() bool {
	return s.requestSeen && s.method != "" &&
		s.responseSeen && s.finished &&
		!s.failed && !s.bodyFetchStarted
}

// reqTracker 管理所有被跟踪的请求状态。
//
// 并发：事件回调（readLoop 线程）与取 body 的 goroutine 都会访问，
// 所以全部状态变更都在同一把锁内完成（Codex：「状态更新保持串行」）。
type reqTracker struct {
	mu     sync.Mutex
	states map[string]*reqState
	order  []string // 登记顺序，用于按上限淘汰最旧的
}

func newReqTracker() *reqTracker {
	return &reqTracker{states: map[string]*reqState{}}
}

// onRequestWillBeSent 登记请求侧信息。
//
// 🔴 重定向会**复用 requestId**：此时必须**覆盖**上一跳的 method 与 URL，
// 否则会拿上一跳的 POST 去批准最终的 GET（Codex 明确警告）。
func (t *reqTracker) onRequestWillBeSent(reqID, method, rawURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.states[reqID]
	if s == nil {
		s = &reqState{}
		t.states[reqID] = s
		t.order = append(t.order, reqID)
		t.evictLocked()
	}
	// 覆盖（不是"仅在空时赋值"）—— 处理重定向复用 requestId 的情形。
	s.requestSeen = true
	s.method = methodOrMissing(method)
	s.requestURL = rawURL
}

// onResponseReceived 登记响应侧信息（**不取 body**）。
func (t *reqTracker) onResponseReceived(reqID, rawURL string, status int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.states[reqID]
	if s == nil {
		s = &reqState{}
		t.states[reqID] = s
		t.order = append(t.order, reqID)
		t.evictLocked()
	}
	s.responseSeen = true
	s.responseURL = rawURL
	s.status = status
}

// onLoadingFinished 标记该请求完成；返回"是否已就绪可取 body"。
//
// ⚠️ 返回 true 只表示**结构就绪**，端点匹配仍由调用方判定。
// 若此时元数据尚未到齐（Codex 指出的情形），返回 false 并**保留状态**，
// 等后续 requestWillBeSent 到达后再由调用方复查。
func (t *reqTracker) onLoadingFinished(reqID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.states[reqID]
	if s == nil {
		return false
	}
	s.finished = true
	return s.readyForBody()
}

// onLoadingFailed 终止并清理该 requestId（Codex 要求）。
func (t *reqTracker) onLoadingFailed(reqID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, reqID)
	t.removeOrderLocked(reqID)
}

// markBodyFetchStarted 在**取 body 之前**置位，防止重复启动。
//
// 返回 false 表示"已经启动过或状态不存在"，调用方应放弃。
func (t *reqTracker) markBodyFetchStarted(reqID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.states[reqID]
	if s == nil || s.bodyFetchStarted {
		return false
	}
	s.bodyFetchStarted = true
	return true
}

// snapshot 返回某 requestId 的状态副本（用于锁外判定）。
func (t *reqTracker) snapshot(reqID string) (reqState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.states[reqID]
	if !ok {
		return reqState{}, false
	}
	return *s, true
}

// evictLocked 按上限淘汰最旧的记录（调用方须持锁）。
func (t *reqTracker) evictLocked() {
	for len(t.order) > reqStateMax {
		oldest := t.order[0]
		t.order = t.order[1:]
		delete(t.states, oldest)
	}
}

// removeOrderLocked 从顺序表里移除一个 requestId（调用方须持锁）。
func (t *reqTracker) removeOrderLocked(reqID string) {
	for i, id := range t.order {
		if id == reqID {
			t.order = append(t.order[:i], t.order[i+1:]...)
			return
		}
	}
}
