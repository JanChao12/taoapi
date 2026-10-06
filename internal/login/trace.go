// trace.go：脱敏的 CDP 事件轨迹（**可观测性**，Codex 第 31 轮要求）。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要（Codex 第 31 轮原文要点）
// ═══════════════════════════════════════════════════════════════════
//
//	「建议立即补一份脱敏事件轨迹：
//	   接收序号、session、requestId、事件类型、host/path、
//	   method、是否含 redirectResponse、响应 status
//	  拒绝原因区分为：
//	   request_metadata_missing
//	   unexpected_method（记录实际方法）
//	  不要打印 query、Authorization 或响应凭据。」
//
// 起因：诊断只说"方法不是 POST"，把**两种完全不同的情况**混在了一起：
//
//	① 请求元数据缺失（还没收到 requestWillBeSent）→ 是关联问题
//	② 真实方法确实不是 POST（重定向变 GET / OPTIONS 预检）→ 是上游行为
//
// 这两种的修法完全不同，必须能区分。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 脱敏纪律
// ═══════════════════════════════════════════════════════════════════
//
// 记录里**只允许**出现：
//   - 事件类型（常量）
//   - requestId（CDP 内部标识，非凭据）
//   - host/path（**不含 query** —— query 可能含 state）
//   - HTTP 方法（常量集合）
//   - 响应状态码
//   - 是否有 redirectResponse（布尔）
//
// **绝不记录**：query、Authorization、任何 header 值、响应体、token。
//
// 轨迹有**上限**（防止登录过程的上百个请求把内存撑爆 —— 本项目红线 60 MB）。
package login

import (
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// traceMaxEntries 是轨迹条数上限。
//
// ponytail: 只留最近 N 条。升级触发条件：若排障时发现需要更长的历史，
// 再改成环形缓冲 + 落盘；当前 N=64 对"一次登录"足够。
const traceMaxEntries = 64

// traceEntry 是一条脱敏的事件记录。
type traceEntry struct {
	Seq    int    // 接收序号
	Event  string // 事件类型（常量）
	ReqID  string // CDP requestId
	Host   string // 不含端口的 host
	Path   string // 不含 query 的 path
	Method string // HTTP 方法（可能为空 = 尚未收到 requestWillBeSent）
	Status int    // 响应状态（0 = 无）
	Redir  bool   // 是否带 redirectResponse
}

// String 输出**单行**脱敏文本。
func (e traceEntry) String() string {
	m := e.Method
	if m == "" {
		m = "(无)"
	}
	s := "#" + strconv.Itoa(e.Seq) + " " + e.Event
	if e.Host != "" || e.Path != "" {
		s += " " + e.Host + e.Path
	}
	s += " method=" + m
	if e.Status != 0 {
		s += " status=" + strconv.Itoa(e.Status)
	}
	if e.Redir {
		s += " redirect=1"
	}
	if e.ReqID != "" {
		s += " reqID=" + e.ReqID
	}
	return s
}

// eventTrace 是脱敏轨迹的环形记录。
type eventTrace struct {
	mu   sync.Mutex
	seq  int
	buf  []traceEntry
	drop int // 因超限被丢弃的条数（要如实报告，不能假装没发生）
}

func newEventTrace() *eventTrace { return &eventTrace{} }

// add 追加一条记录。
func (t *eventTrace) add(e traceEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	e.Seq = t.seq
	t.buf = append(t.buf, e)
	if len(t.buf) > traceMaxEntries {
		// 丢弃最旧的，并如实计数。
		drop := len(t.buf) - traceMaxEntries
		t.buf = t.buf[drop:]
		t.drop += drop
	}
}

// dump 返回全部记录的文本（多行）。
func (t *eventTrace) dump() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.buf)+1)
	if t.drop > 0 {
		out = append(out, "(更早的 "+strconv.Itoa(t.drop)+" 条已丢弃)")
	}
	for _, e := range t.buf {
		out = append(out, e.String())
	}
	return out
}

// splitHostPath 把一个 URL 拆成"不含端口的 host"与"不含 query 的 path"。
//
// 🔴 刻意丢掉 query —— 它可能含 state，不该进轨迹。
func splitHostPath(rawURL string) (host, path string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "(无法解析)"
	}
	h := u.Hostname() // 不含端口
	if h == "" {
		h = u.Host
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return h, p
}

// traceRedactedPathWithRaw 判断 URL 的 path 是否"看起来像目标端点"。
//
// 复用 pathLooksLikeCredential，保持与生产识别一致。
func traceLooksInteresting(rawURL string) bool {
	return pathLooksLikeCredential(rawURL)
}

// truncateForTrace 截断过长的标识（防异常对端塞超长 requestId）。
func truncateForTrace(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// methodOrMissing 把空方法显式标成"缺失"，而不是留给调用方猜。
func methodOrMissing(m string) string {
	if strings.TrimSpace(m) == "" {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(m))
}
