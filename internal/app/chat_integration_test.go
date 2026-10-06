package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/router"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// newChatTestServer 起一个完整链路：假上游 → workbuddy provider → 真实 HTTP 服务。
func newChatTestServer(t *testing.T, sseFixture string) (*httptest.Server, *testutil.FakeUpstream) {
	t.Helper()

	fake := testutil.NewFakeUpstream(t)
	// 目录返回 JSON（client 会先拉目录），对话返回 SSE
	fake.WithJSON("models-listing.json")
	fake.WithSSE(sseFixture)

	wbClient := newWorkbuddyClientForTest(t, fake.URL)
	wbProv := newWorkbuddyProviderForTest(wbClient)

	r := router.New()
	if err := r.Register(context.Background(), wbProv); err != nil {
		t.Fatalf("注册 provider 失败: %v", err)
	}

	h := newMux(Deps{Router: r, Logger: log.New(io.Discard, "", 0)})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, fake
}

// TestChatAggregatePath 是核心端到端测试（非流式）：
// 客户端要非流式 → 我们向上游发流式 → 聚合后返回一个 JSON。
func TestChatAggregatePath(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}

	var got openai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应不是合法 JSON（非流式必须返回 JSON）: %v", err)
	}

	// 聚合正确性
	if len(got.Choices) != 1 {
		t.Fatalf("choices 数 = %d", len(got.Choices))
	}
	msg := got.Choices[0].Message
	if msg.Content != "可用" {
		t.Errorf("content = %q，期望 可用（41 个思考分片不能挤掉正文）", msg.Content)
	}
	if msg.ReasoningContent == "" {
		t.Error("reasoning_content 丢失 —— 客户端将看不到思考过程")
	}
	if got.Usage == nil || got.Usage.CompletionThinkingTokens != 41 {
		t.Errorf("思考 token 未保留: %+v", got.Usage)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}

	// ⚠️ 关键：向上游发的必须是 stream:true（上游拒绝非流式）
	rec, ok := fake.LastRequest()
	if !ok {
		t.Fatal("假上游未收到请求")
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true {
		t.Errorf("向上游发送的 stream = %v，必须为 true（上游拒绝非流式）", sent["stream"])
	}
}

// TestChatStreamPath 端到端测试（流式）：
// 验证 SSE 逐帧下发，且思考与正文都在。
func TestChatStreamPath(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	var (
		reasoning strings.Builder
		content   strings.Builder
		sawDone   bool
		chunks    int
	)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		chunks++

		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("分片不是合法 JSON: %v\n%s", err, payload)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
			reasoning.WriteString(chunk.Choices[0].Delta.ReasoningContent)
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	if !sawDone {
		t.Error("未收到 [DONE]")
	}
	if reasoning.Len() == 0 {
		t.Error("流式路径丢失了思考内容")
	}
	if content.String() != "可用" {
		t.Errorf("流式正文 = %q，期望 可用", content.String())
	}
	if chunks < 2 {
		t.Errorf("分片数 = %d，太少", chunks)
	}
}

// TestChatForcesStreamUpstream 验证非流式请求也不会向上游发 stream:false。
//
// 这是上游硬约束：发 false 直接 400。
func TestChatForcesStreamUpstream(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}],"stream":false}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rec, _ := fake.LastRequest()
	var sent map[string]any
	_ = json.Unmarshal(rec.Body, &sent)
	if sent["stream"] != true {
		t.Errorf("即使客户端要非流式，向上游也必须 stream:true；实际 %v", sent["stream"])
	}
}

// TestChatInjectsDefaultEffort 验证服务端显式注入默认思考档位。
//
// 依据：实测 deepseek 系"不传 reasoning_effort = 完全不思考"，
// 所以即使客户端不传，我们也必须注入，否则用户以为在用思考模型。
func TestChatInjectsDefaultEffort(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rec, _ := fake.LastRequest()
	var sent map[string]any
	_ = json.Unmarshal(rec.Body, &sent)

	effort, _ := sent["reasoning_effort"].(string)
	if effort == "" {
		t.Fatal("未注入 reasoning_effort —— deepseek 系不传就等于完全不思考")
	}
	if effort != "high" {
		t.Errorf("注入的档位 = %q，期望 high（委托人要求默认 high）", effort)
	}
}

// TestChatPreservesClientEffort 验证客户端指定的档位被透传。
func TestChatPreservesClientEffort(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}],"reasoning_effort":"max"}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rec, _ := fake.LastRequest()
	var sent map[string]any
	_ = json.Unmarshal(rec.Body, &sent)
	if sent["reasoning_effort"] != "max" {
		t.Errorf("客户端指定的档位被改写为 %v，应原样透传", sent["reasoning_effort"])
	}
}

// TestChatConvertsDeveloperRole 验证 developer → system。
func TestChatConvertsDeveloperRole(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rec, _ := fake.LastRequest()
	var sent struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body, &sent)
	if len(sent.Messages) == 0 {
		t.Fatal("没有 messages")
	}
	if sent.Messages[0].Role != "system" {
		t.Errorf("developer 未被归一: role = %q（上游白名单不含 developer）", sent.Messages[0].Role)
	}
}

// TestChatConvertsMaxCompletionTokens 验证别名翻译。
func TestChatConvertsMaxCompletionTokens(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}],"max_completion_tokens":12345}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rec, _ := fake.LastRequest()
	var sent map[string]any
	_ = json.Unmarshal(rec.Body, &sent)

	if sent["max_tokens"] != float64(12345) {
		t.Errorf("max_tokens = %v，期望 12345（别名应被翻译）", sent["max_tokens"])
	}
	if _, still := sent["max_completion_tokens"]; still {
		t.Error("别名应被删除（上游只认 max_tokens）")
	}
}

// TestChatStripsModelPrefix 验证模型名前缀被去掉。
func TestChatStripsModelPrefix(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rec, _ := fake.LastRequest()
	var sent map[string]any
	_ = json.Unmarshal(rec.Body, &sent)
	if sent["model"] != "space-bunny" {
		t.Errorf("发给上游的 model = %v，期望去掉前缀的 space-bunny", sent["model"])
	}
}

// TestChatRejectsUnknownModel 验证未知模型返回 404 且是 JSON。
func TestChatRejectsUnknownModel(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/nope","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", resp.StatusCode)
	}
	var e openai.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("错误响应不是 JSON: %v", err)
	}
	if e.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q", e.Error.Code)
	}
}

// TestChatRejectsBadJSON 验证非法 JSON 被拒。
func TestChatRejectsBadJSON(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader("{不是JSON"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestChatRejectsMissingMessages 验证缺 messages 被拒。
func TestChatRejectsMissingMessages(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny"}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestChatMethodNotAllowed 验证非 POST 被拒。
func TestChatMethodNotAllowed(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestChatRejectsOversizedBody 验证超大请求体被拒。
func TestChatRejectsOversizedBody(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	huge := bytes.Repeat([]byte("a"), MaxRequestBodyBytes+1024)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("状态码 = %d，期望 413", resp.StatusCode)
	}
}

// TestChatAcceptsDSHShapedRequest 是 DSH 真实请求的端到端回归（2026-10-05）。
//
// 委托人用 DSH 接入时连续撞到两个 400：
//
//	tools.0 of type uint8            ← tools/tool_choice 曾被误写成 []byte
//	messages.6.content of type string ← content 是多模态数组
//
// 这个测试用 DSH 的真实形状走完整链路，并断言改写后的上游请求体里
// content 数组与 tools 数组【原样保留】—— 我们只替换 model 字段，
// 不做有损的结构体往返。
func TestChatAcceptsDSHShapedRequest(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-deepseek-v4.1-flash-high.txt")

	// 形状取自 DSH 实际发出的请求：多模态 content + tools + tool_choice。
	body := `{
	  "model": "workbuddy/deepseek-v4.1-flash",
	  "messages": [
	    {"role": "system", "content": [{"type": "text", "text": "你是助手"}]},
	    {"role": "user", "content": [{"type": "text", "text": "hi"}]},
	    {"role": "assistant", "content": null, "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "结果"}
	  ],
	  "tools": [{
	    "type": "function",
	    "function": {
	      "name": "get_weather",
	      "description": "查询天气",
	      "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
	    }
	  }],
	  "tool_choice": "auto",
	  "stream": false
	}`

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 = %s", resp.StatusCode, respBody)
	}

	// ── 断言转发到上游的请求体保真 ──
	rec, ok := fake.LastRequest()
	if !ok {
		t.Fatal("上游未收到请求")
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.Body, &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}

	// model 必须已去前缀
	if got, _ := sent["model"].(string); got != "deepseek-v4.1-flash" {
		t.Errorf("上游 model = %q，期望 deepseek-v4.1-flash", got)
	}

	// content 数组必须原样保留（不能被降级成字符串或被丢掉）
	msgs, ok := sent["messages"].([]any)
	if !ok || len(msgs) != 4 {
		t.Fatalf("上游 messages 形状异常: %T len=%d", sent["messages"], len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if _, isArr := first["content"].([]any); !isArr {
		t.Errorf("上游第一条消息的 content 应为数组，实际 %T = %v", first["content"], first["content"])
	}

	// tools 必须原样保留
	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("上游 tools 形状异常: %T = %v", sent["tools"], sent["tools"])
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if name, _ := fn["name"].(string); name != "get_weather" {
		t.Errorf("tools 内容丢失：function.name = %v", fn["name"])
	}

	// tool_choice 透传
	if got, _ := sent["tool_choice"].(string); got != "auto" {
		t.Errorf("tool_choice = %v，期望 auto", sent["tool_choice"])
	}

	// 上游仍必须是 stream:true（硬约束）
	if sent["stream"] != true {
		t.Errorf("向上游的 stream = %v，必须为 true", sent["stream"])
	}
}
