package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 本文件守「流中途失败时，两种新协议绝不能发各自的完成事件」。
//
// 🔴 为什么这是独立的一个测试文件（而不是塞进 protocol_e2e_test.go）：
//
//	它是本项目**最容易被"顺手统一"破坏**的一条契约，而破坏的后果很隐蔽：
//
//	  · Anthropic 的 `message_stop` 语义 = "正常结束"
//	  · Responses 的 `response.completed` 语义 = "正常结束"
//	  · 而 OpenAI 的 `[DONE]` 只表示 "SSE 传输结束"
//
//	⇒ 三种协议的错误收尾**看起来**可以做同一件事（都补一个结束标记），
//	  实际上前两者绝不能补：客户端会把半截回答当成完整回答继续跑
//	  （例如 agent 拿着被截断的 tool_call arguments 去执行）。
//
//	有人"统一"它们时，本文件会立刻变红并说明原因。

// streamFrames 发一个流式请求并返回 (状态码, 原始响应体)。
//
// 名字带 anthropic 是历史原因（最早只服务 Anthropic 路径）；
// 现在两条新协议共用它 —— 它们都是"具名事件 + data"的 SSE 格式。
func anthropicFrames(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp := postJSONWithHeaders(t, url, body, nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestAnthropicStreamNeverStopsOnError 守：Anthropic 流中途失败时
// **绝不能**发 message_stop / message_delta。
//
// 🔴 见本文件顶部说明。这是 anthropic 包 Fail() 的核心契约，
//
//	这里从 **app 层端到端**再验一遍 —— 防止有人把
//	anthropicStreamEncoder.fail 改成"先发 error 再补 message_stop"。
func TestAnthropicStreamNeverStopsOnError(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")
	// 制造流中途失败：去掉 [DONE]
	fake.SSEBody = strings.ReplaceAll(fake.SSEBody, "data: [DONE]", "")

	body := `{"model":"workbuddy/space-bunny","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"x"}]}`
	status, text := anthropicFrames(t, srv.URL+"/v1/messages", body)

	// 流已开始 → 状态码 200，改不了
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（流已开始，状态码改不了）", status)
	}

	if strings.Contains(text, "event: message_stop") {
		t.Fatalf("流中途失败却发了 message_stop —— "+
			"客户端会把半截回答当成完整回答继续跑（agent 会拿着被截断的 "+
			"tool_call arguments 去执行）。\n响应尾部: %s", tail(text, 400))
	}
	if strings.Contains(text, "event: message_delta") {
		t.Fatalf("流中途失败却发了 message_delta（含 stop_reason）—— "+
			"等同宣告正常结束。\n响应尾部: %s", tail(text, 400))
	}
	// 必须有 error 事件告知客户端
	if !strings.Contains(text, "event: error") {
		t.Fatalf("流中途失败却没有 error 事件 —— 客户端无法得知失败。\n响应尾部: %s",
			tail(text, 400))
	}
}

// TestResponsesStreamNeverCompletesOnError 守：Responses 流中途失败时
// **绝不能**发 response.completed / response.incomplete。
func TestResponsesStreamNeverCompletesOnError(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")
	fake.SSEBody = strings.ReplaceAll(fake.SSEBody, "data: [DONE]", "")

	body := `{"model":"workbuddy/space-bunny","input":"x","stream":true}`
	status, text := anthropicFrames(t, srv.URL+"/v1/responses", body)

	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（流已开始，状态码改不了）", status)
	}

	if strings.Contains(text, "event: response.completed") {
		t.Fatalf("流中途失败却发了 response.completed —— "+
			"客户端会把半截回答当成完整回答继续跑。\n响应尾部: %s", tail(text, 400))
	}
	if strings.Contains(text, "event: response.incomplete") {
		t.Fatalf("流中途失败却发了 response.incomplete —— 语义应是 failed。"+
			"\n响应尾部: %s", tail(text, 400))
	}
	if !strings.Contains(text, "event: response.failed") {
		t.Fatalf("流中途失败却没有 response.failed。\n响应尾部: %s", tail(text, 400))
	}

	// response.failed 必须携带 error 对象（否则客户端不知道原因）
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev["type"] != "response.failed" {
			continue
		}
		resp, _ := ev["response"].(map[string]any)
		if resp == nil {
			t.Fatal("response.failed 缺少 response 对象")
		}
		if resp["status"] != "failed" {
			t.Errorf("response.status = %v，期望 failed", resp["status"])
		}
		if _, has := resp["error"]; !has {
			t.Error("response.failed 缺少 error 对象 —— 客户端不知道失败原因")
		}
	}
}

// TestAnthropicStreamErrorTypeCanonicalEndToEnd 守：端到端路径上的
// error.type 也必须落在 Anthropic 规范 9 种枚举内。
//
// 🔴 单元测试已覆盖 ErrorFrame 本身；这里验的是**接线** ——
//
//	app 层可能传进一个 OpenAI 风格的类型串（如 upstream_error），
//	那会让严格客户端判为未知类型。
func TestAnthropicStreamErrorTypeCanonicalEndToEnd(t *testing.T) {
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")
	fake.SSEBody = strings.ReplaceAll(fake.SSEBody, "data: [DONE]", "")

	body := `{"model":"workbuddy/space-bunny","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"x"}]}`
	_, text := anthropicFrames(t, srv.URL+"/v1/messages", body)

	canonical := map[string]bool{
		"invalid_request_error": true, "authentication_error": true,
		"billing_error": true, "permission_error": true, "not_found_error": true,
		"rate_limit_error": true, "gateway_timeout_error": true,
		"api_error": true, "overloaded_error": true,
	}
	found := false
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev["type"] != "error" {
			continue
		}
		found = true
		inner, _ := ev["error"].(map[string]any)
		typ, _ := inner["type"].(string)
		if !canonical[typ] {
			t.Errorf("端到端路径上的 error.type = %q 不在规范 9 种枚举内 —— "+
				"不得自造（app 层可能把 OpenAI 风格的类型串直接传下来了）", typ)
		}
	}
	if !found {
		t.Fatal("没有找到 error 事件")
	}
}

// TestStreamFailuresAreRecordedAsErrors 守：流中途失败要记账为失败。
//
// 🔴 用户拿到了半截回答，用量页必须能看到这次请求是失败的 ——
//
//	否则成功率会被高估，排查"为什么回答不完整"时没有任何线索。
func TestStreamFailuresAreRecordedAsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{"anthropic", "/v1/messages",
			`{"model":"workbuddy/space-bunny","max_tokens":100,"stream":true,` +
				`"messages":[{"role":"user","content":"x"}]}`},
		{"responses", "/v1/responses",
			`{"model":"workbuddy/space-bunny","input":"x","stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")
			fake.SSEBody = strings.ReplaceAll(fake.SSEBody, "data: [DONE]", "")

			resp := postJSONWithHeaders(t, srv.URL+tc.path, tc.body, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			// 记账走 deps.Usage；newChatTestServer 没装 Usage store，
			// 所以这里只能确认"请求没有因为记账失败而崩"。
			// 真正的记账断言在 usage_record_test.go（单元层）。
			if resp.StatusCode != http.StatusOK {
				t.Errorf("状态码 = %d，期望 200（流已开始）", resp.StatusCode)
			}
		})
	}
}
