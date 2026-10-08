package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件是流式出站的 1:1 护栏测试。
//
// 🔴 断言的是**逐帧的字节形状**，不是"没报错"。
//	参考实现（rockswang/wild-work）修掉的三个缺陷全都属于
//	"不报错但语义错"，只断言 err == nil 的测试抓不到任何一个。

// sseRecorder 收集流式帧并解析成 (event, data) 列表。
type sseRecorder struct {
	frames []string
}

func (r *sseRecorder) write(b []byte) error {
	r.frames = append(r.frames, string(b))
	return nil
}

// events 解析出所有事件的 (name, payload)。
func (r *sseRecorder) events(t *testing.T) []struct {
	Name string
	Data map[string]any
} {
	t.Helper()
	var out []struct {
		Name string
		Data map[string]any
	}
	for _, f := range r.frames {
		var name, data string
		for _, line := range strings.Split(strings.TrimRight(f, "\n"), "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			t.Errorf("帧缺少 event: 行 —— Anthropic 客户端按事件名分派，只写 data 会被忽略。帧=%q", f)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Errorf("事件 %s 的 data 不是合法 JSON: %v（%q）", name, err, data)
			continue
		}
		out = append(out, struct {
			Name string
			Data map[string]any
		}{name, payload})
	}
	return out
}

// names 返回事件名序列。
func (r *sseRecorder) names(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range r.events(t) {
		out = append(out, e.Name)
	}
	return out
}

// blockIndex 取出事件的 index；非块事件（message_start/message_stop/error 等）
// 没有该字段，返回 -1。
//
// ⚠️ 不能直接断言 float64：message_start / message_stop / error 都不带 index，
// 直接转换会 panic（本测试首次运行就踩到了）。
func blockIndex(t *testing.T, data map[string]any) int {
	t.Helper()
	v, has := data["index"]
	if !has || v == nil {
		return -1
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("index 字段类型 = %T，期望数字", v)
	}
	return int(f)
}

// drive 跑一条事件序列并返回记录器。
func drive(t *testing.T, evs ...provider.Event) *sseRecorder {
	t.Helper()
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-123", "m")
	if err := sw.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	for _, ev := range evs {
		if err := sw.Handle(ev); err != nil {
			t.Fatalf("Handle(%v) 失败: %v", ev.Type, err)
		}
	}
	if err := sw.Finish(); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
	return rec
}

func content(text string) provider.Event {
	return provider.Event{Type: provider.EventContent, Text: text}
}
func reasoning(text string) provider.Event {
	return provider.Event{Type: provider.EventReasoning, Text: text}
}
func toolCall(idx int, id, name, args string) provider.Event {
	return provider.Event{
		Type:     provider.EventToolCall,
		ToolCall: &provider.ToolCallDelta{Index: idx, ID: id, Name: name, Arguments: args},
	}
}
func done(reason string) provider.Event {
	return provider.Event{Type: provider.EventDone, FinishReason: reason}
}
func usage(prompt, completion int64) provider.Event {
	return provider.Event{Type: provider.EventUsage,
		Usage: &provider.Usage{PromptTokens: prompt, CompletionTokens: completion}}
}

// ─────────────────────────────────────────────────────────────
// 事件序列骨架
// ─────────────────────────────────────────────────────────────

// TestStreamBasicSequence 守最简序列：start → text → stop → delta → stop。
func TestStreamBasicSequence(t *testing.T) {
	rec := drive(t, content("你好"), done("stop"))
	got := rec.names(t)

	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列 = %v\n期望 = %v", got, want)
	}
}

// TestStreamMessageStartShape 守 message_start 的形状。
func TestStreamMessageStartShape(t *testing.T) {
	rec := drive(t, content("x"), done("stop"))
	evs := rec.events(t)

	msg, _ := evs[0].Data["message"].(map[string]any)
	if msg == nil {
		t.Fatal("message_start 缺少 message 对象")
	}
	if msg["type"] != "message" {
		t.Errorf("message.type = %v，期望 message", msg["type"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("message.role = %v，期望 assistant", msg["role"])
	}
	if msg["model"] != "m" {
		t.Errorf("message.model = %v，期望客户端请求的模型名 m", msg["model"])
	}
	// content 必须是数组（哪怕是空）
	if _, ok := msg["content"].([]any); !ok {
		t.Errorf("message.content = %v，期望数组", msg["content"])
	}
	// id 必须是 msg_ 前缀
	id, _ := msg["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("message.id = %q，期望 msg_ 前缀", id)
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 缺陷 1：工具参数必须走 input_json_delta
// ─────────────────────────────────────────────────────────────

// TestToolArgsStreamViaInputJsonDelta 守缺陷 1。
//
// 🔴 参考实现修掉的真实缺陷：tokligence-gateway 在 content_block_start 里
//
//	**一次性给完整 input**，导致等 partial_json 的客户端永远卡住
//	（它按"块开了但参数还没来"等待）。
//
// 本测试断言两件事：
//  1. content_block_start 里的 input 必须是**空对象**
//  2. 参数必须通过 input_json_delta 的 partial_json 下发
func TestToolArgsStreamViaInputJsonDelta(t *testing.T) {
	rec := drive(t,
		toolCall(0, "toolu_1", "get_weather", `{"city":`),
		toolCall(0, "", "", `"北京"}`),
		done("tool_calls"),
	)
	evs := rec.events(t)

	var startInput any
	var hasInputDelta bool
	var partial strings.Builder

	for _, e := range evs {
		if e.Name == "content_block_start" {
			cb, _ := e.Data["content_block"].(map[string]any)
			if cb != nil && cb["type"] == "tool_use" {
				startInput = cb["input"]
			}
		}
		if e.Name == "content_block_delta" {
			d, _ := e.Data["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				hasInputDelta = true
				if s, ok := d["partial_json"].(string); ok {
					partial.WriteString(s)
				}
			}
		}
	}

	// ① input 必须是空对象（不能一次性给完整参数）
	m, ok := startInput.(map[string]any)
	if !ok {
		t.Fatalf("tool_use 的 input = %#v，期望空对象 {}", startInput)
	}
	if len(m) != 0 {
		t.Errorf("content_block_start 里一次性给了完整 input %v —— "+
			"等 partial_json 的客户端会永远卡住，参数必须走 input_json_delta", m)
	}

	// ② 必须有 input_json_delta
	if !hasInputDelta {
		t.Fatal("没有 input_json_delta —— 客户端等不到工具参数")
	}

	// ③ 分片必须被拼成完整参数
	if partial.String() != `{"city":"北京"}` {
		t.Errorf("partial_json 拼接 = %q，期望 %q（分片必须原样透传）",
			partial.String(), `{"city":"北京"}`)
	}
}

// TestToolBlocksPaired 守：每个 tool_use 块必须成对开闭，index 从 2 递增。
func TestToolBlocksPaired(t *testing.T) {
	rec := drive(t,
		toolCall(0, "t1", "f1", `{}`),
		toolCall(1, "t2", "f2", `{}`),
		done("tool_calls"),
	)

	var starts, stops []int
	for _, e := range rec.events(t) {
		idx := blockIndex(t, e.Data)
		switch e.Name {
		case "content_block_start":
			starts = append(starts, idx)
		case "content_block_stop":
			stops = append(stops, idx)
		}
	}
	if len(starts) != 2 || len(stops) != 2 {
		t.Fatalf("start/stop 数 = %d/%d，期望 2/2", len(starts), len(stops))
	}
	if starts[0] != IndexToolBase || starts[1] != IndexToolBase+1 {
		t.Errorf("工具块 index = %v，期望从 %d 起递增", starts, IndexToolBase)
	}
	if starts[0] != stops[0] || starts[1] != stops[1] {
		t.Errorf("块未成对：starts=%v stops=%v", starts, stops)
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 缺陷 2：stop_reason 如实映射
// ─────────────────────────────────────────────────────────────

// TestStopReasonTruthfulMapping 守缺陷 2。
//
// 🔴 参考实现修掉的真实缺陷：tokligence-gateway 把 stop_reason **恒为
//
//	end_turn**，于是客户端不知道模型要调工具，agent 循环直接断掉。
func TestStopReasonTruthfulMapping(t *testing.T) {
	cases := []struct {
		name string
		evs  []provider.Event
		want string
	}{
		{"自然结束", []provider.Event{content("x"), done("stop")}, "end_turn"},
		{"长度截断", []provider.Event{content("x"), done("length")}, "max_tokens"},
		{"工具调用", []provider.Event{toolCall(0, "t", "f", "{}"), done("tool_calls")}, "tool_use"},
		{"finish 为空但有工具", []provider.Event{toolCall(0, "t", "f", "{}"), done("")}, "tool_use"},
		{"内容过滤", []provider.Event{content("x"), done("content_filter")}, "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := drive(t, tc.evs...)
			var got string
			for _, e := range rec.events(t) {
				if e.Name == "message_delta" {
					d, _ := e.Data["delta"].(map[string]any)
					got, _ = d["stop_reason"].(string)
				}
			}
			if got != tc.want {
				t.Errorf("stop_reason = %q，期望 %q —— "+
					"恒为 end_turn 会让客户端不知道要调工具", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 缺陷 3：usage 必须是真实值
// ─────────────────────────────────────────────────────────────

// TestUsageUsesRealValues 守缺陷 3。
//
// 🔴 参考实现修掉的真实缺陷：usage 恒为 0，会让客户端的上下文预算
//
//	彻底失效（客户端以为上下文还空着，于是继续往里塞）。
func TestUsageUsesRealValues(t *testing.T) {
	rec := drive(t, content("x"), usage(1234, 567), done("stop"))

	var got map[string]any
	for _, e := range rec.events(t) {
		if e.Name == "message_delta" {
			got, _ = e.Data["usage"].(map[string]any)
		}
	}
	if got == nil {
		t.Fatal("message_delta 缺少 usage —— 客户端拿不到用量")
	}
	if got["input_tokens"] != float64(1234) {
		t.Errorf("input_tokens = %v，期望上游真实值 1234", got["input_tokens"])
	}
	if got["output_tokens"] != float64(567) {
		t.Errorf("output_tokens = %v，期望上游真实值 567", got["output_tokens"])
	}
}

// TestUsageOmittedWhenUpstreamGaveNone 守：上游没给 usage 时**不输出**该字段。
//
// 🔴 输出 0 会被客户端当成"确实消耗 0 token"，那是伪造数据。
//
//	"未知"与"0"含义完全不同。
func TestUsageOmittedWhenUpstreamGaveNone(t *testing.T) {
	rec := drive(t, content("x"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name == "message_delta" {
			if _, has := e.Data["usage"]; has {
				t.Error("上游没给 usage 却输出了该字段 —— " +
					"0 会被当成「确实消耗 0 token」，那是伪造数据")
			}
		}
	}
}

// TestUsageCacheReadTokens 守缓存命中字段被透传。
func TestUsageCacheReadTokens(t *testing.T) {
	rec := drive(t, content("x"),
		provider.Event{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens: 100, CompletionTokens: 10, PromptCacheHitTokens: 80,
		}}, done("stop"))

	for _, e := range rec.events(t) {
		if e.Name == "message_delta" {
			u, _ := e.Data["usage"].(map[string]any)
			if u["cache_read_input_tokens"] != float64(80) {
				t.Errorf("cache_read_input_tokens = %v，期望 80", u["cache_read_input_tokens"])
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 本项目硬约束 4：流中途失败绝不发 message_stop
// ─────────────────────────────────────────────────────────────

// TestStreamNeverStopsOnError 守：失败收尾只发 error，**绝不发 message_stop**。
//
// 🔴 为什么这是硬约束：
//
//	message_stop 在 Anthropic 协议里等同「正常结束」。半截回答配上
//	message_stop，客户端会把它当完整回答继续跑 —— 例如 agent 拿着
//	被截断的 tool_call arguments 去执行，或把半句话当最终答案。
//	error 事件本身就是终止信号。
func TestStreamNeverStopsOnError(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	if err := sw.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sw.Handle(content("半截")); err != nil {
		t.Fatal(err)
	}
	// 模拟流中途失败
	if err := sw.Fail(errStreamBroken); err != nil {
		t.Fatalf("Fail 失败: %v", err)
	}

	got := rec.names(t)
	for _, n := range got {
		if n == "message_stop" {
			t.Fatalf("失败收尾发出了 message_stop —— 客户端会把半截回答"+
				"当成完整回答继续跑。实际序列: %v", got)
		}
		if n == "message_delta" {
			t.Fatalf("失败收尾发出了 message_delta（含 stop_reason）—— "+
				"等同宣告正常结束。实际序列: %v", got)
		}
	}
	// 必须有 error 事件
	sawError := false
	for _, n := range got {
		if n == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("失败收尾没有 error 事件 —— 客户端无法得知请求失败。实际序列: %v", got)
	}
}

// TestStreamFailClosesBlocks 守：失败收尾仍要关闭已开的块（协议要求成对）。
func TestStreamFailClosesBlocks(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	if err := sw.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sw.Handle(content("x")); err != nil {
		t.Fatal(err)
	}
	if err := sw.Fail(errStreamBroken); err != nil {
		t.Fatal(err)
	}

	var starts, stops int
	for _, n := range rec.names(t) {
		switch n {
		case "content_block_start":
			starts++
		case "content_block_stop":
			stops++
		}
	}
	if starts != stops {
		t.Errorf("块未成对：start=%d stop=%d —— 协议要求成对出现", starts, stops)
	}
}

// TestStreamErrorTypeIsCanonical 守：error.type 只能用规范 9 种枚举。
//
// 🔴 SDK 的 ErrorObject 是 9 元判别联合（type 为 Literal 标签）。
//
//	自造类型（如把内层私有码 upstream_rate_limited 填进去）会让严格
//	客户端判为未知类型。官方文档说"客户端应优雅处理未知 type"——
//	反过来说**服务端不得自造**。
func TestStreamErrorTypeIsCanonical(t *testing.T) {
	canonical := map[string]bool{
		ErrInvalidRequest: true, ErrAuthentication: true, ErrBilling: true,
		ErrPermission: true, ErrNotFound: true, ErrRateLimit: true,
		ErrGatewayTimeout: true, ErrAPI: true, ErrOverloaded: true,
	}
	if len(canonical) != 9 {
		t.Fatalf("规范枚举数 = %d，期望 9", len(canonical))
	}

	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Fail(errStreamBroken)

	for _, e := range rec.events(t) {
		if e.Name != "error" {
			continue
		}
		inner, _ := e.Data["error"].(map[string]any)
		typ, _ := inner["type"].(string)
		if !canonical[typ] {
			t.Errorf("error.type = %q 不在规范 9 种枚举内 —— 不得自造", typ)
		}
	}
}

// TestErrorFrameNeverEmptyType 守：ErrorFrame 的 type 永不落空。
func TestErrorFrameNeverEmptyType(t *testing.T) {
	b, err := ErrorFrame("", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"api_error"`) {
		t.Errorf("空 errType 未回落为 api_error: %s", b)
	}
}

// TestErrorTypeForClassification 守错误分类映射。
func TestErrorTypeForClassification(t *testing.T) {
	if got := ErrorTypeFor(nil); got != ErrAPI {
		t.Errorf("nil 错误 → %q，期望 api_error", got)
	}
	if got := ErrorTypeFor(errStreamBroken); got != ErrAPI {
		t.Errorf("普通错误 → %q，期望 api_error", got)
	}
	// 实现了 provider 分类接口的错误
	if got := ErrorTypeFor(authErr{}); got != ErrAuthentication {
		t.Errorf("鉴权错误 → %q，期望 authentication_error", got)
	}
	if got := ErrorTypeFor(rateErr{}); got != ErrRateLimit {
		t.Errorf("限流错误 → %q，期望 rate_limit_error", got)
	}
}

// ─────────────────────────────────────────────────────────────
// 内容块顺序
// ─────────────────────────────────────────────────────────────

// TestThinkingBlockBeforeText 守：思考块 index 0、文本块 1，且思考先关后开文本。
func TestThinkingBlockBeforeText(t *testing.T) {
	rec := drive(t, reasoning("想"), content("答"), done("stop"))
	got := rec.names(t)

	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta",
		"content_block_stop",  // thinking 先关
		"content_block_start", // text
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列 = %v\n期望 = %v", got, want)
	}

	// index 断言：思考 0、文本 1
	var indices []int
	for _, e := range rec.events(t) {
		if e.Name == "content_block_start" {
			indices = append(indices, blockIndex(t, e.Data))
		}
	}
	if len(indices) != 2 || indices[0] != IndexThinking || indices[1] != IndexText {
		t.Errorf("块 index = %v，期望 [%d %d]（思考必须先于文本）",
			indices, IndexThinking, IndexText)
	}
}

// TestThinkingDeltaUsesThinkingDelta 守 thinking_delta 的类型名。
func TestThinkingDeltaUsesThinkingDelta(t *testing.T) {
	rec := drive(t, reasoning("思考中"), content("答"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name == "content_block_delta" {
			d, _ := e.Data["delta"].(map[string]any)
			if blockIndex(t, e.Data) == IndexThinking {
				if d["type"] != "thinking_delta" {
					t.Errorf("思考块 delta.type = %v，期望 thinking_delta", d["type"])
				}
				if d["thinking"] != "思考中" {
					t.Errorf("thinking_delta.thinking = %v", d["thinking"])
				}
			}
		}
	}
}

// TestTextDeltaUsesTextDelta 守 text_delta 的类型名与字段名。
func TestTextDeltaUsesTextDelta(t *testing.T) {
	rec := drive(t, content("正文"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name == "content_block_delta" {
			d, _ := e.Data["delta"].(map[string]any)
			if d["type"] != "text_delta" {
				t.Errorf("文本块 delta.type = %v，期望 text_delta", d["type"])
			}
			if d["text"] != "正文" {
				t.Errorf("text_delta.text = %v", d["text"])
			}
		}
	}
}

// TestOutOfOrderReasoningDropped 守：文本块已开后又来的思考增量被丢弃。
//
// 🔴 上游顺序异常时若照发，会产生"index 1 的文本块后面又开 index 0 的
//
//	思考块"这种非法序列（Anthropic 要求 index 递增）。
//	宁可少显示思考，也不能产生非法块序列。
func TestOutOfOrderReasoningDropped(t *testing.T) {
	rec := drive(t, content("先正文"), reasoning("迟到的思考"), done("stop"))

	var starts []int
	for _, e := range rec.events(t) {
		if e.Name == "content_block_start" {
			starts = append(starts, blockIndex(t, e.Data))
		}
	}
	if len(starts) != 1 || starts[0] != IndexText {
		t.Errorf("块 index = %v，期望只有文本块 [%d]（乱序思考必须丢弃，不能产生非法序列）",
			starts, IndexText)
	}
	// 并且思考内容不能被混进文本
	for _, e := range rec.events(t) {
		if e.Name == "content_block_delta" {
			d, _ := e.Data["delta"].(map[string]any)
			if s, _ := d["text"].(string); strings.Contains(s, "迟到的思考") {
				t.Error("乱序的思考内容被混进了文本块")
			}
		}
	}
}

// TestNoTextBlockWhenOnlyReasoning 守：只有思考没有正文时**不能**开空的文本块。
func TestNoTextBlockWhenOnlyReasoning(t *testing.T) {
	rec := drive(t, reasoning("只有思考"), done("stop"))
	got := rec.names(t)

	for _, n := range got {
		if n == "content_block_start" {
			// 检查是思考块而非文本块
		}
	}
	var starts []int
	for _, e := range rec.events(t) {
		if e.Name == "content_block_start" {
			starts = append(starts, blockIndex(t, e.Data))
		}
	}
	if len(starts) != 1 || starts[0] != IndexThinking {
		t.Errorf("块 index = %v，期望只有思考块 [%d]（无正文时不该开空文本块）",
			starts, IndexThinking)
	}
}

// TestNoBlocksWhenEmpty 守：完全无内容时不应开任何块，但收尾帧必须齐全。
func TestNoBlocksWhenEmpty(t *testing.T) {
	rec := drive(t, done("stop"))
	got := rec.names(t)
	want := []string{"message_start", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("空流事件序列 = %v\n期望 = %v", got, want)
	}
}

// TestFinishIdempotent 守：重复 Finish / Fail 不重复发帧。
//
// 🔴 真实场景：调用方在流错误后仍可能走到收尾分支。
//
//	若不幂等，客户端会收到两个 message_stop（协议违规）。
func TestFinishIdempotent(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Handle(content("x"))
	_ = sw.Finish()
	n1 := len(rec.frames)
	_ = sw.Finish()
	if len(rec.frames) != n1 {
		t.Errorf("重复 Finish 又写了 %d 帧 —— 必须幂等", len(rec.frames)-n1)
	}
}

// TestFailAfterFinishIsNoop 守：已正常收尾后再 Fail 不发 error。
func TestFailAfterFinishIsNoop(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Handle(content("x"))
	_ = sw.Finish()
	n1 := len(rec.frames)
	_ = sw.Fail(errStreamBroken)
	if len(rec.frames) != n1 {
		t.Error("正常收尾后 Fail 仍写了 error 帧 —— 会让客户端以为请求失败")
	}
}

// TestStartIdempotent 守：重复 Start 不重复发 message_start。
func TestStartIdempotent(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	n1 := len(rec.frames)
	_ = sw.Start()
	if len(rec.frames) != n1 {
		t.Error("重复 Start 又写了 message_start —— 必须幂等")
	}
}

// TestFrameEndsWithBlankLine 守 SSE 帧格式：以空行结束。
func TestFrameEndsWithBlankLine(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	for _, f := range rec.frames {
		if !strings.HasSuffix(f, "\n\n") {
			t.Errorf("帧未以空行结束（SSE 规范要求）: %q", f)
		}
	}
}

// TestToolUseIDFallback 守：上游没给工具 id 时生成 toolu_ 前缀的 id。
//
// 🔴 Anthropic 的 tool_use.id 必填，客户端靠它把 tool_result 关联回来。
func TestToolUseIDFallback(t *testing.T) {
	rec := drive(t, toolCall(0, "", "f", "{}"), done("tool_calls"))
	for _, e := range rec.events(t) {
		if e.Name == "content_block_start" {
			cb, _ := e.Data["content_block"].(map[string]any)
			if cb["type"] != "tool_use" {
				continue
			}
			id, _ := cb["id"].(string)
			if !strings.HasPrefix(id, "toolu_") {
				t.Errorf("缺 id 时生成 %q，期望 toolu_ 前缀", id)
			}
		}
	}
}

// TestToolNameAndArgsSplitAcrossChunks 守：name 分片也要拼接。
func TestToolNameAndArgsSplitAcrossChunks(t *testing.T) {
	rec := drive(t,
		toolCall(0, "t1", "get_", ""),
		toolCall(0, "", "weather", `{"a":1}`),
		done("tool_calls"),
	)
	for _, e := range rec.events(t) {
		if e.Name == "content_block_start" {
			cb, _ := e.Data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" && cb["name"] != "get_weather" {
				t.Errorf("工具名 = %v，期望分片拼接成 get_weather", cb["name"])
			}
		}
	}
}

// TestUsageReturnsNilWhenAbsent 守 Usage() 的 nil 语义。
func TestUsageReturnsNilWhenAbsent(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Handle(content("x"))
	_ = sw.Finish()
	if sw.Usage() != nil {
		t.Error("上游没给 usage 时 Usage() 应为 nil —— " +
			"返回零值会把「未知」记成「确实消耗 0」")
	}
}

// TestUsageReturnsRealValue 守 Usage() 返回真实值。
func TestUsageReturnsRealValue(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Handle(usage(11, 22))
	_ = sw.Finish()
	u := sw.Usage()
	if u == nil || u.PromptTokens != 11 || u.CompletionTokens != 22 {
		t.Errorf("Usage() = %+v，期望 11/22", u)
	}
}

// ─────────────────────────────────────────────────────────────
// 测试替身
// ─────────────────────────────────────────────────────────────

type streamErr struct{ msg string }

func (e streamErr) Error() string { return e.msg }

var errStreamBroken = streamErr{"上游流中断"}

type authErr struct{}

func (authErr) Error() string       { return "鉴权失败" }
func (authErr) IsAuthFailure() bool { return true }
func (authErr) IsRateLimited() bool { return false }

type rateErr struct{}

func (rateErr) Error() string       { return "限流" }
func (rateErr) IsAuthFailure() bool { return false }
func (rateErr) IsRateLimited() bool { return true }
