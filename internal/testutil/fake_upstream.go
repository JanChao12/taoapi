// Package testutil 提供假上游与测试样本。
//
// 核心职责：把「协议契约」变成【可断言的测试】，而不是靠代码审查记忆。
//
// 🔴 本包最重要的断言是安全红线：
//   - chat 请求绝不能带 X-Refresh-Token
//   - 必须携带必需身份头
//   - 必须 stream:true（上游拒绝非流式）
package testutil

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fixtures 嵌入真实上游样本，保证 go test 在任意工作目录都能跑。
//
//go:embed fixtures/*
var fixtures embed.FS

// Fixture 读取一个嵌入的测试样本。
func Fixture(name string) []byte {
	b, err := fixtures.ReadFile("fixtures/" + name)
	if err != nil {
		panic(fmt.Sprintf("testutil: 找不到 fixture %q: %v", name, err))
	}
	return b
}

// FixtureString 以字符串返回样本。
func FixtureString(name string) string { return string(Fixture(name)) }

// FixtureLines 按行拆分（保留空行，用于回放 SSE）。
func FixtureLines(name string) []string {
	raw := strings.ReplaceAll(FixtureString(name), "\r\n", "\n")
	return strings.Split(raw, "\n")
}

// RecordedRequest 记录假上游收到的一次请求，供断言。
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Header 便捷取值。
func (r RecordedRequest) Get(key string) string { return r.Header.Get(key) }

// FakeUpstream 是一个可断言的假上游。
type FakeUpstream struct {
	*httptest.Server

	t        *testing.T
	mu       sync.Mutex
	requests []RecordedRequest

	// SSEBody 是流式响应要回放的内容（原始 SSE 文本）。
	SSEBody string

	// JSONBody 是非流式（目录/额度等）响应的内容。
	JSONBody []byte

	// StatusCode 强制返回的状态码；0 表示 200。
	StatusCode int

	// FailOnViolation 为 true 时，违反契约立即让测试失败。
	FailOnViolation bool

	// violations 记录检测到的契约违反（供元测试检查安全网本身是否有效）。
	violations []string

	// pathStatus 按路径覆盖状态码（如只让 chat 返回 401，目录保持正常）。
	pathStatus map[string]int

	// pathJSON 按路径片段覆盖 JSON 响应体。
	//
	// 🔴 为什么需要（2026-10-07）：本项目对国际版会拉**两个** JSON 端点
	// （/v2/.../models 与 /v3/config），它们的响应体结构不同。
	// 只用一个 JSONBody 会让两个端点返回同一份数据 —— 那样根本测不出
	// "合并两个源"的逻辑（实测踩过：/v3 误拿到 /v2 的样本，
	// 于是补充模型被判定为"已由 /v3 提供"而不再兜底，测试红）。
	pathJSON map[string][]byte

	// blockRefresh 让续期端点在响应前阻塞（制造确定性并发窗口）。
	// 见 SetBlockOnRefresh 的说明。
	blockRefresh *refreshBlock
}

// refreshBlock 是"把续期请求卡住"的一对信号。
type refreshBlock struct {
	entered chan struct{}
	release chan struct{}
}

// SetStatusForPath 让含指定子串的路径返回给定状态码，其余路径正常。
//
// 用途：测"上游对话失败但目录正常"这类场景 —— 否则注册阶段就失败了。
func (f *FakeUpstream) SetStatusForPath(substr string, code int) *FakeUpstream {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pathStatus == nil {
		f.pathStatus = make(map[string]int)
	}
	f.pathStatus[substr] = code
	return f
}

// ViolationDetected 报告是否检测到任何契约违反。
//
// 供「元测试」使用：验证断言逻辑本身有效，而不是默默通过。
func (f *FakeUpstream) ViolationDetected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.violations) > 0
}

// Violations 返回检测到的违反列表。
func (f *FakeUpstream) Violations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.violations))
	copy(out, f.violations)
	return out
}

// AssertChatContract 对一条已记录的请求执行契约断言（导出供元测试）。
//
// 用途：验证断言逻辑自身有效，而不是默默通过。
func (f *FakeUpstream) AssertChatContract(rec RecordedRequest) {
	f.assertChatContract(rec)
}

// NewFakeUpstream 启动一个假上游。
//
// 默认 FailOnViolation = true —— 任何契约违反都会立即 t.Fatal，
// 这正是我们想要的：安全红线不能靠人工检查。
func NewFakeUpstream(t *testing.T) *FakeUpstream {
	t.Helper()
	f := &FakeUpstream{t: t, FailOnViolation: true}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

// WithSSE 设置要回放的 SSE 样本（可传 fixture 文件名）。
func (f *FakeUpstream) WithSSE(fixtureName string) *FakeUpstream {
	f.SSEBody = FixtureString(fixtureName)
	return f
}

// WithJSON 设置要返回的 JSON 样本。
func (f *FakeUpstream) WithJSON(fixtureName string) *FakeUpstream {
	f.JSONBody = Fixture(fixtureName)
	return f
}

// WithJSONForPath 让含指定子串的路径返回给定的 JSON 样本。
//
// 用途：一个测试里同时模拟 /v2 与 /v3 两个不同结构的端点。
// 优先于 WithJSON（更具体的路径规则先命中）。
func (f *FakeUpstream) WithJSONForPath(substr, fixtureName string) *FakeUpstream {
	return f.WithJSONBodyForPath(substr, Fixture(fixtureName))
}

// WithJSONBodyForPath 同 WithJSONForPath，但直接给字节（用于一次性场景，
// 不必为它建一个 fixture 文件）。
func (f *FakeUpstream) WithJSONBodyForPath(substr string, body []byte) *FakeUpstream {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pathJSON == nil {
		f.pathJSON = make(map[string][]byte)
	}
	f.pathJSON[substr] = body
	return f
}

// WithStatus 强制状态码。
func (f *FakeUpstream) WithStatus(code int) *FakeUpstream {
	f.StatusCode = code
	return f
}

// Requests 返回已记录的请求（副本）。
func (f *FakeUpstream) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// CountPath 返回路径含指定子串的请求数。
//
// 用途（2026-10-09 凭证保活）：断言"同账号并发续期只发了一个请求"。
// 那是防 token family 撤销的关键性质，必须能数得出来。
func (f *FakeUpstream) CountPath(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.Contains(r.Path, substr) {
			n++
		}
	}
	return n
}

// SetBlockOnRefresh 让续期端点在响应前阻塞，制造**确定性**的并发交错窗口。
//
// 参数：
//
//	entered —— 请求一进入假上游就 close（测试据此知道"请求已发出"）
//	release —— 测试 close 它之后请求才继续返回
//
// 🔴 为什么需要（2026-10-09）：
//
//	要验证"同账号并发续期去重"，必须有**可复现**的交错点。
//	靠 sleep 猜时序是 flaky 的；把第一个请求真的卡在服务端，
//	才能确定地测出"第二个调用是等待而不是再发一个"。to
//
// ⚠️ 本机无 gcc、`go test -race` 用不了 ⇒ 这提供的是
//
//	**受控确定性交错**，不是动态竞争检测。措辞上不得说成"并发已验证"。
func (f *FakeUpstream) SetBlockOnRefresh(entered, release chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockRefresh = &refreshBlock{entered: entered, release: release}
}

// LastRequest 返回最近一次请求。
func (f *FakeUpstream) LastRequest() (RecordedRequest, bool) {
	reqs := f.Requests()
	if len(reqs) == 0 {
		return RecordedRequest{}, false
	}
	return reqs[len(reqs)-1], true
}

// Reset 清空记录的请求。
func (f *FakeUpstream) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

// handle 处理一次请求：记录 → 断言契约 → 回放响应。
func (f *FakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = r.Body.Close()

	rec := RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()

	// 对 chat 端点执行契约断言
	if strings.Contains(r.URL.Path, "/chat/completions") {
		f.assertChatContract(rec)
	}

	// 续期端点的**确定性阻塞**（见 SetBlockOnRefresh）。
	//
	// ⚠️ 必须在写任何响应之前 —— 否则"卡住"就没有意义了。
	f.mu.Lock()
	block := f.blockRefresh
	f.mu.Unlock()
	if block != nil && strings.Contains(r.URL.Path, "token/refresh") {
		select {
		case <-block.entered:
			// 已经 close 过，避免重复 close panic。
		default:
			close(block.entered)
		}
		<-block.release
	}

	if f.StatusCode != 0 {
		w.WriteHeader(f.StatusCode)
		return
	}

	// 路径级状态码覆盖（优先于默认回放）
	f.mu.Lock()
	for substr, code := range f.pathStatus {
		if strings.Contains(r.URL.Path, substr) {
			f.mu.Unlock()
			w.WriteHeader(code)
			return
		}
	}
	f.mu.Unlock()

	// 路径级 JSON 覆盖（比全局 JSONBody 更具体，先命中）。
	f.mu.Lock()
	for substr, body := range f.pathJSON {
		if strings.Contains(r.URL.Path, substr) {
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
	}
	f.mu.Unlock()

	// 按路径路由：模型目录返回 JSON，对话返回 SSE。
	// 这是必要的，因为测试里 client 会先拉目录再对话。
	isChat := strings.Contains(r.URL.Path, "/chat/completions")
	if !isChat && f.JSONBody != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.JSONBody)
		return
	}

	if f.SSEBody != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 逐行回放，保留真实分片边界（测试解析器对边界的容忍度）
		for _, line := range strings.Split(f.SSEBody, "\n") {
			if line == "" {
				continue
			}
			_, _ = io.WriteString(w, line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}

	if f.JSONBody != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.JSONBody)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"code":0,"msg":"OK"}`)
}

// assertChatContract 断言 chat 请求满足上游契约与安全红线。
func (f *FakeUpstream) assertChatContract(rec RecordedRequest) {
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		f.mu.Lock()
		f.violations = append(f.violations, msg)
		f.mu.Unlock()
		if f.FailOnViolation {
			f.t.Errorf("假上游契约违反: %s", msg)
		}
	}

	// 🔴 安全红线 1：绝不能带 X-Refresh-Token
	if v := rec.Get("X-Refresh-Token"); v != "" {
		fail("chat 请求携带了 X-Refresh-Token —— 这是安全红线，refresh token 只允许用于 refresh 端点")
	}

	// 必需身份头
	if v := rec.Get("Authorization"); v == "" {
		fail("缺少 Authorization 头")
	} else if !strings.HasPrefix(v, "Bearer ") {
		fail("Authorization 头格式应为 'Bearer <token>'，实际为 %q", v)
	}
	if rec.Get("X-User-Id") == "" {
		fail("缺少 X-User-Id 头")
	}

	// 伪装头
	if v := rec.Get("X-IDE-Name"); v != "WorkBuddy" {
		fail("X-IDE-Name 应为 WorkBuddy，实际为 %q", v)
	}
	if v := rec.Get("X-Product"); v != "WorkBuddy" {
		fail("X-Product 应为 WorkBuddy，实际为 %q", v)
	}

	// 链路追踪头
	for _, h := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID"} {
		if rec.Get(h) == "" {
			fail("缺少链路追踪头 %s", h)
		}
	}

	// 🔴 契约：必须 stream:true（上游拒绝非流式）
	if len(rec.Body) > 0 {
		var payload struct {
			Stream *bool `json:"stream"`
		}
		if err := json.Unmarshal(rec.Body, &payload); err == nil {
			if payload.Stream == nil {
				fail("请求体缺少 stream 字段；上游要求显式 stream:true")
			} else if !*payload.Stream {
				fail("请求体 stream=false；上游拒绝非流式（会返回 400），网关必须自行聚合")
			}
		}
	}
}

// AssertNoRefreshTokenHeader 供测试在 fake 之外单独断言。
func AssertNoRefreshTokenHeader(t *testing.T, h http.Header) {
	t.Helper()
	if v := h.Get("X-Refresh-Token"); v != "" {
		t.Fatalf("请求携带了 X-Refresh-Token，违反安全红线")
	}
}

// LoadFixtureJSON 把 fixture 解析到目标结构，失败即 t.Fatal。
func LoadFixtureJSON(t *testing.T, name string, dst any) {
	t.Helper()
	if err := json.Unmarshal(Fixture(name), dst); err != nil {
		t.Fatalf("解析 fixture %s 失败: %v", name, err)
	}
}

// WriteTempFile 在临时目录写一个文件（供需要文件输入的测试用）。
func WriteTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + name
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	return path
}
