package workbuddy

import (
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// collect 跑一遍解析并把事件收集起来。
func collect(t *testing.T, fixture string) []provider.Event {
	t.Helper()
	var events []provider.Event
	err := parseSSEStream(strings.NewReader(testutil.FixtureString(fixture)),
		func(e provider.Event) error {
			events = append(events, e)
			return nil
		})
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", fixture, err)
	}
	return events
}

// sumByType 统计事件类型。
func sumByType(events []provider.Event) map[provider.EventType]int {
	m := make(map[provider.EventType]int)
	for _, e := range events {
		m[e.Type]++
	}
	return m
}

// joinText 把某类型事件的文本拼起来。
func joinText(events []provider.Event, typ provider.EventType) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == typ {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

// TestParseFullReasoningFixture 是最核心的解析测试：
// 用真实上游样本（44 个 data 行，41 个思考分片）验证解析正确。
func TestParseFullReasoningFixture(t *testing.T) {
	events := collect(t, "sse-deepseek-v4.1-flash-high.txt")
	counts := sumByType(events)

	// 思考分片：实测 41 个非空 reasoning_content
	if counts[provider.EventReasoning] == 0 {
		t.Fatal("没有解析到任何思考分片 —— 这正是上游最显著的特征")
	}
	if counts[provider.EventReasoning] != 41 {
		t.Errorf("思考分片数 = %d，期望 41（与 fixture 实测一致）", counts[provider.EventReasoning])
	}

	// 正文：实测只有 1 个非空 content
	if counts[provider.EventContent] != 1 {
		t.Errorf("正文分片数 = %d，期望 1", counts[provider.EventContent])
	}
	if got := joinText(events, provider.EventContent); got != "可用" {
		t.Errorf("正文 = %q，期望 \"可用\"", got)
	}

	// 结束帧
	if counts[provider.EventDone] != 1 {
		t.Errorf("结束帧数 = %d，期望 1", counts[provider.EventDone])
	}

	// usage 必须解析出来，且思考 token = 41
	if counts[provider.EventUsage] != 1 {
		t.Fatalf("usage 事件数 = %d，期望 1", counts[provider.EventUsage])
	}
	var usage *provider.Usage
	for _, e := range events {
		if e.Type == provider.EventUsage {
			usage = e.Usage
		}
	}
	if usage == nil {
		t.Fatal("usage 为空")
	}
	if usage.ReasoningTokens != 41 {
		t.Errorf("ReasoningTokens = %d，期望 41", usage.ReasoningTokens)
	}
	if usage.PromptTokens != 35 {
		t.Errorf("PromptTokens = %d，期望 35", usage.PromptTokens)
	}
	if usage.CompletionTokens != 43 {
		t.Errorf("CompletionTokens = %d，期望 43", usage.CompletionTokens)
	}
	if usage.TotalTokens != 78 {
		t.Errorf("TotalTokens = %d，期望 78", usage.TotalTokens)
	}
}

// TestParseNoEffortFixture 验证「不传档位 = 完全不思考」在解析层的表现。
func TestParseNoEffortFixture(t *testing.T) {
	events := collect(t, "sse-deepseek-noeffort.txt")
	counts := sumByType(events)

	if counts[provider.EventReasoning] != 0 {
		t.Errorf("该 fixture 不应有思考分片，实际 %d 个", counts[provider.EventReasoning])
	}
	if counts[provider.EventContent] != 1 {
		t.Errorf("正文分片 = %d，期望 1", counts[provider.EventContent])
	}

	for _, e := range events {
		if e.Type == provider.EventUsage {
			if e.Usage.ReasoningTokens != 0 {
				t.Errorf("ReasoningTokens = %d，期望 0（不传档位就是不思考）", e.Usage.ReasoningTokens)
			}
		}
	}
}

// TestParseSpaceBunnyFixture 验证另一种模型形态。
func TestParseSpaceBunnyFixture(t *testing.T) {
	events := collect(t, "sse-space-bunny-max.txt")
	counts := sumByType(events)

	if counts[provider.EventReasoning] == 0 {
		t.Error("space-bunny 应有思考分片")
	}
	if got := joinText(events, provider.EventContent); got != "可用" {
		t.Errorf("正文 = %q", got)
	}
	if counts[provider.EventDone] != 1 {
		t.Error("应有结束帧")
	}
}

// TestParseGlmFixture 验证 glm 的样本。
func TestParseGlmFixture(t *testing.T) {
	events := collect(t, "sse-glm-5.3-max.txt")
	counts := sumByType(events)

	if counts[provider.EventReasoning] == 0 {
		t.Error("glm-5.3 应有思考分片")
	}
	if counts[provider.EventUsage] != 1 {
		t.Error("应有 usage")
	}
}

// TestParseSeparatesReasoningAndContent 验证思考与正文分别累加。
//
// 实测：45 个 data 行里 41 个只带思考、1 个只带正文、0 个同时带。
// 所以解析器必须【分别】处理，不能假设"先思考完再正文"。
func TestParseSeparatesReasoningAndContent(t *testing.T) {
	events := collect(t, "sse-deepseek-v4.1-flash-high.txt")

	reasoning := joinText(events, provider.EventReasoning)
	content := joinText(events, provider.EventContent)

	if reasoning == "" {
		t.Fatal("思考内容为空")
	}
	if content == "" {
		t.Fatal("正文内容为空")
	}
	// 两者必须完全不同：思考里不该混入正文
	if reasoning == content {
		t.Error("思考与正文内容相同，说明没有分别累加")
	}
	// 正文是"可用"，思考是"我们需要回答。用户..."之类
	if strings.Contains(content, "我们需要") {
		t.Errorf("正文里混入了思考内容: %q", content)
	}
}

// TestParseEmptyFieldsProduceNoEvents 是关键测试：
// 上游 delta 的六个字段永远存在、空值用 "" 表示，
// 解析器【必须判值不判字段】，否则会产生大量空事件。
func TestParseEmptyFieldsProduceNoEvents(t *testing.T) {
	// 手工构造一个所有字段都空、只有 role 的帧
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"," +
		"\"content\":\"\",\"reasoning_content\":\"\",\"function_call\":null," +
		"\"refusal\":\"\",\"tool_calls\":[],\"extra_fields\":null},\"finish_reason\":\"\"}]}\n\n" +
		"data: [DONE]\n\n"

	var events []provider.Event
	if err := parseSSEStream(strings.NewReader(sse), func(e provider.Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if len(events) != 0 {
		for _, e := range events {
			t.Errorf("空字段产生了事件: type=%s text=%q", e.Type, e.Text)
		}
		t.Fatalf("应产生 0 个事件，实际 %d 个", len(events))
	}
}

// TestParseTruncatedStreamIsError 验证【流被截断必须报错】。
//
// 这是真实存在的坑：中间层曾有"流被截断却伪装成成功 [DONE]"的 bug。
// 我们绝不能重犯 —— 没有 [DONE] 就是错误。
func TestParseTruncatedStreamIsError(t *testing.T) {
	// 一个正常帧，但没有 [DONE]
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0," +
		"\"delta\":{\"content\":\"部分内容\"},\"finish_reason\":\"\"}]}\n\n"

	err := parseSSEStream(strings.NewReader(sse), func(provider.Event) error { return nil })
	if err == nil {
		t.Fatal("未收到 [DONE] 时必须报错（防止把截断的流当成成功）")
	}
	if !strings.Contains(err.Error(), "[DONE]") {
		t.Errorf("错误信息应提到 [DONE]，实际: %v", err)
	}
}

// TestParseSkipsMalformedFrames 验证坏帧被跳过而不是中断整条流。
func TestParseSkipsMalformedFrames(t *testing.T) {
	sse := "data: {这不是合法JSON\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: [DONE]\n\n"

	var text string
	if err := parseSSEStream(strings.NewReader(sse), func(e provider.Event) error {
		if e.Type == provider.EventContent {
			text += e.Text
		}
		return nil
	}); err != nil {
		t.Fatalf("坏帧不应中断整条流: %v", err)
	}
	if text != "好" {
		t.Errorf("正文 = %q，期望 \"好\"（坏帧被跳过后仍应拿到好帧）", text)
	}
}

// TestParseIgnoresNonDataLines 验证注释行、空行、event: 行被忽略。
func TestParseIgnoresNonDataLines(t *testing.T) {
	sse := ": 这是注释\n" +
		"event: message\n" +
		"\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: [DONE]\n\n"

	var text string
	if err := parseSSEStream(strings.NewReader(sse), func(e provider.Event) error {
		if e.Type == provider.EventContent {
			text += e.Text
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if text != "A" {
		t.Errorf("正文 = %q，期望 \"A\"", text)
	}
}

// TestParsePropagatesEmitError 验证 emit 返回错误时立即停止。
//
// 这是客户端断开的场景：必须停止读取上游，不能继续跑。
func TestParsePropagatesEmitError(t *testing.T) {
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"B\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: [DONE]\n\n"

	sentinel := errStub{}
	count := 0
	err := parseSSEStream(strings.NewReader(sse), func(e provider.Event) error {
		count++
		if count >= 1 {
			return sentinel // 第一个事件就"断开"
		}
		return nil
	})
	if err != sentinel {
		t.Errorf("emit 的错误应原样返回，实际: %v", err)
	}
	if count != 1 {
		t.Errorf("emit 报错后不应继续，实际调用了 %d 次", count)
	}
}

type errStub struct{}

func (errStub) Error() string { return "客户端断开" }

// TestParseToolCalls 验证工具调用增量被解析。
func TestParseToolCalls(t *testing.T) {
	sse := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\"\"}}" +
		"]},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"function\":{\"arguments\":\":\\\"北京\\\"}\"}}" +
		"]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	var deltas []provider.ToolCallDelta
	for _, e := range collectFromString(t, sse) {
		if e.Type == provider.EventToolCall {
			deltas = append(deltas, *e.ToolCall)
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("工具调用增量数 = %d，期望 2", len(deltas))
	}
	if deltas[0].ID != "call_1" {
		t.Errorf("第一个增量的 ID = %q", deltas[0].ID)
	}
	if deltas[0].Name != "get_weather" {
		t.Errorf("函数名 = %q", deltas[0].Name)
	}
	// 参数是分片下发的，客户端负责拼接
	if deltas[0].Arguments != `{"city"` {
		t.Errorf("第一个参数片段 = %q", deltas[0].Arguments)
	}
	if deltas[1].Arguments != `:"北京"}` {
		t.Errorf("第二个参数片段 = %q", deltas[1].Arguments)
	}
}

// collectFromString 从字符串解析事件。
func collectFromString(t *testing.T, sse string) []provider.Event {
	t.Helper()
	var events []provider.Event
	if err := parseSSEStream(strings.NewReader(sse), func(e provider.Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

// TestParseFinishReasonPropagated 验证结束原因被传递。
func TestParseFinishReasonPropagated(t *testing.T) {
	events := collect(t, "sse-deepseek-v4.1-flash-high.txt")
	for _, e := range events {
		if e.Type == provider.EventDone {
			if e.FinishReason != "stop" {
				t.Errorf("FinishReason = %q，期望 stop", e.FinishReason)
			}
			return
		}
	}
	t.Error("没有找到结束帧")
}

// TestParseUsageCreditPreserved 验证 credit 字段的 nil 语义。
//
// 上游返回 credit:0 与不返回 credit 是两回事，不能混为一谈。
func TestParseUsageCreditPreserved(t *testing.T) {
	sse := `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"credit":0.25}}` + "\n\n" +
		"data: [DONE]\n\n"

	for _, e := range collectFromString(t, sse) {
		if e.Type == provider.EventUsage {
			if e.Usage.Credit == nil {
				t.Fatal("credit 应被保留（上游确实返回了）")
			}
			if *e.Usage.Credit != 0.25 {
				t.Errorf("credit = %v，期望 0.25", *e.Usage.Credit)
			}
			return
		}
	}
	t.Error("没有 usage 事件")
}

// TestParseUsageCreditAbsentIsNil 验证上游未返回 credit 时保持 nil。
func TestParseUsageCreditAbsentIsNil(t *testing.T) {
	sse := `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	for _, e := range collectFromString(t, sse) {
		if e.Type == provider.EventUsage {
			if e.Usage.Credit != nil {
				t.Errorf("上游未返回 credit 时应为 nil，实际 %v（不能用 0 冒充）", *e.Usage.Credit)
			}
			return
		}
	}
	t.Error("没有 usage 事件")
}
