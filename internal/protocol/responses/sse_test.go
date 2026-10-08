package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件是流式出站的 1:1 护栏测试。
//
// 🔴 断言的是**逐帧的字节形状**，不是"没报错"。
//	与 anthropic 包同一纪律：三个参考实现级缺陷都属于"不报错但语义错"。

// sseRecorder 收集流式帧。
type sseRecorder struct {
	frames []string
}

func (r *sseRecorder) write(b []byte) error {
	r.frames = append(r.frames, string(b))
	return nil
}

type parsedEvent struct {
	Name string
	Data map[string]any
}

// events 解析出所有事件的 (name, payload)。
func (r *sseRecorder) events(t *testing.T) []parsedEvent {
	t.Helper()
	var out []parsedEvent
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
			t.Errorf("帧缺少 event: 行 —— Responses 客户端按事件名分派。帧=%q", f)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Errorf("事件 %s 的 data 不是合法 JSON: %v（%q）", name, err, data)
			continue
		}
		out = append(out, parsedEvent{name, payload})
	}
	return out
}

func (r *sseRecorder) names(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range r.events(t) {
		out = append(out, e.Name)
	}
	return out
}

// has 报告事件名序列里是否含某个事件。
func (r *sseRecorder) has(t *testing.T, name string) bool {
	t.Helper()
	for _, n := range r.names(t) {
		if n == name {
			return true
		}
	}
	return false
}

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

// TestStreamBasicSequence 守最简序列的完整事件名。
func TestStreamBasicSequence(t *testing.T) {
	rec := drive(t, content("你好"), done("stop"))
	got := rec.names(t)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",  // message
		"response.content_part.added", //
		"response.output_text.delta",  //
		"response.output_text.done",   //
		"response.content_part.done",  //
		"response.output_item.done",   // message
		"response.completed",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列 =\n  %v\n期望 =\n  %v", got, want)
	}
}

// TestSequenceNumberMonotonic 守：sequence_number 必须存在且单调递增。
//
// ⚠️ 规范要求该字段。漏掉会让严格客户端无法检测事件乱序/丢失。
func TestSequenceNumberMonotonic(t *testing.T) {
	rec := drive(t, content("x"), done("stop"))
	var last int64 = -1
	n := 0
	for _, e := range rec.events(t) {
		v, has := e.Data["sequence_number"]
		if !has {
			t.Errorf("事件 %s 缺少 sequence_number —— 规范要求该字段", e.Name)
			continue
		}
		f, ok := v.(float64)
		if !ok {
			t.Errorf("事件 %s 的 sequence_number 类型 = %T，期望数字", e.Name, v)
			continue
		}
		cur := int64(f)
		if cur <= last {
			t.Errorf("事件 %s 的 sequence_number = %d，未递增（上一个 %d）",
				e.Name, cur, last)
		}
		last = cur
		n++
	}
	if n == 0 {
		t.Fatal("没有解析到任何事件")
	}
}

// TestEveryEventHasType 守：每个事件的 data 里都要有 type 字段。
func TestEveryEventHasType(t *testing.T) {
	rec := drive(t, reasoning("想"), content("答"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Data["type"] != e.Name {
			t.Errorf("事件 %s 的 data.type = %v，期望与事件名一致",
				e.Name, e.Data["type"])
		}
	}
}

// TestCreatedEventShape 守 response.created 的形状。
func TestCreatedEventShape(t *testing.T) {
	rec := drive(t, content("x"), done("stop"))
	evs := rec.events(t)
	if evs[0].Name != "response.created" {
		t.Fatalf("首个事件 = %s，期望 response.created", evs[0].Name)
	}
	resp, _ := evs[0].Data["response"].(map[string]any)
	if resp == nil {
		t.Fatal("response.created 缺少 response 对象")
	}
	if resp["object"] != "response" {
		t.Errorf("response.object = %v，期望 response", resp["object"])
	}
	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Errorf("response.id = %q，期望 resp_ 前缀", id)
	}
	if resp["model"] != "m" {
		t.Errorf("response.model = %v，期望客户端请求的模型名", resp["model"])
	}
	if _, ok := resp["output"].([]any); !ok {
		t.Errorf("response.output = %v，期望数组", resp["output"])
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 缺陷 1：工具参数必须走 function_call_arguments.delta
// ─────────────────────────────────────────────────────────────

// TestToolArgsStreamViaDelta 守缺陷 1。
//
// 🔴 与 anthropic 的 input_json_delta 同类缺陷：若在 output_item.added 里
//
//	一次性给完整 arguments，**等 .delta 的客户端会永远卡住**
//	（它按"item 开了但参数还没来"等待）。
func TestToolArgsStreamViaDelta(t *testing.T) {
	rec := drive(t,
		toolCall(0, "call_1", "get_weather", `{"city":`),
		toolCall(0, "", "", `"北京"}`),
		done("tool_calls"),
	)

	var addedArgs any
	var hasDelta bool
	var partial strings.Builder

	for _, e := range rec.events(t) {
		switch e.Name {
		case "response.output_item.added":
			item, _ := e.Data["item"].(map[string]any)
			if item != nil && item["type"] == ItemFunctionCall {
				addedArgs = item["arguments"]
			}
		case "response.function_call_arguments.delta":
			hasDelta = true
			if s, ok := e.Data["delta"].(string); ok {
				partial.WriteString(s)
			}
		}
	}

	// ① added 事件里的 arguments 必须是空串
	if s, ok := addedArgs.(string); !ok || s != "" {
		t.Errorf("output_item.added 里的 arguments = %#v，期望空串 —— "+
			"一次性给完整参数会让等 .delta 的客户端卡住", addedArgs)
	}
	// ② 必须有 .delta 事件
	if !hasDelta {
		t.Fatal("没有 response.function_call_arguments.delta —— 客户端等不到工具参数")
	}
	// ③ 分片必须能拼成完整参数
	if partial.String() != `{"city":"北京"}` {
		t.Errorf("delta 拼接 = %q，期望 %q", partial.String(), `{"city":"北京"}`)
	}
}

// TestToolCallHasDoneEvent 守：函数调用 item 有 .done 事件。
func TestToolCallHasDoneEvent(t *testing.T) {
	rec := drive(t, toolCall(0, "c1", "f", `{"a":1}`), done("tool_calls"))
	if !rec.has(t, "response.function_call_arguments.done") {
		t.Error("缺少 response.function_call_arguments.done")
	}
}

// TestToolItemPaired 守：每个 function_call item 都要 added + done 成对。
func TestToolItemPaired(t *testing.T) {
	rec := drive(t,
		toolCall(0, "c1", "f1", `{}`),
		toolCall(1, "c2", "f2", `{}`),
		done("tool_calls"),
	)

	var added, doneCount int
	var addedIdx []int
	for _, e := range rec.events(t) {
		switch e.Name {
		case "response.output_item.added":
			item, _ := e.Data["item"].(map[string]any)
			if item != nil && item["type"] == ItemFunctionCall {
				added++
				addedIdx = append(addedIdx, int(e.Data["output_index"].(float64)))
			}
		case "response.output_item.done":
			item, _ := e.Data["item"].(map[string]any)
			if item != nil && item["type"] == ItemFunctionCall {
				doneCount++
			}
		}
	}
	if added != 2 || doneCount != 2 {
		t.Fatalf("function_call item 的 added/done 数 = %d/%d，期望 2/2", added, doneCount)
	}
	if addedIdx[0] != IndexItemBase || addedIdx[1] != IndexItemBase+1 {
		t.Errorf("function_call output_index = %v，期望从 %d 起递增", addedIdx, IndexItemBase)
	}
}

// TestToolCallIDAndItemIDDistinct 守：item id 与 call_id 是两个不同字段。
//
// 🔴 用错会导致客户端关联不上工具结果（function_call_output 靠 call_id）。
func TestToolCallIDAndItemIDDistinct(t *testing.T) {
	rec := drive(t, toolCall(0, "call_abc", "f", `{}`), done("tool_calls"))
	for _, e := range rec.events(t) {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item == nil || item["type"] != ItemFunctionCall {
			continue
		}
		id, _ := item["id"].(string)
		callID, _ := item["call_id"].(string)
		if !strings.HasPrefix(id, "fc_") {
			t.Errorf("item.id = %q，期望 fc_ 前缀", id)
		}
		if callID != "call_abc" {
			t.Errorf("call_id = %q，期望上游给的 call_abc", callID)
		}
		if id == callID {
			t.Error("item.id 与 call_id 相同 —— 它们是两个不同字段")
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 缺陷 2：status 如实映射
// ─────────────────────────────────────────────────────────────

// TestIncompleteOnLength 守缺陷 2。
//
// 🔴 被 max_output_tokens 截断的响应若标成 completed，
//
//	客户端会把半截回答当完整回答继续跑（与 anthropic 的 stop_reason 同类）。
func TestIncompleteOnLength(t *testing.T) {
	rec := drive(t, content("半截"), done("length"))

	if rec.has(t, "response.completed") {
		t.Fatal("finish_reason=length 却发了 response.completed —— " +
			"客户端会把被截断的回答当成完整回答")
	}
	if !rec.has(t, "response.incomplete") {
		t.Fatalf("finish_reason=length 却没有 response.incomplete。事件序列: %v",
			rec.names(t))
	}
	// incomplete_details.reason 必须如实
	for _, e := range rec.events(t) {
		if e.Name != "response.incomplete" {
			continue
		}
		resp, _ := e.Data["response"].(map[string]any)
		if resp["status"] != StatusIncomplete {
			t.Errorf("response.status = %v，期望 incomplete", resp["status"])
		}
		det, _ := resp["incomplete_details"].(map[string]any)
		if det == nil || det["reason"] != "max_output_tokens" {
			t.Errorf("incomplete_details = %v，期望 reason=max_output_tokens", det)
		}
	}
}

// TestCompletedOnStop 守：正常结束时发 completed 且**不带** incomplete_details。
func TestCompletedOnStop(t *testing.T) {
	rec := drive(t, content("完整"), done("stop"))
	if !rec.has(t, "response.completed") {
		t.Fatal("正常结束却没有 response.completed")
	}
	for _, e := range rec.events(t) {
		if e.Name != "response.completed" {
			continue
		}
		resp, _ := e.Data["response"].(map[string]any)
		if resp["status"] != StatusCompleted {
			t.Errorf("response.status = %v，期望 completed", resp["status"])
		}
		if _, has := resp["incomplete_details"]; has {
			t.Error("正常完成却带了 incomplete_details")
		}
	}
}

// TestContentFilterMapsIncomplete 守：content_filter 也映射为 incomplete。
func TestContentFilterMapsIncomplete(t *testing.T) {
	rec := drive(t, content("x"), done("content_filter"))
	if !rec.has(t, "response.incomplete") {
		t.Errorf("content_filter 未映射为 incomplete。序列: %v", rec.names(t))
	}
}

// TestIncompleteReasonMapping 守映射函数本身。
func TestIncompleteReasonMapping(t *testing.T) {
	cases := map[string]string{
		"length":         "max_output_tokens",
		"max_tokens":     "max_output_tokens",
		"content_filter": "content_filter",
		"stop":           "",
		"":               "",
		"tool_calls":     "",
	}
	for in, want := range cases {
		if got := IncompleteReason(in); got != want {
			t.Errorf("IncompleteReason(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 缺陷 3：usage 必须是真实值
// ─────────────────────────────────────────────────────────────

// TestUsageUsesRealValues 守缺陷 3。
func TestUsageUsesRealValues(t *testing.T) {
	rec := drive(t, content("x"), usage(1234, 567), done("stop"))

	var got map[string]any
	for _, e := range rec.events(t) {
		if e.Name == "response.completed" {
			resp, _ := e.Data["response"].(map[string]any)
			got, _ = resp["usage"].(map[string]any)
		}
	}
	if got == nil {
		t.Fatal("response.completed 缺少 usage —— 客户端拿不到用量")
	}
	if got["input_tokens"] != float64(1234) {
		t.Errorf("input_tokens = %v，期望上游真实值 1234", got["input_tokens"])
	}
	if got["output_tokens"] != float64(567) {
		t.Errorf("output_tokens = %v，期望上游真实值 567", got["output_tokens"])
	}
}

// TestUsageOmittedWhenAbsent 守：上游没给 usage 时不输出该字段。
//
// 🔴 输出 0 会被当成"确实消耗 0 token"，那是伪造数据。
func TestUsageOmittedWhenAbsent(t *testing.T) {
	rec := drive(t, content("x"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name != "response.completed" {
			continue
		}
		resp, _ := e.Data["response"].(map[string]any)
		if _, has := resp["usage"]; has {
			t.Error("上游没给 usage 却输出了该字段 —— 0 是伪造数据")
		}
	}
}

// TestUsageCacheAndReasoningDetails 守缓存/思考明细被透传。
func TestUsageCacheAndReasoningDetails(t *testing.T) {
	rec := drive(t, content("x"),
		provider.Event{Type: provider.EventUsage, Usage: &provider.Usage{
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			ReasoningTokens: 30, PromptCacheHitTokens: 80,
		}}, done("stop"))

	for _, e := range rec.events(t) {
		if e.Name != "response.completed" {
			continue
		}
		resp, _ := e.Data["response"].(map[string]any)
		u, _ := resp["usage"].(map[string]any)
		in, _ := u["input_tokens_details"].(map[string]any)
		if in["cached_tokens"] != float64(80) {
			t.Errorf("cached_tokens = %v，期望 80", in["cached_tokens"])
		}
		out, _ := u["output_tokens_details"].(map[string]any)
		if out["reasoning_tokens"] != float64(30) {
			t.Errorf("reasoning_tokens = %v，期望 30", out["reasoning_tokens"])
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 本项目硬约束 4：流中途失败绝不发 response.completed
// ─────────────────────────────────────────────────────────────

// TestStreamNeverCompletesOnError 守：失败收尾只发 response.failed。
//
// 🔴 response.completed 在 Responses 协议里等同「正常结束」。
//
//	半截回答配上它，客户端会把它当完整回答继续跑
//	（agent 拿着被截断的 function_call arguments 去执行）。
func TestStreamNeverCompletesOnError(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	if err := sw.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sw.Handle(content("半截")); err != nil {
		t.Fatal(err)
	}
	if err := sw.Fail(errStreamBroken); err != nil {
		t.Fatalf("Fail 失败: %v", err)
	}

	got := rec.names(t)
	for _, n := range got {
		if n == "response.completed" {
			t.Fatalf("失败收尾发出了 response.completed —— 客户端会把半截回答"+
				"当成完整回答继续跑。实际序列: %v", got)
		}
		if n == "response.incomplete" {
			t.Fatalf("失败收尾发出了 response.incomplete —— 语义应是 failed。"+
				"实际序列: %v", got)
		}
	}
	if !rec.has(t, "response.failed") {
		t.Fatalf("失败收尾没有 response.failed —— 客户端无法得知失败。序列: %v", got)
	}
}

// TestFailedEventCarriesError 守：response.failed 必须携带 error 对象。
func TestFailedEventCarriesError(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Fail(errStreamBroken)

	for _, e := range rec.events(t) {
		if e.Name != "response.failed" {
			continue
		}
		resp, _ := e.Data["response"].(map[string]any)
		if resp["status"] != StatusFailed {
			t.Errorf("response.status = %v，期望 failed", resp["status"])
		}
		errObj, _ := resp["error"].(map[string]any)
		if errObj == nil {
			t.Fatal("response.failed 缺少 error 对象 —— 客户端不知道失败原因")
		}
		if errObj["code"] == nil || errObj["code"] == "" {
			t.Error("error.code 为空")
		}
		if errObj["message"] == nil {
			t.Error("error.message 缺失")
		}
	}
}

// TestErrorCodeClassification 守错误码映射（必须落在规范枚举内）。
func TestErrorCodeClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"普通错误", errStreamBroken, "server_error"},
		{"鉴权", authErr{}, "invalid_api_key"},
		{"限流", rateErr{}, "rate_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := errorCode(tc.err)
			if code != tc.want {
				t.Errorf("errorCode = %q，期望 %q", code, tc.want)
			}
		})
	}
	// nil 错误也不能 panic
	if code, _ := errorCode(nil); code == "" {
		t.Error("nil 错误的 code 不应为空")
	}
}

// ─────────────────────────────────────────────────────────────
// 输出项顺序
// ─────────────────────────────────────────────────────────────

// TestReasoningItemBeforeMessage 守：思考项 output_index 0、消息项 1。
func TestReasoningItemBeforeMessage(t *testing.T) {
	rec := drive(t, reasoning("想"), content("答"), done("stop"))

	var idx []int
	for _, e := range rec.events(t) {
		if e.Name == "response.output_item.added" {
			idx = append(idx, int(e.Data["output_index"].(float64)))
		}
	}
	if len(idx) != 2 || idx[0] != IndexReasoning || idx[1] != IndexMessage {
		t.Errorf("output_index = %v，期望 [%d %d]（思考必须先于正文）",
			idx, IndexReasoning, IndexMessage)
	}
}

// TestReasoningSummaryDelta 守思考增量的字段名。
func TestReasoningSummaryDelta(t *testing.T) {
	rec := drive(t, reasoning("思考中"), content("答"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name != "response.reasoning_summary_text.delta" {
			continue
		}
		if e.Data["delta"] != "思考中" {
			t.Errorf("reasoning delta = %v", e.Data["delta"])
		}
		if e.Data["output_index"] != float64(IndexReasoning) {
			t.Errorf("output_index = %v", e.Data["output_index"])
		}
	}
}

// TestOutOfOrderReasoningDropped 守：消息项已开后的思考增量被丢弃。
//
// 🔴 照发会产生非法的 output_index 序列（要求递增）。
func TestOutOfOrderReasoningDropped(t *testing.T) {
	rec := drive(t, content("先正文"), reasoning("迟到的思考"), done("stop"))

	var idx []int
	for _, e := range rec.events(t) {
		if e.Name == "response.output_item.added" {
			idx = append(idx, int(e.Data["output_index"].(float64)))
		}
	}
	if len(idx) != 1 || idx[0] != IndexMessage {
		t.Errorf("output_index = %v，期望只有消息项 [%d]（乱序思考必须丢弃）",
			idx, IndexMessage)
	}
	// 思考内容不能混进正文
	for _, e := range rec.events(t) {
		if e.Name == "response.output_text.delta" {
			if s, _ := e.Data["delta"].(string); strings.Contains(s, "迟到的思考") {
				t.Error("乱序的思考内容被混进了正文")
			}
		}
	}
}

// TestOnlyReasoningNoMessage 守：只有思考时不开消息项。
func TestOnlyReasoningNoMessage(t *testing.T) {
	rec := drive(t, reasoning("只有思考"), done("stop"))
	var idx []int
	for _, e := range rec.events(t) {
		if e.Name == "response.output_item.added" {
			idx = append(idx, int(e.Data["output_index"].(float64)))
		}
	}
	if len(idx) != 1 || idx[0] != IndexReasoning {
		t.Errorf("output_index = %v，期望只有思考项 [%d]", idx, IndexReasoning)
	}
}

// TestEmptyStreamStillCompletes 守：无内容时也要发 created + completed。
func TestEmptyStreamStillCompletes(t *testing.T) {
	rec := drive(t, done("stop"))
	got := rec.names(t)
	want := []string{"response.created", "response.in_progress", "response.completed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("空流序列 = %v\n期望 = %v", got, want)
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 最隐蔽的缺陷：output_item.done 必须带**完整**内容
// ─────────────────────────────────────────────────────────────

// TestMessageDoneCarriesFullContent 守：message 的 output_item.done
// 必须带完整正文，不能是空数组。
//
// 🔴 这是"事件名全对、内容却丢了"的缺陷（2026-10-09 从 DSH 的 app.asar
//
//	提取真实契约时发现）：
//
//	DSH 先按 response.output_text.delta 累加文本，然后在
//	response.output_item.done 时**用 item.content[] 覆盖**累加结果
//	（pi-ai/dist/api/openai-responses-shared.js:595-604）：
//
//	  slot.block.text =
//	      item.content?.map(c => c.type === "output_text" ? c.text : c.refusal).join("") || "";
//
//	⇒ 若 done 里的 content[] 是空数组，累加好的正文被**抹成空串** ——
//	  用户看到一条空白回答，而所有事件名都"正确"。
//
//	⚠️ 这类缺陷只看"事件名齐全"的测试抓不到，必须断言 done 事件的**内容**。
func TestMessageDoneCarriesFullContent(t *testing.T) {
	rec := drive(t, content("你好"), content("世界"), done("stop"))

	var doneText string
	var found bool
	for _, e := range rec.events(t) {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item == nil || item["type"] != ItemMessage {
			continue
		}
		found = true
		contentParts, _ := item["content"].([]any)
		if len(contentParts) == 0 {
			t.Fatal("message 的 output_item.done 里 content[] 为空 —— " +
				"DSH 会用这个空数组覆盖已累加的正文，用户看到空白回答")
		}
		part, _ := contentParts[0].(map[string]any)
		if part["type"] != ItemOutputText {
			t.Errorf("content[0].type = %v，期望 output_text", part["type"])
		}
		doneText, _ = part["text"].(string)
	}
	if !found {
		t.Fatal("没有找到 message 的 output_item.done")
	}
	if doneText != "你好世界" {
		t.Errorf("output_item.done 里的正文 = %q，期望完整拼接的 你好世界 —— "+
			"空数组或残缺都会让客户端显示错误内容", doneText)
	}
}

// TestOutputTextDoneCarriesFullText 守：response.output_text.done 带完整文本。
func TestOutputTextDoneCarriesFullText(t *testing.T) {
	rec := drive(t, content("甲"), content("乙"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name != "response.output_text.done" {
			continue
		}
		if e.Data["text"] != "甲乙" {
			t.Errorf("output_text.done 的 text = %v，期望 甲乙", e.Data["text"])
		}
	}
}

// TestContentPartDoneCarriesFullText 守：response.content_part.done 带完整文本。
func TestContentPartDoneCarriesFullText(t *testing.T) {
	rec := drive(t, content("甲"), content("乙"), done("stop"))
	for _, e := range rec.events(t) {
		if e.Name != "response.content_part.done" {
			continue
		}
		part, _ := e.Data["part"].(map[string]any)
		if part == nil || part["text"] != "甲乙" {
			t.Errorf("content_part.done 的 part.text = %v，期望 甲乙", part)
		}
	}
}

// TestReasoningDoneCarriesFullSummary 守：思考项的 summary[] 必须带完整文本。
//
// 🔴 与 message 的 content[] 同一个陷阱（DSH 会用 summary[] 覆盖累加结果）。
func TestReasoningDoneCarriesFullSummary(t *testing.T) {
	rec := drive(t, reasoning("想一"), reasoning("想二"), content("答"), done("stop"))

	var summaryText string
	var found bool
	for _, e := range rec.events(t) {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item == nil || item["type"] != ItemReasoning {
			continue
		}
		found = true
		summary, _ := item["summary"].([]any)
		if len(summary) == 0 {
			t.Fatal("reasoning 的 output_item.done 里 summary[] 为空 —— " +
				"DSH 会用空数组覆盖已累加的思考内容")
		}
		part, _ := summary[0].(map[string]any)
		summaryText, _ = part["text"].(string)
	}
	if !found {
		t.Fatal("没有找到 reasoning 的 output_item.done")
	}
	if summaryText != "想一想二" {
		t.Errorf("summary 文本 = %q，期望 想一想二", summaryText)
	}
}

// TestCompletedCarriesFullOutput 守：response.completed 的 output[] 非空且完整。
//
// 🔴 DSH 会在 completed 时重新扫描 response.output[] 回填字段
//
//	（shared.js:417-432 backfillReasoningSignatures）。给空数组虽然不崩，
//	但客户端拿不到完整最终输出，store:false 多轮重放会缺上下文。
func TestCompletedCarriesFullOutput(t *testing.T) {
	rec := drive(t, reasoning("想"), content("答"),
		toolCall(0, "call_1", "f", `{"a":1}`), done("tool_calls"))

	for _, e := range rec.events(t) {
		if e.Name != "response.completed" {
			continue
		}
		resp, _ := e.Data["response"].(map[string]any)
		output, _ := resp["output"].([]any)
		if len(output) == 0 {
			t.Fatal("response.completed 的 output[] 为空 —— " +
				"客户端拿不到完整最终输出")
		}
		kinds := map[string]bool{}
		for _, o := range output {
			om, _ := o.(map[string]any)
			kinds[asString(om["type"])] = true
		}
		for _, k := range []string{ItemReasoning, ItemMessage, ItemFunctionCall} {
			if !kinds[k] {
				t.Errorf("response.completed 的 output[] 缺 %s 项（实际 %v）", k, kinds)
			}
		}
	}
}

// TestFunctionCallItemsHaveUniqueIDs 守：多个 function_call 的 item id 必须唯一。
//
// 🔴 DSH 用 `${item.call_id}|${item.id}` 作为块的标识
//
//	（shared.js:368）。若多个调用的 id 相同，它们会被认成同一个调用，
//	参数互相覆盖。
func TestFunctionCallItemsHaveUniqueIDs(t *testing.T) {
	rec := drive(t,
		toolCall(0, "call_1", "f1", `{"a":1}`),
		toolCall(1, "call_2", "f2", `{"b":2}`),
		done("tool_calls"),
	)

	seenID := map[string]bool{}
	seenCall := map[string]bool{}
	n := 0
	for _, e := range rec.events(t) {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item == nil || item["type"] != ItemFunctionCall {
			continue
		}
		n++
		id := asString(item["id"])
		callID := asString(item["call_id"])
		if seenID[id] {
			t.Errorf("function_call item id %q 重复 —— 多个调用会被认成同一个", id)
		}
		if seenCall[callID] {
			t.Errorf("call_id %q 重复", callID)
		}
		seenID[id] = true
		seenCall[callID] = true
		if !strings.HasPrefix(id, "fc_") {
			t.Errorf("item id = %q，期望 fc_ 前缀（DSH 契约）", id)
		}
	}
	if n != 2 {
		t.Fatalf("function_call item 数 = %d，期望 2", n)
	}
}

// TestFunctionCallBothIDsPresent 守：function_call item 同时有 call_id 与 id。
//
// 🔴 DSH 两者都要（用 `${call_id}|${id}` 作块标识）。
func TestFunctionCallBothIDsPresent(t *testing.T) {
	rec := drive(t, toolCall(0, "call_x", "f", `{}`), done("tool_calls"))
	for _, e := range rec.events(t) {
		if e.Name != "response.output_item.done" {
			continue
		}
		item, _ := e.Data["item"].(map[string]any)
		if item == nil || item["type"] != ItemFunctionCall {
			continue
		}
		if asString(item["id"]) == "" {
			t.Error("function_call item 缺 id —— DSH 需要 call_id 与 id 两者")
		}
		if asString(item["call_id"]) == "" {
			t.Error("function_call item 缺 call_id")
		}
	}
}

// TestCompletedTerminalEventPresent 守：流必须以终结事件结束。
//
// 🔴 DSH 明确要求（shared.js:656-658）：
//
//	if (!sawTerminalResponseEvent) {
//	    throw new Error("OpenAI Responses stream ended before a terminal response event");
//	}
//
//	⇒ 只有 [DONE] 或空流会让 DSH 直接抛错。
func TestCompletedTerminalEventPresent(t *testing.T) {
	rec := drive(t, content("x"), done("stop"))
	names := rec.names(t)
	last := names[len(names)-1]
	if last != "response.completed" && last != "response.incomplete" &&
		last != "response.failed" {
		t.Errorf("最后一个事件 = %s，期望是终结事件（completed/incomplete/failed）—— "+
			"DSH 会因「流在终结事件前结束」直接抛错", last)
	}
}

// ─────────────────────────────────────────────────────────────
// 幂等
// ─────────────────────────────────────────────────────────────

// TestFinishIdempotent 守：重复 Finish 不重复发 completed。
//
// 🔴 真实场景：流错误后调用方仍可能走到收尾分支。
//
//	不幂等会让客户端收到两个 response.completed（协议违规）。
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

// TestFailAfterFinishIsNoop 守：已正常收尾后 Fail 不发 failed。
func TestFailAfterFinishIsNoop(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Handle(content("x"))
	_ = sw.Finish()
	n1 := len(rec.frames)
	_ = sw.Fail(errStreamBroken)
	if len(rec.frames) != n1 {
		t.Error("正常收尾后 Fail 仍写了帧 —— 会让客户端以为请求失败")
	}
}

// TestStartIdempotent 守：重复 Start 不重复发 created。
func TestStartIdempotent(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	n1 := len(rec.frames)
	_ = sw.Start()
	if len(rec.frames) != n1 {
		t.Error("重复 Start 又写了帧 —— 必须幂等")
	}
}

// TestFrameEndsWithBlankLine 守 SSE 帧格式。
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

// TestUsageReturnsNilWhenAbsent 守 Usage() 的 nil 语义。
func TestUsageReturnsNilWhenAbsent(t *testing.T) {
	rec := &sseRecorder{}
	sw := NewStreamWriter(rec.write, "chatcmpl-1", "m")
	_ = sw.Start()
	_ = sw.Handle(content("x"))
	_ = sw.Finish()
	if sw.Usage() != nil {
		t.Error("上游没给 usage 时 Usage() 应为 nil")
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
// 非流式聚合
// ─────────────────────────────────────────────────────────────

// TestAggregateBasic 守非流式聚合的基本形状。
func TestAggregateBasic(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(reasoning("想"))
	agg.Add(content("答"))
	agg.Add(usage(10, 20))
	agg.Add(done("stop"))

	got := agg.Result()
	if got.Object != "response" {
		t.Errorf("object = %q，期望 response", got.Object)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q，期望 completed", got.Status)
	}
	if !strings.HasPrefix(got.ID, "resp_") {
		t.Errorf("id = %q，期望 resp_ 前缀", got.ID)
	}
	if got.Model != "m" {
		t.Errorf("model = %q，期望客户端模型名", got.Model)
	}
	if len(got.Output) != 2 {
		t.Fatalf("output 项数 = %d，期望 2（reasoning + message）", len(got.Output))
	}
	if got.Output[0]["type"] != ItemReasoning {
		t.Errorf("output[0].type = %v，期望 reasoning", got.Output[0]["type"])
	}
	if got.Output[1]["type"] != ItemMessage {
		t.Errorf("output[1].type = %v，期望 message", got.Output[1]["type"])
	}
	if got.Usage == nil || got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 20 {
		t.Errorf("usage = %+v，期望 10/20", got.Usage)
	}
}

// TestAggregateMessageContentShape 守 message item 的 content 形状。
func TestAggregateMessageContentShape(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(content("正文"))
	agg.Add(done("stop"))

	got := agg.Result()
	msg := got.Output[0]
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 分片数 = %d，期望 1", len(content))
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != ItemOutputText {
		t.Errorf("content[0].type = %v，期望 output_text", part["type"])
	}
	if part["text"] != "正文" {
		t.Errorf("content[0].text = %v", part["text"])
	}
	// annotations 字段必须存在（规范要求，客户端可能直接读）
	if _, has := part["annotations"]; !has {
		t.Error("output_text 分片缺少 annotations 字段")
	}
}

// TestAggregateReasoningSummaryShape 守 reasoning item 的 summary 形状。
func TestAggregateReasoningSummaryShape(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(reasoning("思考内容"))
	agg.Add(done("stop"))

	got := agg.Result()
	summary, _ := got.Output[0]["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary 项数 = %d，期望 1", len(summary))
	}
	part, _ := summary[0].(map[string]any)
	if part["type"] != "summary_text" {
		t.Errorf("summary[0].type = %v，期望 summary_text", part["type"])
	}
	if part["text"] != "思考内容" {
		t.Errorf("summary[0].text = %v", part["text"])
	}
}

// TestAggregateToolCall 守非流式工具调用。
func TestAggregateToolCall(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(toolCall(0, "call_1", "get_", `{"a":`))
	agg.Add(toolCall(0, "", "weather", "1}"))
	agg.Add(done("tool_calls"))

	got := agg.Result()
	var fc map[string]any
	for _, item := range got.Output {
		if item["type"] == ItemFunctionCall {
			fc = item
		}
	}
	if fc == nil {
		t.Fatal("output 里没有 function_call 项")
	}
	if fc["call_id"] != "call_1" {
		t.Errorf("call_id = %v，期望 call_1", fc["call_id"])
	}
	if fc["name"] != "get_weather" {
		t.Errorf("name = %v，期望分片拼接成 get_weather", fc["name"])
	}
	// 🔴 Responses 的 arguments 是 JSON **字符串**（与 anthropic 的对象不同）
	args, ok := fc["arguments"].(string)
	if !ok {
		t.Fatalf("arguments 类型 = %T，期望字符串（Responses 规范）", fc["arguments"])
	}
	if args != `{"a":1}` {
		t.Errorf("arguments = %q，期望分片拼接成 {\"a\":1}", args)
	}
}

// TestAggregateIncompleteOnLength 守非流式的 status 映射。
func TestAggregateIncompleteOnLength(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(content("半截"))
	agg.Add(done("length"))

	got := agg.Result()
	if got.Status != StatusIncomplete {
		t.Errorf("status = %q，期望 incomplete", got.Status)
	}
	if got.IncompleteDetails == nil || got.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("incomplete_details = %+v，期望 reason=max_output_tokens", got.IncompleteDetails)
	}
}

// TestAggregateUsageNilWhenAbsent 守：上游没给 usage 时为 nil。
func TestAggregateUsageNilWhenAbsent(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(content("x"))
	agg.Add(done("stop"))
	if agg.Usage() != nil {
		t.Error("上游没给 usage 时 Usage() 应为 nil")
	}
	if agg.Result().Usage != nil {
		t.Error("上游没给 usage 时 Result().Usage 应为 nil（不输出该字段）")
	}
}

// TestAggregateEmptyContent 守：完全无内容时 output 为空数组而非 null。
func TestAggregateEmptyContent(t *testing.T) {
	agg := NewAggregator("chatcmpl-1", "m")
	agg.Add(done("stop"))
	got := agg.Result()
	if got.Output == nil {
		t.Error("output 为 nil —— 序列化会变成 null，客户端期望数组")
	}
	if len(got.Output) != 0 {
		t.Errorf("output 项数 = %d，期望 0", len(got.Output))
	}
}

// TestParseArgs 守 ParseArgs 的兜底。
func TestParseArgs(t *testing.T) {
	if got := ParseArgs(""); got != "{}" {
		t.Errorf("空参数 → %q，期望 {}", got)
	}
	if got := ParseArgs(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("原样透传失败: %q", got)
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
