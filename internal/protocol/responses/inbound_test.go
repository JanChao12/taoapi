package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件是入站翻译的 1:1 护栏测试。
//
// 组织原则：**每个翻译点一个测试**，断言"翻译后的形状"而非"没有报错"。
// 三个参考实现级缺陷都属于"不报错但语义错"，只看 error == nil 的测试抓不到。

func mustConvert(t *testing.T, body string) *Inbound {
	t.Helper()
	in, err := Convert([]byte(body))
	if err != nil {
		t.Fatalf("Convert 失败: %v\n请求体: %s", err, body)
	}
	return in
}

func decodeRaw(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("翻译结果不是合法 JSON: %v\n内容: %s", err, b)
	}
	return m
}

// msgs 取出翻译后的 messages。
func msgs(t *testing.T, in *Inbound) []any {
	t.Helper()
	got := decodeRaw(t, in.RawBody)
	m, _ := got["messages"].([]any)
	return m
}

// ─────────────────────────────────────────────────────────────
// 翻译点 1：instructions → 首条 system 消息
// ─────────────────────────────────────────────────────────────

// TestInstructionsBecomesSystemMessage 守：顶层 instructions
// 必须变成 messages 里的第一条 system 消息。
func TestInstructionsBecomesSystemMessage(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","instructions":"你是助手",
		"input":"你好"
	}`)
	m := msgs(t, in)
	if len(m) != 2 {
		t.Fatalf("messages 数 = %d，期望 2（system + user）", len(m))
	}
	first, _ := m[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("首条 role = %v，期望 system", first["role"])
	}
	if first["content"] != "你是助手" {
		t.Errorf("system 内容 = %v", first["content"])
	}
}

// TestNoInstructionsNoSystemMessage 守：没有 instructions 就不造 system 消息。
func TestNoInstructionsNoSystemMessage(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"你好"}`)
	m := msgs(t, in)
	if len(m) != 1 {
		t.Fatalf("messages 数 = %d，期望 1", len(m))
	}
}

// ─────────────────────────────────────────────────────────────
// 翻译点 2：input 的两种形状
// ─────────────────────────────────────────────────────────────

// TestInputStringShortcut 守：input 可以是纯字符串。
func TestInputStringShortcut(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"纯字符串"}`)
	m := msgs(t, in)
	if len(m) != 1 {
		t.Fatalf("messages 数 = %d", len(m))
	}
	got, _ := m[0].(map[string]any)
	if got["role"] != "user" {
		t.Errorf("role = %v，期望 user", got["role"])
	}
	if got["content"] != "纯字符串" {
		t.Errorf("content = %v", got["content"])
	}
}

// TestInputItemArray 守：input 是 item 数组时正确翻译。
func TestInputItemArray(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"你好"}
			]}
		]
	}`)
	m := msgs(t, in)
	if len(m) != 1 {
		t.Fatalf("messages 数 = %d", len(m))
	}
	got, _ := m[0].(map[string]any)
	if got["content"] != "你好" {
		t.Errorf("content = %v，期望 input_text 的文本", got["content"])
	}
}

// TestInputMultipleTextPartsJoined 守：多个 input_text 分片被拼接。
func TestInputMultipleTextPartsJoined(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"甲"},
			{"type":"input_text","text":"乙"}
		]}]
	}`)
	m := msgs(t, in)
	got, _ := m[0].(map[string]any)
	if got["content"] != "甲\n乙" {
		t.Errorf("content = %q，期望两个分片被拼接", got["content"])
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 翻译点 3：function_call 是平级 item，必须归并进 assistant 消息
// ─────────────────────────────────────────────────────────────

// TestFunctionCallMergedIntoAssistantMessage 守 Responses 的扁平 item
// 结构被正确转换成 Chat 的嵌套结构。
//
// 🔴 这是最容易写错的地方：
//
//	Responses 的 function_call 与 message 是**平级**的独立 item，
//	而 Chat 要求工具调用**挂在 assistant 消息的 tool_calls 数组里**。
//	照搬成独立消息会让上游报错（没有 tool_calls 的 assistant）。
func TestFunctionCallMergedIntoAssistantMessage(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"天气"}
			]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"晴 25 度"}
		]
	}`)
	m := msgs(t, in)

	// 期望：user / assistant(带 tool_calls) / tool
	if len(m) != 3 {
		t.Fatalf("messages 数 = %d，期望 3（user / assistant / tool）\n实际: %+v", len(m), m)
	}

	asst, _ := m[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("第 2 条 role = %v，期望 assistant", asst["role"])
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数 = %d，期望 1 —— function_call 必须归并进 assistant 消息",
			len(tcs))
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool_call id = %v，期望 call_1", tc["id"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("function.arguments = %v，期望原样 JSON 字符串", fn["arguments"])
	}

	tool, _ := m[2].(map[string]any)
	if tool["role"] != "tool" {
		t.Fatalf("第 3 条 role = %v，期望 tool", tool["role"])
	}
	if tool["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v", tool["tool_call_id"])
	}
	if tool["content"] != "晴 25 度" {
		t.Errorf("tool content = %v", tool["content"])
	}
}

// TestAssistantBeforeToolOutput 守顺序：assistant 必须排在 tool 结果之前。
//
// 🔴 顺序错了上游会报错（Chat 规范要求 tool 结果紧跟其调用）。
func TestAssistantBeforeToolOutput(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"结果"}
		]
	}`)
	m := msgs(t, in)
	if len(m) != 2 {
		t.Fatalf("messages 数 = %d，期望 2", len(m))
	}
	asst, _ := m[0].(map[string]any)
	tool, _ := m[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("第 1 条 role = %v，期望 assistant（必须排在 tool 结果之前）", asst["role"])
	}
	if tool["role"] != "tool" {
		t.Errorf("第 2 条 role = %v，期望 tool", tool["role"])
	}
}

// TestMultipleFunctionCallsMergedIntoOneMessage 守：连续多个 function_call
// 归并到**同一条** assistant 消息（贴近真实对话结构）。
func TestMultipleFunctionCallsMergedIntoOneMessage(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"c1","name":"f1","arguments":"{}"},
			{"type":"function_call","call_id":"c2","name":"f2","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"a"},
			{"type":"function_call_output","call_id":"c2","output":"b"}
		]
	}`)
	m := msgs(t, in)
	if len(m) != 3 {
		t.Fatalf("messages 数 = %d，期望 3（一条 assistant + 两条 tool）\n实际: %+v",
			len(m), m)
	}
	asst, _ := m[0].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Errorf("归并后的 tool_calls 数 = %d，期望 2（连续调用应在同一条 assistant 消息里）",
			len(tcs))
	}
}

// TestFunctionCallMissingCallIDFails 守：缺 call_id 必须报错。
func TestFunctionCallMissingCallIDFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m",
		"input":[{"type":"function_call","name":"f","arguments":"{}"}]
	}`))
	if err == nil {
		t.Fatal("function_call 缺 call_id 却转换成功 —— " +
			"凭空造 id 会让 function_call_output 永远关联不上")
	}
	if !strings.Contains(err.Error(), "call_id") {
		t.Errorf("错误信息未点名 call_id: %v", err)
	}
}

// TestFunctionCallOutputMissingCallIDFails 守：缺 call_id 必须报错。
func TestFunctionCallOutputMissingCallIDFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m",
		"input":[{"type":"function_call_output","output":"结果"}]
	}`))
	if err == nil {
		t.Fatal("function_call_output 缺 call_id 却转换成功")
	}
	if !strings.Contains(err.Error(), "call_id") {
		t.Errorf("错误信息未点名 call_id: %v", err)
	}
}

// TestReasoningItemDropped 守：历史 reasoning item 被丢弃（有意为之）。
//
// ⚠️ 这不违反"不静默丢弃"：思考是过程性内容，上游不接受历史 thinking，
//
//	且它对后续推理没有信息增量。有意的简化要能被测试看见。
func TestReasoningItemDropped(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"我想想"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}
		]
	}`)
	m := msgs(t, in)
	if len(m) != 1 {
		t.Fatalf("messages 数 = %d，期望 1（reasoning item 不产生消息）", len(m))
	}
	got, _ := m[0].(map[string]any)
	if s, _ := got["content"].(string); strings.Contains(s, "我想想") {
		t.Error("历史 reasoning 内容不应进入请求体")
	}
}

// ─────────────────────────────────────────────────────────────
// 翻译点 4：tools 是扁平形状
// ─────────────────────────────────────────────────────────────

// TestToolsFlatShape 守：Responses 的扁平工具定义 → Chat 嵌套形状。
func TestToolsFlatShape(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"x",
		"tools":[{"type":"function","name":"get_weather","description":"查天气",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)
	if !in.HasTools {
		t.Error("HasTools = false，期望 true")
	}
	got := decodeRaw(t, in.RawBody)
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数 = %d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v", tool["type"])
	}
	// 🔴 必须转成 Chat 的嵌套形状：function:{name,...}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0].function 缺失 —— Responses 的扁平形状必须转成 Chat 嵌套形状: %+v", tool)
	}
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v", fn["name"])
	}
	if fn["description"] != "查天气" {
		t.Errorf("function.description = %v", fn["description"])
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters 未从 parameters 翻译过来: %v", params)
	}
}

// TestToolsAlreadyNestedTolerated 守：已经嵌套的形状也能接受（部分客户端混用）。
func TestToolsAlreadyNestedTolerated(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"x",
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]
	}`)
	got := decodeRaw(t, in.RawBody)
	tools, _ := got["tools"].([]any)
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" {
		t.Errorf("嵌套形状未被识别: %+v", tools[0])
	}
}

// TestToolsMissingParametersGetsEmptyObject 守：parameters 缺失时给空 schema。
//
// 🔴 上游要求 tools[].function.parameters 存在，null 会 400。
func TestToolsMissingParametersGetsEmptyObject(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"x",
		"tools":[{"type":"function","name":"noop"}]
	}`)
	got := decodeRaw(t, in.RawBody)
	tools, _ := got["tools"].([]any)
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("parameters = %v，期望空 object schema", fn["parameters"])
	}
}

// TestBuiltinToolsFail 守：内置服务端工具必须**明确报错**。
//
// 🔴 委托方第 3 条边界。静默跳过的后果：客户端以为工具可用，
//
//	然后一直等一个永远不会来的 function_call。
func TestBuiltinToolsFail(t *testing.T) {
	for _, typ := range []string{"web_search", "file_search", "computer_use_preview", "code_interpreter", "image_generation", "mcp"} {
		t.Run(typ, func(t *testing.T) {
			_, err := Convert([]byte(`{
				"model":"m","input":"x",
				"tools":[{"type":"` + typ + `"}]
			}`))
			if err == nil {
				t.Fatalf("内置工具 %s 却转换成功 —— 本服务无法代为执行，"+
					"必须明确报错而不是静默丢弃", typ)
			}
			if !strings.Contains(err.Error(), typ) {
				t.Errorf("错误信息未点名工具 type: %v", err)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────
// 翻译点 5：reasoning.effort
// ─────────────────────────────────────────────────────────────

// TestReasoningEffortMapping 守 reasoning.effort 的映射。
func TestReasoningEffortMapping(t *testing.T) {
	cases := []struct{ in, want string }{
		{"minimal", "minimal"},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		{"ultra", "ultra"},
		{"off", "off"},
		{"none", "off"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			in := mustConvert(t, `{"model":"m","input":"x",
				"reasoning":{"effort":"`+tc.in+`"}}`)
			if in.ReasoningEffort != tc.want {
				t.Errorf("reasoning.effort=%q → %q，期望 %q",
					tc.in, in.ReasoningEffort, tc.want)
			}
		})
	}
}

// TestReasoningEffortNotInRawBody 守：档位不写进 RawBody。
//
// 🔴 写两处必然漂移：provider.buildChatBody 会用
// ChatRequest.ReasoningEffort 覆盖该字段。
func TestReasoningEffortNotInRawBody(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","reasoning":{"effort":"high"}}`)
	got := decodeRaw(t, in.RawBody)
	if _, has := got["reasoning_effort"]; has {
		t.Error("reasoning_effort 不应写进 RawBody —— provider 会覆盖它")
	}
	if _, has := got["reasoning"]; has {
		t.Error("reasoning 不应原样透传给上游（上游不认这个字段）")
	}
}

// TestReasoningUnknownEffortFails 守：未知档位显式报错。
func TestReasoningUnknownEffortFails(t *testing.T) {
	_, err := Convert([]byte(`{"model":"m","input":"x","reasoning":{"effort":"veryhigh"}}`))
	if err == nil {
		t.Fatal("未知档位 veryhigh 却转换成功 —— 应显式报错而不是发上去换 400")
	}
	if !strings.Contains(err.Error(), "veryhigh") {
		t.Errorf("错误信息未点名非法档位: %v", err)
	}
}

// TestReasoningEmptyEffortMeansDefault 守：reasoning 存在但无 effort = 未指定。
func TestReasoningEmptyEffortMeansDefault(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","reasoning":{}}`)
	if in.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort = %q，期望空串（交由 provider 注入默认档）",
			in.ReasoningEffort)
	}
}

// ─────────────────────────────────────────────────────────────
// 明确报错的边界（委托方第 3 条）
// ─────────────────────────────────────────────────────────────

// TestPreviousResponseIDFails 守：previous_response_id 必须报错。
//
// 🔴 静默忽略是最坏的选择：客户端以为服务端记得上文，于是只发最新一句，
//
//	模型却看到一条孤立的消息 —— 用户得到"答非所问"的结果，且不知道为什么。
func TestPreviousResponseIDFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","input":"继续","previous_response_id":"resp_abc"
	}`))
	if err == nil {
		t.Fatal("previous_response_id 却转换成功 —— 静默忽略会让模型看到孤立消息")
	}
	if !strings.Contains(err.Error(), "previous_response_id") {
		t.Errorf("错误信息未点名 previous_response_id: %v", err)
	}
}

// TestStoreTrueFails 守：store=true 必须报错。
func TestStoreTrueFails(t *testing.T) {
	_, err := Convert([]byte(`{"model":"m","input":"x","store":true}`))
	if err == nil {
		t.Fatal("store=true 却转换成功 —— 本服务不保存任何会话数据")
	}
	if !strings.Contains(err.Error(), "store") {
		t.Errorf("错误信息未点名 store: %v", err)
	}
}

// TestStoreFalseAccepted 守：store=false 是正常用法，不该被拒。
//
// 🔴 反向对照：若把"存在 store 字段"当成错误，客户端默认发的 store:false
//
//	会导致所有请求失败。
func TestStoreFalseAccepted(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","store":false}`)
	if in.Model != "m" {
		t.Errorf("Model = %q", in.Model)
	}
}

// TestTruncationAutoFails 守：truncation=auto 必须报错（本服务不截断）。
func TestTruncationAutoFails(t *testing.T) {
	_, err := Convert([]byte(`{"model":"m","input":"x","truncation":"auto"}`))
	if err == nil {
		t.Fatal("truncation=auto 却转换成功 —— 本服务不做服务端截断")
	}
}

// TestTruncationDisabledAccepted 守：truncation=disabled 是默认值，不该被拒。
func TestTruncationDisabledAccepted(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","truncation":"disabled"}`)
	if in.Model != "m" {
		t.Errorf("Model = %q", in.Model)
	}
}

// TestIncludeAcceptedAndIgnored 守：include **必须被接受**，不能报错。
//
// 🔴 这条测试原本断言的是"include 报错"，那是**错的**（2026-10-09 修正）。
//
//	DSH 的真实契约（从 app.asar 提取，pi-ai/dist/api/openai-responses.js:230-270）：
//
//	  if (model.reasoning) {
//	      if (options?.reasoningEffort || options?.reasoningSummary) {
//	          params.reasoning = { effort, summary: "auto" };
//	          params.include = ["reasoning.encrypted_content"];  // ← 总是带上
//	      }
//	  }
//
//	⇒ 只要客户端开了思考，DSH 就**必然**发 include。
//	  若这里报错，DSH 的 Responses 路径**每一个带思考的请求都失败**。
//
//	为什么"忽略"不算违反委托方第 3 条边界：
//	  include 是"额外**可选**附带数据"，不是"我需要这个能力才能工作"。
//	  不产出 encrypted_content 不构成谎言（DSH 只用它做多轮重放兜底）。
func TestIncludeAcceptedAndIgnored(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"x",
		"include":["reasoning.encrypted_content"]
	}`)
	if in.Model != "m" {
		t.Errorf("Model = %q", in.Model)
	}
	// 不该被转发给上游（上游不认这个字段）
	got := decodeRaw(t, in.RawBody)
	if _, has := got["include"]; has {
		t.Error("include 被转发给上游 —— 上游不认这个字段，应只做校验后忽略")
	}
}

// TestIncludeWrongShapeFails 守：include 元素类型不对时要报错。
//
// ⚠️ 接受 include ≠ 什么都不校验。形状错了要明确说，
//
//	否则客户端以为自己的参数被理解了。
func TestIncludeWrongShapeFails(t *testing.T) {
	_, err := Convert([]byte(`{"model":"m","input":"x","include":[123]}`))
	if err == nil {
		t.Fatal("include 元素是数字却转换成功 —— 应报错说明形状不对")
	}
}

// TestReasoningPlusIncludeCombination 守 DSH 真实组合：reasoning + include 同时出现。
//
// 🔴 这是 DSH 实际会发的**完整形态**（见 TestIncludeAcceptedAndIgnored 的引用）。
//
//	单独测 include、单独测 reasoning 都不够 —— 必须测它们一起出现。
func TestReasoningPlusIncludeCombination(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"x","stream":true,"store":false,
		"max_output_tokens":64000,
		"reasoning":{"effort":"high","summary":"auto"},
		"include":["reasoning.encrypted_content"]
	}`)
	if in.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q，期望 high", in.ReasoningEffort)
	}
	got := decodeRaw(t, in.RawBody)
	if got["max_tokens"] != float64(64000) {
		t.Errorf("max_tokens = %v，期望 64000", got["max_tokens"])
	}
}

// TestServerToolItemsFail 守：服务端工具产生的 item 必须报错。
func TestServerToolItemsFail(t *testing.T) {
	for _, typ := range []string{"web_search_call", "file_search_call",
		"computer_call", "computer_call_output", "code_interpreter_call",
		"image_generation_call", "mcp_call", "item_reference"} {
		t.Run(typ, func(t *testing.T) {
			_, err := Convert([]byte(`{
				"model":"m",
				"input":[{"type":"` + typ + `","id":"x"}]
			}`))
			if err == nil {
				t.Fatalf("item 类型 %s 却转换成功 —— 静默跳过会让模型看到凭空缺失的上下文", typ)
			}
			if !strings.Contains(err.Error(), typ) {
				t.Errorf("错误信息未点名 item 类型: %v", err)
			}
		})
	}
}

// TestUnknownItemTypeFails 守：未知 item 类型必须报错。
func TestUnknownItemTypeFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","input":[{"type":"未来新类型","data":"x"}]
	}`))
	if err == nil {
		t.Fatal("未知 item 类型却转换成功 —— 静默跳过是「错误的成功」")
	}
	if !strings.Contains(err.Error(), "未来新类型") {
		t.Errorf("错误信息未点名 item 类型: %v", err)
	}
}

// TestUnknownContentPartFails 守：未知 content 分片类型必须报错。
func TestUnknownContentPartFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m",
		"input":[{"type":"message","role":"user","content":[
			{"type":"未来分片","data":"x"}
		]}]
	}`))
	if err == nil {
		t.Fatal("未知 content 分片却转换成功")
	}
}

// TestToolChoiceNamedFunctionFails 守：指定具体函数必须报错。
//
// 🔴 上游要求 tool_choice 是字符串，传对象直接 400（code=11101）。
//
//	降级为 required 会改变语义，属于委托方禁止的静默改变行为。
func TestToolChoiceNamedFunctionFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","input":"x",
		"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"f"}
	}`))
	if err == nil {
		t.Fatal("tool_choice 指定函数却转换成功 —— 降级为 required 会改变语义")
	}
}

// TestToolChoiceAllowedToolsFails 守：allowed_tools 必须报错。
func TestToolChoiceAllowedToolsFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","input":"x",
		"tool_choice":{"type":"allowed_tools","mode":"auto"}
	}`))
	if err == nil {
		t.Fatal("tool_choice.allowed_tools 却转换成功 —— 上游无等价物")
	}
}

// TestToolChoiceStringMapping 守：字符串形式的 tool_choice 正确映射。
func TestToolChoiceStringMapping(t *testing.T) {
	for _, tc := range []string{"auto", "none", "required"} {
		in := mustConvert(t, `{"model":"m","input":"x","tool_choice":"`+tc+`"}`)
		got := decodeRaw(t, in.RawBody)
		if got["tool_choice"] != tc {
			t.Errorf("tool_choice %q → %v", tc, got["tool_choice"])
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 宽容性
// ─────────────────────────────────────────────────────────────

// TestToleratesUnknownTopLevelFields 守：未知顶层字段必须被忽略。
//
// 🔴 DSH 会注入自己的扩展顶层字段（与 anthropic 路径同源的问题）。
//
//	若做顶层 key 白名单校验，DSH 的所有请求都会被拒。
func TestToleratesUnknownTopLevelFields(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"hi",
		"dsh_session_log":"日志",
		"dsh_plugin_packages":["a"],
		"some_future_field":{"x":1}
	}`)
	if in.Model != "m" {
		t.Errorf("Model = %q", in.Model)
	}
	got := decodeRaw(t, in.RawBody)
	for _, k := range []string{"dsh_session_log", "dsh_plugin_packages", "some_future_field"} {
		if _, has := got[k]; has {
			t.Errorf("未知顶层字段 %q 被转发给上游 —— 应忽略", k)
		}
	}
}

// TestMaxOutputTokensDefault 守：缺 max_output_tokens 时给默认值。
func TestMaxOutputTokensDefault(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x"}`)
	got := decodeRaw(t, in.RawBody)
	if got["max_tokens"] != float64(DefaultMaxOutputTokens) {
		t.Errorf("max_tokens = %v，期望默认 %d", got["max_tokens"], DefaultMaxOutputTokens)
	}
}

// TestMaxOutputTokensPassthrough 守：给了就用给的值。
func TestMaxOutputTokensPassthrough(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","max_output_tokens":12345}`)
	got := decodeRaw(t, in.RawBody)
	if got["max_tokens"] != float64(12345) {
		t.Errorf("max_tokens = %v，期望 12345", got["max_tokens"])
	}
}

// TestSamplingParams 守 temperature / top_p 透传。
func TestSamplingParams(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","temperature":0.7,"top_p":0.9}`)
	got := decodeRaw(t, in.RawBody)
	if got["temperature"] != 0.7 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	if got["top_p"] != 0.9 {
		t.Errorf("top_p = %v", got["top_p"])
	}
}

// TestTextFormatBecomesResponseFormat 守 text.format → response_format。
func TestTextFormatBecomesResponseFormat(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","input":"x",
		"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}}
	}`)
	got := decodeRaw(t, in.RawBody)
	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format 缺失: %+v", got["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v", rf["type"])
	}
}

// TestImageInput 守图片分片翻译。
func TestImageInput(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"看图"},
			{"type":"input_image","image_url":"https://e.com/a.png"}
		]}]
	}`)
	m := msgs(t, in)
	content, _ := m[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("多模态分片数 = %d，期望 2", len(content))
	}
	img, _ := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("图片分片 type = %v", img["type"])
	}
	url, _ := img["image_url"].(map[string]any)["url"].(string)
	if url != "https://e.com/a.png" {
		t.Errorf("图片 URL = %q", url)
	}
}

// TestImageFileIDFails 守：file_id 形式的图片必须报错。
func TestImageFileIDFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_image","file_id":"file_abc"}
		]}]
	}`))
	if err == nil {
		t.Fatal("image file_id 却转换成功 —— 本服务不支持文件上传")
	}
}

// TestDeveloperRoleNormalized 守：developer → system。
//
// 🔴 上游白名单不含 developer，会触发内容过滤误杀
//
//	（见 docs/upstream-contract.md §九）。
func TestDeveloperRoleNormalized(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m",
		"input":[{"type":"message","role":"developer","content":[
			{"type":"input_text","text":"规则"}
		]}]
	}`)
	m := msgs(t, in)
	got, _ := m[0].(map[string]any)
	if got["role"] != "system" {
		t.Errorf("developer 未归一为 system，实际 %v —— 上游会触发内容过滤误杀", got["role"])
	}
}

// TestRejectsMissingModel 守：缺 model 报错。
func TestRejectsMissingModel(t *testing.T) {
	if _, err := Convert([]byte(`{"input":"x"}`)); err == nil {
		t.Fatal("缺 model 却转换成功")
	}
}

// TestRejectsMissingInput 守：缺 input / 空数组报错。
func TestRejectsMissingInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"m"}`,
		`{"model":"m","input":[]}`,
	} {
		if _, err := Convert([]byte(body)); err == nil {
			t.Errorf("缺/空 input 却转换成功: %s", body)
		}
	}
}

// TestRejectsBadJSON 守：非法 JSON 报错。
func TestRejectsBadJSON(t *testing.T) {
	if _, err := Convert([]byte(`{不是JSON`)); err == nil {
		t.Fatal("非法 JSON 却转换成功")
	}
}

// TestStreamFlag 守 stream 标志透传。
func TestStreamFlag(t *testing.T) {
	in := mustConvert(t, `{"model":"m","input":"x","stream":true}`)
	if !in.Stream {
		t.Error("Stream = false，期望 true")
	}
	got := decodeRaw(t, in.RawBody)
	if got["stream"] != true {
		t.Errorf("请求体 stream = %v", got["stream"])
	}
}
