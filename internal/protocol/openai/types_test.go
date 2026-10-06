package openai

import (
	"encoding/json"
	"testing"
)

// 本文件守住 2026-10-05 的真实故障（DSH 接入时暴露）：
//
//	400 invalid_json: cannot unmarshal array into Go struct field
//	    ChatCompletionRequest.tools.0 of type uint8
//	400 invalid_json: cannot unmarshal array into Go struct field
//	    ChatCompletionRequest.messages.6.content of type string
//
// 两个都是"入站解析比 OpenAI 实际协议更窄"造成的。
// 真因：tools/tool_choice 曾被写成 `type json_RawMessage = []byte`（笔误），
// content 只接受字符串。下面两个测试用 DSH 的真实形状把它们钉住。

// TestInboundToolsArrayParses 守住 tools 数组能被解析。
//
// 回归信号：若 json_RawMessage 又变回 []byte，这里会报
// "cannot unmarshal array into Go struct field ... of type uint8"。
func TestInboundToolsArrayParses(t *testing.T) {
	body := []byte(`{
	  "model": "workbuddy/deepseek-v4.1-flash",
	  "messages": [{"role": "user", "content": "hi"}],
	  "tools": [
	    {
	      "type": "function",
	      "function": {
	        "name": "get_weather",
	        "description": "查询天气",
	        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
	      }
	    }
	  ],
	  "tool_choice": "auto"
	}`)

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("解析失败（tools 数组形状回归）: %v", err)
	}
	if len(req.Tools) == 0 {
		t.Fatal("Tools 为空，说明数组内容没有被保留")
	}
	// 原文必须留着（RawMessage 的语义），否则上游收到的是坏数据。
	if !json.Valid(req.Tools) {
		t.Fatalf("Tools 不是合法 JSON 原文: %s", req.Tools)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(req.Tools, &parsed); err != nil {
		t.Fatalf("Tools 应为 JSON 数组: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("tools 长度 = %d，期望 1", len(parsed))
	}
}

// TestInboundContentArrayParses 守 content 为多模态数组时能解析。
//
// 回归信号：若 InboundMessage.Content 变回 string，这里会报
// "cannot unmarshal array into Go struct field ... content of type string"。
func TestInboundContentArrayParses(t *testing.T) {
	body := []byte(`{
	  "model": "workbuddy/deepseek-v4.1-flash",
	  "messages": [
	    {"role": "system", "content": "你是助手"},
	    {"role": "user", "content": [{"type": "text", "text": "hi"}]},
	    {"role": "user", "content": [
	      {"type": "text", "text": "两部分"},
	      {"type": "text", "text": "拼起来"}
	    ]},
	    {"role": "user", "content": [{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}}]}
	  ]
	}`)

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("解析失败（content 数组形状回归）: %v", err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages 长度 = %d，期望 4", len(req.Messages))
	}
	if got := req.Messages[0].Content.Text; got != "你是助手" {
		t.Errorf("字符串 content = %q，期望 你是助手", got)
	}
	if got := req.Messages[1].Content.Text; got != "hi" {
		t.Errorf("数组 content = %q，期望 hi", got)
	}
	if got := req.Messages[2].Content.Text; got != "两部分拼起来" {
		t.Errorf("多分片 content = %q，期望 两部分拼起来", got)
	}
	// 纯图片消息没有文本，应为空串且不报错（不能比上游更严）
	if got := req.Messages[3].Content.Text; got != "" {
		t.Errorf("纯图片 content = %q，期望空串", got)
	}
	if !req.Messages[3].Content.OK {
		t.Error("纯图片消息应被判定为解析成功（OK=true）")
	}
}

// TestInboundContentNullAndMissing 守 content 缺失/null 不报错。
// 助手消息带 tool_calls 时常常没有 content。
func TestInboundContentNullAndMissing(t *testing.T) {
	cases := []string{
		`{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
		`{"role":"assistant","tool_calls":[]}`,
	}
	for i, c := range cases {
		var m InboundMessage
		if err := json.Unmarshal([]byte(c), &m); err != nil {
			t.Fatalf("case %d 解析失败: %v", i, err)
		}
		if m.Content.Text != "" {
			t.Errorf("case %d: Text = %q，期望空", i, m.Content.Text)
		}
	}
}
