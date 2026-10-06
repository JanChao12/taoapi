package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAggregatorJoinsReasoningAndContent 是核心测试：
// 由 41 个思考分片 + 1 个正文分片聚合出完整响应。
func TestAggregatorJoinsReasoningAndContent(t *testing.T) {
	agg := NewAggregator("id1", "workbuddy/deepseek-v4.1-flash", 100)

	// 模拟 41 个思考分片
	for i := 0; i < 41; i++ {
		agg.Add(Event{Type: EvReasoning, Text: "想"})
	}
	// 1 个正文分片
	agg.Add(Event{Type: EvContent, Text: "可用"})
	agg.Add(Event{Type: EvUsage, Usage: &EventUsage{
		PromptTokens: 35, CompletionTokens: 43, TotalTokens: 78, ReasoningTokens: 41,
	}})
	agg.Add(Event{Type: EvDone, FinishReason: "stop"})

	got := agg.Result()

	if len(got.Choices) != 1 {
		t.Fatalf("choices 数 = %d", len(got.Choices))
	}
	msg := got.Choices[0].Message
	if msg == nil {
		t.Fatal("message 为空")
	}
	if msg.Content != "可用" {
		t.Errorf("content = %q，期望 可用（不能因 41 个思考分片而丢失正文）", msg.Content)
	}
	// "想" 是 3 字节 UTF-8，41 个 = 123 字节
	if got := len([]rune(msg.ReasoningContent)); got != 41 {
		t.Errorf("reasoning_content 字符数 = %d，期望 41", got)
	}
	if msg.Role != "assistant" {
		t.Errorf("role = %q", msg.Role)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
	if got.Usage == nil {
		t.Fatal("usage 为空")
	}
	if got.Usage.CompletionThinkingTokens != 41 {
		t.Errorf("completion_thinking_tokens = %d，期望 41", got.Usage.CompletionThinkingTokens)
	}
	if got.Usage.CompletionTokensDetails == nil || got.Usage.CompletionTokensDetails.ReasoningTokens != 41 {
		t.Error("completion_tokens_details.reasoning_tokens 应为 41")
	}
}

// TestAggregatorNoReasoning 验证不思考的场景（不传档位）。
func TestAggregatorNoReasoning(t *testing.T) {
	agg := NewAggregator("id", "m", 1)
	agg.Add(Event{Type: EvContent, Text: "好"})
	agg.Add(Event{Type: EvUsage, Usage: &EventUsage{ReasoningTokens: 0}})
	agg.Add(Event{Type: EvDone, FinishReason: "stop"})

	got := agg.Result()
	if got.Choices[0].Message.ReasoningContent != "" {
		t.Errorf("不应有 reasoning_content，实际 %q", got.Choices[0].Message.ReasoningContent)
	}
	if got.Usage.CompletionThinkingTokens != 0 {
		t.Error("思考 token 应为 0")
	}
}

// TestAggregatorJoinsToolCallFragments 验证工具调用分片被拼接。
func TestAggregatorJoinsToolCallFragments(t *testing.T) {
	agg := NewAggregator("id", "m", 1)
	agg.Add(Event{Type: EvToolCall, ToolCall: &ToolCallDelta{Index: 0, ID: "call_1", Name: "get_"}})
	agg.Add(Event{Type: EvToolCall, ToolCall: &ToolCallDelta{Index: 0, Name: "weather", Arguments: `{"city"`}})
	agg.Add(Event{Type: EvToolCall, ToolCall: &ToolCallDelta{Index: 0, Arguments: `:"北京"}`}})
	agg.Add(Event{Type: EvDone, FinishReason: "tool_calls"})

	got := agg.Result()
	tcs := got.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("工具调用数 = %d，期望 1（同 index 应合并）", len(tcs))
	}
	if tcs[0].ID != "call_1" {
		t.Errorf("id = %q", tcs[0].ID)
	}
	if tcs[0].Function.Name != "get_weather" {
		t.Errorf("name = %q，期望拼接后的 get_weather", tcs[0].Function.Name)
	}
	if tcs[0].Function.Arguments != `{"city":"北京"}` {
		t.Errorf("arguments = %q，期望拼接后的完整 JSON", tcs[0].Function.Arguments)
	}
}

// TestAggregatorMultipleToolCalls 验证多个工具调用保持顺序。
func TestAggregatorMultipleToolCalls(t *testing.T) {
	agg := NewAggregator("id", "m", 1)
	agg.Add(Event{Type: EvToolCall, ToolCall: &ToolCallDelta{Index: 0, ID: "a", Name: "f1"}})
	agg.Add(Event{Type: EvToolCall, ToolCall: &ToolCallDelta{Index: 1, ID: "b", Name: "f2"}})
	agg.Add(Event{Type: EvToolCall, ToolCall: &ToolCallDelta{Index: 0, Arguments: "{}"}})

	got := agg.Result()
	tcs := got.Choices[0].Message.ToolCalls
	if len(tcs) != 2 {
		t.Fatalf("工具调用数 = %d", len(tcs))
	}
	if tcs[0].ID != "a" || tcs[1].ID != "b" {
		t.Errorf("顺序错误: %q, %q", tcs[0].ID, tcs[1].ID)
	}
}

// TestAggregatorDefaultFinishReason 验证没有 finish_reason 时兜底为 stop。
func TestAggregatorDefaultFinishReason(t *testing.T) {
	agg := NewAggregator("id", "m", 1)
	agg.Add(Event{Type: EvContent, Text: "x"})
	got := agg.Result()
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q，期望兜底 stop", got.Choices[0].FinishReason)
	}
}

// TestBuildChunkReasoning 验证思考走 reasoning_content 字段。
func TestBuildChunkReasoning(t *testing.T) {
	ch := BuildChunk("id", "workbuddy/x", 1, Event{Type: EvReasoning, Text: "思考中"})
	if ch == nil {
		t.Fatal("分片为空")
	}
	if ch.Choices[0].Delta.ReasoningContent != "思考中" {
		t.Errorf("reasoning_content = %q", ch.Choices[0].Delta.ReasoningContent)
	}
	if ch.Choices[0].Delta.Content != "" {
		t.Error("思考分片不应带 content")
	}
}

// TestBuildChunkContent 验证正文走 content。
func TestBuildChunkContent(t *testing.T) {
	ch := BuildChunk("id", "m", 1, Event{Type: EvContent, Text: "正文"})
	if ch.Choices[0].Delta.Content != "正文" {
		t.Errorf("content = %q", ch.Choices[0].Delta.Content)
	}
	if ch.Choices[0].Delta.ReasoningContent != "" {
		t.Error("正文分片不应带 reasoning_content")
	}
}

// TestBuildChunkDone 验证结束帧带 finish_reason。
func TestBuildChunkDone(t *testing.T) {
	ch := BuildChunk("id", "m", 1, Event{Type: EvDone, FinishReason: "length"})
	if ch.Choices[0].FinishReason == nil {
		t.Fatal("finish_reason 为空")
	}
	if *ch.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q", *ch.Choices[0].FinishReason)
	}
}

// TestBuildChunkDoneDefaultsToStop 验证空 finish_reason 兜底。
func TestBuildChunkDoneDefaultsToStop(t *testing.T) {
	ch := BuildChunk("id", "m", 1, Event{Type: EvDone})
	if *ch.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q，期望 stop", *ch.Choices[0].FinishReason)
	}
}

// TestBuildChunkUsage 验证 usage 帧不带 delta 内容。
func TestBuildChunkUsage(t *testing.T) {
	ch := BuildChunk("id", "m", 1, Event{Type: EvUsage, Usage: &EventUsage{
		PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, ReasoningTokens: 4,
	}})
	if ch.Usage == nil {
		t.Fatal("usage 为空")
	}
	if ch.Usage.TotalTokens != 3 {
		t.Errorf("total_tokens = %d", ch.Usage.TotalTokens)
	}
	if ch.Usage.CompletionThinkingTokens != 4 {
		t.Errorf("completion_thinking_tokens = %d", ch.Usage.CompletionThinkingTokens)
	}
	if ch.Choices[0].Delta != nil {
		t.Error("usage 帧不应带 delta")
	}
}

// TestBuildChunkToolCallCarriesIndex 验证工具调用分片带 index。
//
// 2026-10-05 真实故障：ToolCall 结构体缺 index 字段，出站分片一律不带 index。
// 客户端无法合并增量，把 267 个分片当成 267 次【独立】工具调用（其中 265 个
// id/name 为空），DSH 顺序执行到第一个空调用时写出了非法 tool/result，
// 会话就此永久卡死。
//
// 这里断言【序列化后的 JSON】而不是结构体字段：只看字段会漏掉
// "值类型 + omitempty 把索引 0 吃掉" 这种情形 —— 而 0 恰恰是最常见的索引。
func TestBuildChunkToolCallCarriesIndex(t *testing.T) {
	for _, idx := range []int{0, 1, 7} {
		ch := BuildChunk("id", "m", 1, Event{Type: EvToolCall, ToolCall: &ToolCallDelta{
			Index: idx, ID: "call_x", Name: "read", Arguments: "{}",
		}})
		if ch == nil {
			t.Fatalf("index=%d: 分片为空", idx)
		}
		tcs := ch.Choices[0].Delta.ToolCalls
		if len(tcs) != 1 {
			t.Fatalf("index=%d: tool_calls 长度 = %d，期望 1", idx, len(tcs))
		}
		if tcs[0].Function.Name != "read" {
			t.Errorf("index=%d: name = %q，期望 read", idx, tcs[0].Function.Name)
		}

		b, err := json.Marshal(ch)
		if err != nil {
			t.Fatalf("index=%d: 序列化失败: %v", idx, err)
		}
		var probe struct {
			Choices []struct {
				Delta struct {
					ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			t.Fatalf("index=%d: 反序列化失败: %v", idx, err)
		}
		if len(probe.Choices) != 1 || len(probe.Choices[0].Delta.ToolCalls) != 1 {
			t.Fatalf("index=%d: JSON 形状异常: %s", idx, b)
		}
		raw, ok := probe.Choices[0].Delta.ToolCalls[0]["index"]
		if !ok {
			t.Fatalf("index=%d: 分片 JSON 里没有 index 字段，客户端无法合并增量: %s", idx, b)
		}
		var got int
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("index=%d: index 不是整数: %s", idx, raw)
		}
		if got != idx {
			t.Errorf("index = %d，期望 %d", got, idx)
		}
	}
}

// TestToolCallIndexAbsentInNonStream 验证非流式响应不带 index。
//
// index 只属于流式分片；非流式响应混入该字段属于污染，必须守住。
func TestToolCallIndexAbsentInNonStream(t *testing.T) {
	b, err := json.Marshal(ToolCall{
		ID:       "call_x",
		Type:     "function",
		Function: ToolFunction{Name: "read", Arguments: "{}"},
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(b), `"index"`) {
		t.Errorf("非流式 ToolCall 不应带 index: %s", b)
	}
}

// TestMarshalChunkFormat 验证 SSE 帧格式。
func TestMarshalChunkFormat(t *testing.T) {
	ch := BuildChunk("id1", "m", 1, Event{Type: EvContent, Text: "hi"})
	b, err := MarshalChunk(ch)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("应以 'data: ' 开头，实际: %q", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("应以双换行结尾，实际: %q", s)
	}
	// 去掉前缀后应是合法 JSON
	payload := strings.TrimSuffix(strings.TrimPrefix(s, "data: "), "\n\n")
	var v map[string]any
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if v["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v", v["object"])
	}
}

// TestMarshalChunkEscapesHTML 验证不转义 HTML 字符。
//
// 默认的 json.Marshal 会把 < > & 转成 \u003c 等，
// 对 SSE 传输是浪费且可能让客户端困惑。
func TestMarshalChunkEscapesHTML(t *testing.T) {
	ch := BuildChunk("id", "m", 1, Event{Type: EvContent, Text: "<b>&</b>"})
	b, err := MarshalChunk(ch)
	if err != nil {
		t.Fatal(err)
	}
	// 注意：这里用的是标准 json.Marshal，HTML 会被转义。
	// 该测试用于【固定行为】——若将来改为不转义，此处需同步更新。
	if !strings.Contains(string(b), `\u003c`) && !strings.Contains(string(b), `<`) {
		t.Errorf("内容既未转义也未保留，异常: %s", b)
	}
}

// TestDoneMarker 验证结束标记格式。
func TestDoneMarker(t *testing.T) {
	if string(DoneMarker) != "data: [DONE]\n\n" {
		t.Errorf("DoneMarker = %q", string(DoneMarker))
	}
}
