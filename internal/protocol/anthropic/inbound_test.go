package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件是入站翻译的 1:1 护栏测试。
//
// 组织原则：**每个翻译点一个测试**，且断言的是"翻译后的形状"，
// 不是"没有报错"。参考实现修掉的三个缺陷都属于"不报错但语义错"，
// 所以只看 error == nil 的测试毫无价值。

// mustConvert 转换并在失败时终止。
func mustConvert(t *testing.T, body string) *Inbound {
	t.Helper()
	in, err := Convert([]byte(body))
	if err != nil {
		t.Fatalf("Convert 失败: %v\n请求体: %s", err, body)
	}
	return in
}

// decodeRaw 把翻译结果解成 map 便于断言。
func decodeRaw(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("翻译结果不是合法 JSON: %v\n内容: %s", err, b)
	}
	return m
}

// ─────────────────────────────────────────────────────────────
// 翻译点 1：system 是顶层字段
// ─────────────────────────────────────────────────────────────

// TestConvertSystemBecomesFirstMessage 守：Anthropic 的顶层 system
// 必须变成 messages 里的第一条 system 消息。
func TestConvertSystemBecomesFirstMessage(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"system":"你是助手",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d，期望 2（system + user）", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("首条 role = %v，期望 system —— 顶层 system 必须转成首条消息", first["role"])
	}
	if first["content"] != "你是助手" {
		t.Errorf("system 内容 = %v", first["content"])
	}
}

// TestConvertSystemBlockArray 守：system 也可以是 block 数组。
func TestConvertSystemBlockArray(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"system":[{"type":"text","text":"规则一"},{"type":"text","text":"规则二"}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["content"] != "规则一规则二" {
		t.Errorf("block 数组 system 拼接结果 = %v", first["content"])
	}
}

// TestConvertNoSystemNoMessage 守：没有 system 时不能凭空造一条。
func TestConvertNoSystemNoMessage(t *testing.T) {
	in := mustConvert(t, `{"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"hi"}]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 数 = %d，期望 1（无 system 就不该有 system 消息）", len(msgs))
	}
}

// ─────────────────────────────────────────────────────────────
// 翻译点 2：max_tokens 必填，缺失时给默认值（不报错）
// ─────────────────────────────────────────────────────────────

// TestConvertMaxTokensDefault 守：缺 max_tokens 时给 4096 而**不是报错**。
//
// 🔴 反向对照过的点：若改成返回 error，用户发普通对话就会被拒。
func TestConvertMaxTokensDefault(t *testing.T) {
	in := mustConvert(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if in.MaxTokens != DefaultMaxTokens {
		t.Errorf("MaxTokens = %d，期望默认 %d", in.MaxTokens, DefaultMaxTokens)
	}
	got := decodeRaw(t, in.RawBody)
	if got["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("请求体 max_tokens = %v，期望 %d", got["max_tokens"], DefaultMaxTokens)
	}
}

// TestConvertMaxTokensPassthrough 守：给了就用给的值。
func TestConvertMaxTokensPassthrough(t *testing.T) {
	in := mustConvert(t, `{"model":"m","max_tokens":12345,
		"messages":[{"role":"user","content":"hi"}]}`)
	if in.MaxTokens != 12345 {
		t.Errorf("MaxTokens = %d，期望 12345", in.MaxTokens)
	}
}

// TestConvertMaxTokensZeroFallsBack 守：0 / 负数视为未给（Anthropic 要求正数）。
func TestConvertMaxTokensZeroFallsBack(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		in := mustConvert(t, `{"model":"m","max_tokens":`+v+`,
			"messages":[{"role":"user","content":"hi"}]}`)
		if in.MaxTokens != DefaultMaxTokens {
			t.Errorf("max_tokens=%s 时 MaxTokens = %d，期望默认 %d",
				v, in.MaxTokens, DefaultMaxTokens)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 翻译点 3：工具调用双向
// ─────────────────────────────────────────────────────────────

// TestConvertToolUseToToolCalls 守：assistant 的 tool_use 块 → tool_calls。
func TestConvertToolUseToToolCalls(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":"天气"},
			{"role":"assistant","content":[
				{"type":"text","text":"我查一下"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}
			]}
		]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d，期望 2", len(msgs))
	}
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("第二条 role = %v，期望 assistant", asst["role"])
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数 = %d，期望 1", len(tcs))
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" {
		t.Errorf("tool_call id = %v，期望 toolu_1", tc["id"])
	}
	if tc["type"] != "function" {
		t.Errorf("tool_call type = %v，期望 function", tc["type"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v", fn["name"])
	}
	// 参数必须是 JSON 字符串（Chat 规范）
	args, _ := fn["arguments"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments 不是 JSON 字符串: %v（原文 %q）", err, args)
	}
	if parsed["city"] != "北京" {
		t.Errorf("arguments.city = %v", parsed["city"])
	}
	// 工具调用与文本可以并存
	if asst["content"] != "我查一下" {
		t.Errorf("assistant content = %v，期望与 tool_calls 并存的文本", asst["content"])
	}
}

// TestConvertToolResultBecomesToolMessage 守：user 的 tool_result → 独立 role=tool 消息，
// 且 **assistant 必须排在它之前**。
//
// 🔴 顺序错了上游会报错（Chat 规范要求 tool 结果紧跟其调用）。
func TestConvertToolResultBecomesToolMessage(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":"天气"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"晴 25 度"}
			]}
		]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages 数 = %d，期望 3（user / assistant / tool）", len(msgs))
	}
	// 顺序断言：assistant 必须在 tool 之前
	asst, _ := msgs[1].(map[string]any)
	tool, _ := msgs[2].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("第 2 条 role = %v，期望 assistant（必须排在 tool 结果之前）", asst["role"])
	}
	if tool["role"] != "tool" {
		t.Fatalf("第 3 条 role = %v，期望 tool", tool["role"])
	}
	if tool["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_call_id = %v，期望 toolu_1", tool["tool_call_id"])
	}
	if tool["content"] != "晴 25 度" {
		t.Errorf("tool content = %v", tool["content"])
	}
}

// TestConvertToolResultError 守：is_error 必须体现在内容里（不能丢）。
func TestConvertToolResultError(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"t1","name":"f","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"超时","is_error":true}
			]}
		]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	tool, _ := msgs[len(msgs)-1].(map[string]any)
	if c, _ := tool["content"].(string); !strings.Contains(c, "ERROR") {
		t.Errorf("is_error=true 的结果 = %q，期望含 ERROR 标记", c)
	}
}

// TestConvertToolResultMissingIDFails 守：tool_result 缺 tool_use_id 必须报错。
//
// 🔴 不能静默造一个 id：上游用 tool_call_id 关联具体调用，凭空造的 id
// 匹配不上，结果是"看起来成功但模型拿不到工具结果"。
func TestConvertToolResultMissingIDFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"tool_result","content":"结果"}
		]}]}`))
	if err == nil {
		t.Fatal("tool_result 缺 tool_use_id 却转换成功 —— " +
			"凭空造 id 会让模型拿不到工具结果，必须报错")
	}
	if !strings.Contains(err.Error(), "tool_use_id") {
		t.Errorf("错误信息未点名 tool_use_id: %v", err)
	}
}

// TestConvertToolResultBlockContent 守：tool_result.content 可以是 block 数组。
func TestConvertToolResultBlockContent(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"t1","name":"f","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1",
				 "content":[{"type":"text","text":"甲"},{"type":"text","text":"乙"}]}
			]}
		]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	tool, _ := msgs[len(msgs)-1].(map[string]any)
	if tool["content"] != "甲乙" {
		t.Errorf("block 数组 tool_result 拼接 = %v", tool["content"])
	}
}

// ─────────────────────────────────────────────────────────────
// tools 定义与 tool_choice
// ─────────────────────────────────────────────────────────────

// TestConvertToolsInputSchema 守：input_schema → function.parameters。
func TestConvertToolsInputSchema(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"get_weather","description":"查天气",
			"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
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
		t.Errorf("tool type = %v，期望 function", tool["type"])
	}
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v", fn["name"])
	}
	if fn["description"] != "查天气" {
		t.Errorf("function.description = %v", fn["description"])
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters 未从 input_schema 翻译过来: %v", params)
	}
}

// TestConvertToolsMissingSchemaGetsEmptyObject 守：input_schema 缺失时
// 给空 schema 而**不是** null —— 上游要求 parameters 存在，null 会 400。
func TestConvertToolsMissingSchemaGetsEmptyObject(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"noop"}]
	}`)
	got := decodeRaw(t, in.RawBody)
	tools, _ := got["tools"].([]any)
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("缺失 input_schema 时 parameters = %v，期望空 object schema", fn["parameters"])
	}
}

// TestConvertServerToolFails 守：服务端工具（web_search_20250305）必须**明确报错**。
//
// 🔴 委托方 2026-10-09 拍板第 3 条：做不到的特性明确报错，不静默丢弃。
//
//	静默跳过的后果：客户端以为工具可用，然后一直等一个永远不会来的 tool_use。
func TestConvertServerToolFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"搜一下"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3}]
	}`))
	if err == nil {
		t.Fatal("服务端工具却转换成功 —— 本服务无法代为执行搜索，" +
			"必须明确报错而不是静默丢弃")
	}
	if !strings.Contains(err.Error(), "web_search_20250305") {
		t.Errorf("错误信息未点名工具 type: %v", err)
	}
}

// TestConvertToolChoiceMapping 守 tool_choice 的四种映射。
func TestConvertToolChoiceMapping(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
	}
	for _, tc := range cases {
		in := mustConvert(t, `{"model":"m","max_tokens":100,
			"messages":[{"role":"user","content":"x"}],
			"tool_choice":`+tc.in+`}`)
		got := decodeRaw(t, in.RawBody)
		if got["tool_choice"] != tc.want {
			t.Errorf("tool_choice %s → %v，期望 %v", tc.in, got["tool_choice"], tc.want)
		}
	}
}

// TestConvertToolChoiceNamedToolFails 守：{"type":"tool","name":"x"} 必须报错。
//
// 🔴 为什么不降级为 "required"（最省事的错误修法）：
//
//	那会**改变语义** —— 客户端要求"必须调用 x"，变成"必须调用某个工具"，
//	模型可能去调 y。属于委托方明令禁止的"静默改变行为"。
//	而且上游要求 tool_choice 是字符串，传对象直接 400（code=11101）。
func TestConvertToolChoiceNamedToolFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"x","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"x"}
	}`))
	if err == nil {
		t.Fatal("tool_choice.type=tool 却转换成功 —— 降级为 required 会改变语义，" +
			"必须明确报错")
	}
}

// ─────────────────────────────────────────────────────────────
// 翻译点 6：thinking
// ─────────────────────────────────────────────────────────────

// TestConvertThinkingMapping 守 thinking 三种取值的映射。
//
// 🔴 DSH 实测（2026-10-09，asar 提取）：
//
//	路径 A 只发 {type:"enabled"} 或 {type:"disabled"}（无 budget_tokens）
//	路径 B 还发 {type:"adaptive"}，且带 display / block_binding 子字段
//	⇒ 三者都必须接受，且非标准子字段必须被忽略而不是报错。
func TestConvertThinkingMapping(t *testing.T) {
	cases := []struct {
		name     string
		thinking string
		want     string
	}{
		{"disabled", `{"type":"disabled"}`, "off"},
		{"enabled", `{"type":"enabled"}`, ""},
		{"enabled+budget", `{"type":"enabled","budget_tokens":8000}`, ""},
		{"adaptive", `{"type":"adaptive"}`, ""},
		{
			"adaptive+非标准子字段",
			`{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := mustConvert(t, `{"model":"m","max_tokens":100,
				"messages":[{"role":"user","content":"x"}],
				"thinking":`+tc.thinking+`}`)
			if in.ReasoningEffort != tc.want {
				t.Errorf("thinking %s → ReasoningEffort %q，期望 %q",
					tc.thinking, in.ReasoningEffort, tc.want)
			}
		})
	}
}

// TestConvertOutputConfigEffort 守：**档位值来自 output_config.effort**。
//
// 🔴 这是 DSH 的真实契约（asar 提取，2026-10-09）：
//
//	thinking:      { type: effort==="off" ? "disabled" : "enabled" }
//	output_config: { effort }        ← effort ∈ off/low/high/max
//
//	**档位值只在 output_config.effort 里**。只读 thinking.type 会把
//	low/high/max 全部抹平 —— 用户在 DSH 里选 "max" 却拿到 high，
//	属于静默改变行为。
func TestConvertOutputConfigEffort(t *testing.T) {
	cases := []struct {
		effort string
		want   string
	}{
		{"minimal", "minimal"},
		{"low", "low"},
		{"high", "high"},
		{"max", "max"},
		{"off", "off"},
	}
	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			in := mustConvert(t, `{"model":"m","max_tokens":100,
				"messages":[{"role":"user","content":"x"}],
				"thinking":{"type":"enabled"},
				"output_config":{"effort":"`+tc.effort+`"}}`)
			if in.ReasoningEffort != tc.want {
				t.Errorf("output_config.effort=%q → ReasoningEffort %q，期望 %q",
					tc.effort, in.ReasoningEffort, tc.want)
			}
		})
	}
}

// TestConvertOutputConfigEffortWinsOverThinking 守优先级：
// output_config.effort 胜过 thinking.type。
//
// DSH 发 thinking.type="enabled" + output_config.effort="off" 是
// 不可能的组合，但若客户端真这么发，档位字段更具体，应当胜出。
func TestConvertOutputConfigEffortWinsOverThinking(t *testing.T) {
	in := mustConvert(t, `{"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"enabled"},
		"output_config":{"effort":"low"}}`)
	if in.ReasoningEffort != "low" {
		t.Errorf("ReasoningEffort = %q，期望 low（output_config 优先于 thinking.type）",
			in.ReasoningEffort)
	}
}

// TestConvertThinkingRejectsUnknownEffort 守：未知档位**显式报错**。
//
// 🔴 委托方第 3 条边界：与其把 "veryhigh" 发上去换一个上游 400，
//
//	不如当场说清楚是哪个值不被支持。
func TestConvertThinkingRejectsUnknownEffort(t *testing.T) {
	_, err := Convert([]byte(`{"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"enabled"},
		"output_config":{"effort":"veryhigh"}}`))
	if err == nil {
		t.Fatal("未知档位 veryhigh 却转换成功 —— 应显式报错而不是发上去换 400")
	}
	if !strings.Contains(err.Error(), "veryhigh") {
		t.Errorf("错误信息未点名非法档位: %v", err)
	}
}

// TestConvertThinkingNotInRawBody 守：思考档位**不能**写进 RawBody。
//
// 🔴 写两处必然漂移：provider.buildChatBody 会用
// ChatRequest.ReasoningEffort 覆盖该字段。留在这里会被静默覆盖，
// 排查时看到请求体里有一个不生效的值，极难定位。
func TestConvertThinkingNotInRawBody(t *testing.T) {
	in := mustConvert(t, `{"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"enabled"}}`)
	got := decodeRaw(t, in.RawBody)
	if _, has := got["reasoning_effort"]; has {
		t.Error("reasoning_effort 不应写进 RawBody —— " +
			"provider 会用 ChatRequest.ReasoningEffort 覆盖它，两处必然不一致")
	}
	if _, has := got["thinking"]; has {
		t.Error("thinking 不应原样透传给上游（上游不认这个字段）")
	}
}

// ─────────────────────────────────────────────────────────────
// 显式报错的边界（委托方第 3 条）
// ─────────────────────────────────────────────────────────────

// TestConvertDocumentFails 守：document（PDF）块必须明确报错。
func TestConvertDocumentFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAA"}}
		]}]}`))
	if err == nil {
		t.Fatal("document 块却转换成功 —— 静默丢弃会让模型看不到用户上传的文档")
	}
	if !strings.Contains(err.Error(), "document") {
		t.Errorf("错误信息未点名 document: %v", err)
	}
}

// TestConvertUnknownBlockFails 守：未知 content block 类型必须报错。
//
// 🔴 这是"错误的成功"最典型的来源：静默跳过 = 用户以为内容发出去了，
//
//	实际模型没看到。
func TestConvertUnknownBlockFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"未来新块类型","data":"x"}
		]}]}`))
	if err == nil {
		t.Fatal("未知 content block 却转换成功 —— 静默跳过是「错误的成功」")
	}
	if !strings.Contains(err.Error(), "未来新块类型") {
		t.Errorf("错误信息未点名块类型: %v", err)
	}
}

// TestConvertUnknownImageSourceFails 守：不支持的 image.source.type 必须报错。
func TestConvertUnknownImageSourceFails(t *testing.T) {
	_, err := Convert([]byte(`{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"file_ref","file_id":"f1"}}
		]}]}`))
	if err == nil {
		t.Fatal("未知 image source 却转换成功 —— 静默返回空串会让模型收到" +
			"一条「没有图片」的消息")
	}
}

// ─────────────────────────────────────────────────────────────
// 宽容性（DSH 实测要求）
// ─────────────────────────────────────────────────────────────

// TestConvertToleratesUnknownTopLevelFields 守：未知顶层字段必须被忽略。
//
// 🔴 DSH 实测（asar 提取，2026-10-09）：路径 A 默认开启两个扩展，
//
//	会往请求体注入 **dsh_session_log** 与 **dsh_plugin_packages** 顶层字段。
//	若做顶层 key 白名单校验，DSH 的所有请求都会被拒。
func TestConvertToleratesUnknownTopLevelFields(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"dsh_session_log":"一些日志",
		"dsh_plugin_packages":["a","b"],
		"output_config":{"effort":"high"},
		"betas":["files-api-2025-04-14"]
	}`)
	if in.Model != "m" {
		t.Errorf("Model = %q", in.Model)
	}
	got := decodeRaw(t, in.RawBody)
	// 未知字段不应被转发给上游（上游不认，且可能触发内容过滤）
	for _, k := range []string{"dsh_session_log", "dsh_plugin_packages", "output_config", "betas"} {
		if _, has := got[k]; has {
			t.Errorf("未知顶层字段 %q 被转发给上游 —— 应忽略", k)
		}
	}
}

// TestConvertImageBase64 守 base64 图片 → data URI。
func TestConvertImageBase64(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
		]}]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("多模态 content 分片数 = %d，期望 2", len(content))
	}
	img, _ := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("图片分片 type = %v，期望 image_url", img["type"])
	}
	url, _ := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Errorf("data URI = %q，期望 data:image/jpeg;base64, 前缀", url)
	}
}

// TestConvertImageURL 守直链图片。
func TestConvertImageURL(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://e.com/a.png"}}
		]}]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	url, _ := content[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if url != "https://e.com/a.png" {
		t.Errorf("图片 URL = %q", url)
	}
}

// TestConvertHistoryThinkingDropped 守：历史 thinking 块被丢弃（有意为之）。
//
// ⚠️ 这不违反"不静默丢弃"：思考是过程性内容，上游不接受历史 thinking，
//
//	且它对后续推理没有信息增量。有意的简化要能被测试看见。
func TestConvertHistoryThinkingDropped(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"我先想想","signature":"sig"},
				{"type":"text","text":"答案"}
			]}
		]}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 数 = %d，期望 1（thinking 块不产生消息）", len(msgs))
	}
	asst, _ := msgs[0].(map[string]any)
	if asst["content"] != "答案" {
		t.Errorf("content = %v，期望只保留文本块", asst["content"])
	}
	if s, _ := asst["content"].(string); strings.Contains(s, "我先想想") {
		t.Error("历史 thinking 内容不应进入请求体")
	}
}

// ─────────────────────────────────────────────────────────────
// 其他翻译点与校验
// ─────────────────────────────────────────────────────────────

// TestConvertSamplingParams 守 temperature / top_p / top_k / stop_sequences。
func TestConvertSamplingParams(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"temperature":0.7,"top_p":0.9,"top_k":40,
		"stop_sequences":["\n\n","STOP"]
	}`)
	got := decodeRaw(t, in.RawBody)
	if got["temperature"] != 0.7 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	if got["top_p"] != 0.9 {
		t.Errorf("top_p = %v", got["top_p"])
	}
	if got["top_k"] != float64(40) {
		t.Errorf("top_k = %v", got["top_k"])
	}
	stop, _ := got["stop"].([]any)
	if len(stop) != 2 || stop[0] != "\n\n" {
		t.Errorf("stop = %v，期望 stop_sequences 原样翻译", stop)
	}
}

// TestConvertMetadataUser 守 metadata.user_id → user。
func TestConvertMetadataUser(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"x"}],
		"metadata":{"user_id":"u-123"}
	}`)
	got := decodeRaw(t, in.RawBody)
	if got["user"] != "u-123" {
		t.Errorf("user = %v，期望 metadata.user_id", got["user"])
	}
}

// TestConvertRejectsMissingModel 守：缺 model 必须报错。
func TestConvertRejectsMissingModel(t *testing.T) {
	if _, err := Convert([]byte(`{"max_tokens":100,"messages":[{"role":"user","content":"x"}]}`)); err == nil {
		t.Fatal("缺 model 却转换成功")
	}
}

// TestConvertRejectsMissingMessages 守：缺 messages / 空数组必须报错。
func TestConvertRejectsMissingMessages(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":100}`,
		`{"model":"m","max_tokens":100,"messages":[]}`,
	} {
		if _, err := Convert([]byte(body)); err == nil {
			t.Errorf("缺/空 messages 却转换成功: %s", body)
		}
	}
}

// TestConvertRejectsBadJSON 守：非法 JSON 必须报错。
func TestConvertRejectsBadJSON(t *testing.T) {
	if _, err := Convert([]byte(`{不是JSON`)); err == nil {
		t.Fatal("非法 JSON 却转换成功")
	}
}

// TestConvertRoleNormalization 守：非 assistant 的 role 一律按 user 处理。
func TestConvertRoleNormalization(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"怪角色","content":"x"}]
	}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("未知 role 未归一为 user: %v", msgs[0])
	}
}

// TestConvertContentStringShortcut 守：content 允许纯字符串简写。
func TestConvertContentStringShortcut(t *testing.T) {
	in := mustConvert(t, `{
		"model":"m","max_tokens":100,
		"messages":[{"role":"user","content":"纯字符串"}]
	}`)
	got := decodeRaw(t, in.RawBody)
	msgs, _ := got["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "纯字符串" {
		t.Errorf("字符串 content 未正确转换: %v", msgs[0])
	}
}

// TestConvertStreamFlag 守 stream 标志被如实传递。
func TestConvertStreamFlag(t *testing.T) {
	in := mustConvert(t, `{"model":"m","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"x"}]}`)
	if !in.Stream {
		t.Error("Stream = false，期望 true")
	}
	got := decodeRaw(t, in.RawBody)
	if got["stream"] != true {
		t.Errorf("请求体 stream = %v", got["stream"])
	}
}
