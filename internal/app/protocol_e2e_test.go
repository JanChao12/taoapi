package app

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 本文件是 Anthropic Messages 与 OpenAI Responses 两条新协议的
// **端到端**护栏测试：走真实的 newMux → requireAPIKey* → handleChatWith
// → TryChat → workbuddy provider → 假上游。
//
// 🔴 为什么必须有端到端（不能只有协议包单测）：
//
//	协议这种东西单测全绿也可能在生产坏掉 —— 第 17/19 轮那两次
//	"前后端契约不一致"都是单元测试覆盖不到的。本文件测的是
//	**接线**：路由、鉴权、翻译器接入、编码器接入、记账。

// postJSONWithHeaders 发一个 JSON POST（可带自定义头）并返回响应。
//
// ⚠️ 刻意不叫 postJSON：checkin_daemon_test.go 已有一个同名同包函数
// （签名是 (t, url, body) → (code, body)）。重名会编译失败，
// 改自己的新辅助函数比动别人的既有测试安全。
func postJSONWithHeaders(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readSSE 逐行读 SSE，返回 (事件名, data) 列表。
//
// ⚠️ 同时兼容两种格式：
//
//	OpenAI     只写 `data: {...}`
//	Anthropic  写 `event: name` + `data: {...}`
//	Responses  写 `event: name` + `data: {...}`
func readSSE(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	var (
		out  []sseFrame
		name string
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, sseFrame{
				Event: name,
				Data:  strings.TrimPrefix(line, "data: "),
			})
			name = ""
		}
	}
	return out
}

type sseFrame struct {
	Event string
	Data  string
}

// ─────────────────────────────────────────────────────────────
// Anthropic：非流式
// ─────────────────────────────────────────────────────────────

// TestAnthropicAggregateEndToEnd 守 Anthropic 非流式的完整链路。
func TestAnthropicAggregateEndToEnd(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","max_tokens":1024,
		"messages":[{"role":"user","content":"hi"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	if got["type"] != "message" {
		t.Errorf("type = %v，期望 message", got["type"])
	}
	if got["role"] != "assistant" {
		t.Errorf("role = %v，期望 assistant", got["role"])
	}
	// 客户端请求的模型名要回显（不是上游 ID）
	if got["model"] != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("model = %v，期望客户端请求的名字", got["model"])
	}
	id, _ := got["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("id = %q，期望 msg_ 前缀", id)
	}
	content, _ := got["content"].([]any)
	if len(content) == 0 {
		t.Fatal("content 为空 —— 客户端会收到空消息")
	}

	// 思考块必须在文本块之前，且正文必须保留
	var sawThinking, sawText bool
	for _, c := range content {
		cm, _ := c.(map[string]any)
		switch cm["type"] {
		case "thinking":
			sawThinking = true
			if cm["thinking"] == "" {
				t.Error("thinking 块内容为空")
			}
			if _, has := cm["signature"]; !has {
				t.Error("thinking 块缺少 signature 字段（必填，可为空串）")
			}
		case "text":
			if sawThinking == false && sawText {
				t.Error("文本块出现在思考块之前")
			}
			sawText = true
			if cm["text"] != "可用" {
				t.Errorf("text = %v，期望 可用", cm["text"])
			}
		}
	}
	if !sawThinking {
		t.Error("思考块丢失 —— 上游有 41 个思考分片，客户端应能看到")
	}
	if !sawText {
		t.Error("文本块丢失")
	}

	// usage 字段名必须是 Anthropic 形状
	u, _ := got["usage"].(map[string]any)
	if u == nil {
		t.Fatal("缺少 usage")
	}
	if _, has := u["input_tokens"]; !has {
		t.Errorf("usage 缺 input_tokens（Anthropic 字段名）: %v", u)
	}
	if _, has := u["output_tokens"]; !has {
		t.Errorf("usage 缺 output_tokens: %v", u)
	}
	if _, has := u["prompt_tokens"]; has {
		t.Error("usage 里出现了 OpenAI 字段名 prompt_tokens —— 字段名没翻译")
	}
}

// ─────────────────────────────────────────────────────────────
// Anthropic：流式
// ─────────────────────────────────────────────────────────────

// TestAnthropicStreamEndToEnd 守 Anthropic 流式的完整事件序列。
func TestAnthropicStreamEndToEnd(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","max_tokens":1024,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages", body, nil)
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	frames := readSSE(t, resp.Body)
	if len(frames) == 0 {
		t.Fatal("没有收到任何 SSE 帧")
	}

	var (
		events    []string
		reasoning strings.Builder
		text      strings.Builder
		stopSeq   bool
	)
	for _, f := range frames {
		if f.Event == "" {
			t.Errorf("帧缺少 event 名（Anthropic 客户端按事件名分派）: %s", f.Data)
			continue
		}
		events = append(events, f.Event)

		var payload map[string]any
		if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
			t.Errorf("帧 %s 的 data 不是合法 JSON: %v", f.Event, err)
			continue
		}
		if f.Event == "content_block_delta" {
			d, _ := payload["delta"].(map[string]any)
			switch d["type"] {
			case "thinking_delta":
				reasoning.WriteString(asStr(d["thinking"]))
			case "text_delta":
				text.WriteString(asStr(d["text"]))
			}
		}
		if f.Event == "message_stop" {
			stopSeq = true
		}
	}

	// 序列骨架
	if events[0] != "message_start" {
		t.Errorf("首个事件 = %s，期望 message_start", events[0])
	}
	if !stopSeq {
		t.Error("缺少 message_stop")
	}
	if text.String() != "可用" {
		t.Errorf("正文 = %q，期望 可用（41 个思考分片不能挤掉正文）", text.String())
	}
	if reasoning.Len() == 0 {
		t.Error("思考增量丢失 —— 客户端将看不到思考过程")
	}
}

// ─────────────────────────────────────────────────────────────
// Anthropic：鉴权
// ─────────────────────────────────────────────────────────────

// TestAnthropicAcceptsXAPIKey 守：x-api-key 头必须被接受。
//
// 🔴 委托方 2026-10-09 拍板第 2 条：x-api-key 与 Authorization 都要接受。
//
//	DSH 的 Anthropic 路径**只发 x-api-key**（从 app.asar 提取的真实契约），
//	不支持它等于 DSH 切到 Anthropic 协议后立刻 401。
func TestAnthropicAcceptsXAPIKey(t *testing.T) {
	srv := newAuthEnv(t, "secret-anthro-key")

	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`
	// 只带 x-api-key，不带 Authorization
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages", body,
		map[string]string{"x-api-key": "secret-anthro-key"})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("x-api-key 正确却返回 401 —— DSH 的 Anthropic 路径只发这个头，" +
			"不支持它等于切到 Anthropic 协议后完全不可用")
	}
}

// TestAnthropicAcceptsBearer 守：Authorization: Bearer 也必须被接受。
func TestAnthropicAcceptsBearer(t *testing.T) {
	srv := newAuthEnv(t, "secret-anthro-key")

	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages", body,
		map[string]string{"Authorization": "Bearer secret-anthro-key"})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("Bearer 正确却返回 401")
	}
}

// TestAnthropicRejectsWrongKey 守：错的 key 必须 401（多一个入口不能放松判定）。
func TestAnthropicRejectsWrongKey(t *testing.T) {
	srv := newAuthEnv(t, "secret-anthro-key")

	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`
	for _, h := range []map[string]string{
		{"x-api-key": "wrong"},
		{"Authorization": "Bearer wrong"},
		{"x-api-key": ""},
		nil,
	} {
		resp := postJSONWithHeaders(t, srv.URL+"/v1/messages", body, h)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("headers=%v 状态码 = %d，期望 401", h, resp.StatusCode)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// Anthropic：count_tokens
// ─────────────────────────────────────────────────────────────

// TestAnthropicCountTokens 守 count_tokens 端点。
func TestAnthropicCountTokens(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","max_tokens":100,
		"messages":[{"role":"user","content":"你好世界"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages/count_tokens", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	n, ok := got["input_tokens"].(float64)
	if !ok {
		t.Fatalf("input_tokens 缺失或类型错: %v", got)
	}
	if n <= 0 {
		t.Errorf("input_tokens = %v，期望正数", n)
	}
}

// TestAnthropicCountTokensValidatesShape 守：count_tokens 也做形状校验。
//
// 🔴 只把 body 丢给启发式会**静默接受**任何东西 —— 连
//
//	`{"messages":"不是数组"}` 都会"成功"返回一个数字，
//	客户端据此以为请求合法，直到真正发对话才失败。
func TestAnthropicCountTokensValidatesShape(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	for _, body := range []string{
		`{"messages":"不是数组"}`,
		`{"model":"m"}`,
		`{"model":"m","messages":[]}`,
		`{不是JSON`,
	} {
		resp := postJSONWithHeaders(t, srv.URL+"/v1/messages/count_tokens", body, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("非法请求体 %s 状态码 = %d，期望 400", body, resp.StatusCode)
		}
	}
}

// TestAnthropicCountTokensIgnoresRouter 守：未配置渠道时 count_tokens 仍可用。
//
// 🔴 它是纯计算，不发上游。若要求 Router 存在，客户端在启动阶段
//
//	探测能力时就会失败。
func TestAnthropicCountTokensIgnoresRouter(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	// 用一个不存在的模型：count_tokens 不该因此失败
	body := `{"model":"whatever","max_tokens":100,
		"messages":[{"role":"user","content":"x"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages/count_tokens", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200（count_tokens 不该依赖模型解析）", resp.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────
// Anthropic：错误形状
// ─────────────────────────────────────────────────────────────

// TestAnthropicErrorShape 守：错误必须是 Anthropic 形状。
//
//	{"type":"error","error":{"type":"...","message":"..."}}
//
// 🔴 若返回 OpenAI 形状（{"error":{"message","type","code"}}），
//
//	Anthropic 客户端解不出 type 字段，会当成未知错误。
func TestAnthropicErrorShape(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/nope","max_tokens":100,
		"messages":[{"role":"user","content":"x"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if got["type"] != "error" {
		t.Errorf("外层 type = %v，期望 error（Anthropic 形状）", got["type"])
	}
	inner, _ := got["error"].(map[string]any)
	if inner == nil {
		t.Fatal("缺少 error 对象")
	}
	typ, _ := inner["type"].(string)
	// 🔴 必须是规范 9 种枚举之一
	canonical := map[string]bool{
		"invalid_request_error": true, "authentication_error": true,
		"billing_error": true, "permission_error": true, "not_found_error": true,
		"rate_limit_error": true, "gateway_timeout_error": true,
		"api_error": true, "overloaded_error": true,
	}
	if !canonical[typ] {
		t.Errorf("error.type = %q 不在 Anthropic 规范 9 种枚举内 —— 不得自造", typ)
	}
	if inner["message"] == "" {
		t.Error("error.message 为空")
	}
}

// TestAnthropicMethodNotAllowed 守：非 POST 返回 405 且是 Anthropic 形状。
func TestAnthropicMethodNotAllowed(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if got["type"] != "error" {
		t.Errorf("405 响应不是 Anthropic 形状: %v", got)
	}
}

// TestAnthropicBetaQueryString 守：?beta=true 的 query 必须被容忍。
//
// 🔴 DSH 的 pi-ai 路径固定发 `/v1/messages?beta=true`
//
//	（从 app.asar 提取的真实契约）。若路由不接受 query，DSH 直接 404。
func TestAnthropicBetaQueryString(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","max_tokens":100,
		"messages":[{"role":"user","content":"x"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/messages?beta=true", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("/v1/messages?beta=true 返回 404 —— DSH 的 pi-ai 路径固定带这个 query")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}
}

// ─────────────────────────────────────────────────────────────
// Responses：非流式 / 流式 / 鉴权 / 错误
// ─────────────────────────────────────────────────────────────

// TestResponsesAggregateEndToEnd 守 Responses 非流式的完整链路。
func TestResponsesAggregateEndToEnd(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","instructions":"你是助手",
		"input":"hi","max_output_tokens":1024}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/responses", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got["object"] != "response" {
		t.Errorf("object = %v，期望 response", got["object"])
	}
	id, _ := got["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Errorf("id = %q，期望 resp_ 前缀", id)
	}
	if got["model"] != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("model = %v，期望客户端请求的名字", got["model"])
	}
	if got["status"] != "completed" {
		t.Errorf("status = %v，期望 completed", got["status"])
	}
	output, _ := got["output"].([]any)
	if len(output) == 0 {
		t.Fatal("output 为空 —— 客户端拿不到任何内容")
	}
	// 必须含 message 项且带 output_text
	var sawMessage bool
	for _, o := range output {
		om, _ := o.(map[string]any)
		if om["type"] != "message" {
			continue
		}
		sawMessage = true
		content, _ := om["content"].([]any)
		if len(content) == 0 {
			t.Fatal("message 项的 content 为空")
		}
		part, _ := content[0].(map[string]any)
		if part["type"] != "output_text" {
			t.Errorf("content[0].type = %v，期望 output_text", part["type"])
		}
		if part["text"] != "可用" {
			t.Errorf("text = %v，期望 可用", part["text"])
		}
	}
	if !sawMessage {
		t.Error("output 里没有 message 项")
	}
}

// TestResponsesStreamEndToEnd 守 Responses 流式的完整事件序列。
func TestResponsesStreamEndToEnd(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","input":"hi",
		"max_output_tokens":1024,"stream":true}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/responses", body, nil)
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	frames := readSSE(t, resp.Body)
	var (
		events []string
		text   strings.Builder
	)
	for _, f := range frames {
		if f.Event == "" {
			t.Errorf("帧缺少 event 名: %s", f.Data)
			continue
		}
		events = append(events, f.Event)
		var payload map[string]any
		if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
			t.Errorf("帧 %s 不是合法 JSON: %v", f.Event, err)
			continue
		}
		if f.Event == "response.output_text.delta" {
			text.WriteString(asStr(payload["delta"]))
		}
	}
	if len(events) == 0 {
		t.Fatal("没有收到任何事件")
	}
	if events[0] != "response.created" {
		t.Errorf("首个事件 = %s，期望 response.created", events[0])
	}
	// 正常结束必须是 response.completed
	if events[len(events)-1] != "response.completed" {
		t.Errorf("最后一个事件 = %s，期望 response.completed（序列: %v）",
			events[len(events)-1], events)
	}
	if text.String() != "可用" {
		t.Errorf("正文 = %q，期望 可用", text.String())
	}
}

// TestResponsesAcceptsBothAuthHeaders 守两种鉴权头都被接受。
func TestResponsesAcceptsBothAuthHeaders(t *testing.T) {
	srv := newAuthEnv(t, "secret-resp-key")

	body := `{"model":"m","input":"x"}`
	for name, h := range map[string]map[string]string{
		"x-api-key": {"x-api-key": "secret-resp-key"},
		"bearer":    {"Authorization": "Bearer secret-resp-key"},
	} {
		resp := postJSONWithHeaders(t, srv.URL+"/v1/responses", body, h)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s 鉴权被拒（401）", name)
		}
	}
}

// TestResponsesRejectsWrongKey 守错误 key 必须 401。
func TestResponsesRejectsWrongKey(t *testing.T) {
	srv := newAuthEnv(t, "secret-resp-key")

	body := `{"model":"m","input":"x"}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/responses", body,
		map[string]string{"x-api-key": "wrong"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("状态码 = %d，期望 401", resp.StatusCode)
	}
}

// TestResponsesUnsupportedFeatureErrors 守：不支持的特性返回 400 且点名。
func TestResponsesUnsupportedFeatureErrors(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	cases := []struct {
		body string
		want string
	}{
		{`{"model":"workbuddy/space-bunny","input":"x","store":true}`, "store"},
		{`{"model":"workbuddy/space-bunny","input":"x","previous_response_id":"resp_1"}`, "previous_response_id"},
		{`{"model":"workbuddy/space-bunny","input":"x","truncation":"auto"}`, "truncation"},
		{`{"model":"workbuddy/space-bunny","input":[{"type":"web_search_call","id":"x"}]}`, "web_search_call"},
		{`{"model":"workbuddy/space-bunny","input":"x","tools":[{"type":"web_search"}]}`, "web_search"},
	}
	for _, tc := range cases {
		resp := postJSONWithHeaders(t, srv.URL+"/v1/responses", tc.body, nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body=%s 状态码 = %d，期望 400", tc.body, resp.StatusCode)
			continue
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("body=%s 的错误信息未点名 %q: %s", tc.body, tc.want, b)
		}
	}
}

// TestResponsesIncludeMustNotBeRejected 守：include **必须被接受**。
//
// 🔴 这是端到端的反向护栏（2026-10-09 修正了一个真实缺陷）：
//
//	DSH 只要开了思考就**必然**发 include（从 app.asar 提取的真实契约，
//	pi-ai/dist/api/openai-responses.js:230-270）：
//
//	  if (model.reasoning && (effort || summary)) {
//	      params.reasoning = { effort, summary: "auto" };
//	      params.include = ["reasoning.encrypted_content"];   // ← 总是带上
//	  }
//
//	若把 include 当"不支持的特性"报 400，DSH 的 Responses 路径
//	**每一个带思考的请求都会失败** —— 而思考正是最常用的形态。
//
//	为什么"接受并忽略"不算违反"做不到的明确报错"：
//	  include 是"额外可选附带数据"，不是功能承诺。不产出
//	  encrypted_content 不会让客户端卡住或少看到内容
//	  （DSH 只用它做 store:false 下的多轮重放兜底），所以忽略它是诚实的。
func TestResponsesIncludeMustNotBeRejected(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	// DSH 实际会发的完整形态
	body := `{"model":"workbuddy/space-bunny","input":"x","stream":false,
		"store":false,"max_output_tokens":4096,
		"reasoning":{"effort":"high","summary":"auto"},
		"include":["reasoning.encrypted_content"]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/responses", body, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("include 被拒（400）—— DSH 只要开思考就必然发它，"+
			"这会让 Responses 路径的每个思考请求都失败。响应: %s", b)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应: %s", resp.StatusCode, b)
	}
}

// TestResponsesMethodNotAllowed 守：非 POST 返回 405。
func TestResponsesMethodNotAllowed(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	resp, err := http.Get(srv.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────
// 跨协议：记账与既有契约不被破坏
// ─────────────────────────────────────────────────────────────

// TestProtocolRecordedInUsage 守：用量记录里的 protocol 字段区分三种协议。
//
// 🔴 为什么要区分：用量页按模型聚合，同一个模型可能被三种协议的客户端
//
//	分别调用。不区分的话，排查"是不是某个协议的转换有问题"时无从下手。
func TestProtocolRecordedInUsage(t *testing.T) {
	// 直接测 recorder 的协议标记（端到端断言需要读 JSONL，成本更高）
	cases := []struct {
		protocol string
		want     string
	}{
		{"chat", "chat"},
		{"messages", "messages"},
		{"responses", "responses"},
		{"", "chat"}, // 空值回落 chat（零值可用）
	}
	for _, tc := range cases {
		rec := newUsageRecorderFor(Deps{}, "m", "up", false, "acct", "prov", tc.protocol)
		if got := rec.base().Protocol; got != tc.want {
			t.Errorf("protocol=%q → 记录 %q，期望 %q", tc.protocol, got, tc.want)
		}
	}
}

// TestChatCompletionsUnchanged 守：加两个协议**没有**改变
// /v1/chat/completions 的对外行为。
//
// 🔴 这是委托方第 1 条边界（只新增，不动既有）的护栏。
func TestChatCompletionsUnchanged(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/chat/completions", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 必须是 OpenAI Chat 形状（不是 Anthropic 的 message / Responses 的 response）
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v，期望 chat.completion —— "+
			"新增协议不该改变既有端点的形状", got["object"])
	}
	if _, has := got["choices"]; !has {
		t.Error("缺少 choices —— OpenAI Chat 形状被破坏")
	}
	// usage 必须是 OpenAI 字段名
	u, _ := got["usage"].(map[string]any)
	if u != nil {
		if _, has := u["prompt_tokens"]; !has {
			t.Error("usage 缺 prompt_tokens（OpenAI 字段名）")
		}
	}
}

// TestAllThreeProtocolsRoutable 守：三个协议端点同时可达。
func TestAllThreeProtocolsRoutable(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	cases := []struct {
		path string
		body string
	}{
		{"/v1/chat/completions",
			`{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`},
		{"/v1/messages",
			`{"model":"workbuddy/space-bunny","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`},
		{"/v1/responses",
			`{"model":"workbuddy/space-bunny","input":"x"}`},
	}
	for _, tc := range cases {
		resp := postJSONWithHeaders(t, srv.URL+tc.path, tc.body, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200", tc.path, resp.StatusCode)
		}
	}
}

// asStr 从 any 取字符串（宽容）。
func asStr(v any) string {
	s, _ := v.(string)
	return s
}
