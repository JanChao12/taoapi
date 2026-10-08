package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件实现入站翻译：Responses API 请求 → OpenAI Chat 形状。
//
// 🔴 Responses 的入站形状与 Chat/Anthropic **都不一样**，三个关键差异：
//
//  1. **system 叫 `instructions`**（顶层字符串），不是 messages 里的 role。
//  2. **`input` 是"item 数组"**，而不是"message 数组"：
//     每条 message 的 content 是 `[{type:"input_text",text:...}]`，
//     且工具调用/结果是与 message **平级**的独立 item
//     （type=function_call / function_call_output），不是嵌在 content 里。
//  3. **工具定义是扁平的**：`{type:"function",name,parameters}` ——
//     没有 Chat 的 `function:{}` 那一层嵌套。
//
// ⚠️ 委托方第 3 条边界（做不到的明确报错）在本文件的落点：
//	服务端工具（web_search / file_search / computer_use 等）、
//	`previous_response_id`（服务端多轮状态）、未知 item 类型、
//	未知 content part 类型 —— 一律 invalid_request_error 并点名。

// DefaultMaxOutputTokens 是 max_output_tokens 缺失时的默认值。
//
// ⚠️ 与 Anthropic 不同：Responses 的 max_output_tokens 是**可选**的
//
//	（Chat 的 max_tokens 也是可选），因此这里不是"补规范要求"
//	而是"给一个安全的默认值"。取 4096 与 anthropic 包保持一致，
//	便于用户理解两个协议的行为。
const DefaultMaxOutputTokens = 4096

// Inbound 是翻译后的入站请求。
type Inbound struct {
	// RawBody 是 OpenAI Chat 形状的请求体（model 仍为客户端原名）。
	RawBody []byte

	// Model 客户端请求的模型名（原始，可能命中别名）。
	Model string

	// Stream 客户端是否要求流式。
	Stream bool

	// ReasoningEffort 由 reasoning.effort 翻译来的思考档位。
	//
	// ⚠️ 刻意不写进 RawBody：provider.buildChatBody 会用
	//	ChatRequest.ReasoningEffort 覆盖该字段。
	ReasoningEffort string

	// HasTools 请求里是否带工具定义。
	HasTools bool
}

// Convert 把 Responses 请求体翻译为 OpenAI Chat 形状。
func Convert(body []byte) (*Inbound, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()

	var in map[string]any
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if in == nil {
		return nil, fmt.Errorf("请求体必须是 JSON 对象")
	}

	model := strings.TrimSpace(asString(in["model"]))
	if model == "" {
		return nil, fmt.Errorf("缺少 model 字段")
	}

	// 🔴 previous_response_id：服务端多轮状态，本服务无法支持。
	//
	//	静默忽略是最坏的选择：客户端以为服务端记得上文，于是只发最新一句，
	//	模型却看到一条孤立的消息 —— 用户会得到"答非所问"的结果，
	//	而且完全不知道为什么。
	if v := strings.TrimSpace(asString(in["previous_response_id"])); v != "" {
		return nil, fmt.Errorf(
			"不支持 previous_response_id（服务端多轮状态）：本服务不保存会话，" +
				"请把完整对话放在 input 里重新发送")
	}

	out := map[string]any{
		"model":  model,
		"stream": asBool(in["stream"]),
	}

	// ── instructions → 首条 system 消息 ──
	var msgs []any
	if ins := strings.TrimSpace(asString(in["instructions"])); ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}

	// ── input：字符串 或 item 数组 ──
	converted, err := convertInput(in["input"])
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, converted...)

	if len(msgs) == 0 {
		return nil, fmt.Errorf("`input` 必填且必须包含至少一条消息")
	}
	out["messages"] = msgs

	// ── 输出上限 ──
	maxOut := DefaultMaxOutputTokens
	if n, has := asInt(in["max_output_tokens"]); has && n > 0 {
		maxOut = n
	}
	out["max_tokens"] = maxOut

	// ── 采样参数 ──
	// ⚠️ Responses 把它们放在顶层（temperature / top_p），与 Chat 相同。
	if f, ok := asFloat(in["temperature"]); ok {
		out["temperature"] = f
	}
	if f, ok := asFloat(in["top_p"]); ok {
		out["top_p"] = f
	}

	// ── tools：扁平形状 → Chat 嵌套形状 ──
	hasTools := false
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		convertedTools, err := convertTools(tools)
		if err != nil {
			return nil, err
		}
		if len(convertedTools) > 0 {
			out["tools"] = convertedTools
			hasTools = true
		}
	}

	// ── tool_choice ──
	if tc, err := convertToolChoice(in["tool_choice"]); err != nil {
		return nil, err
	} else if tc != nil {
		out["tool_choice"] = tc
	}

	// ── reasoning.effort → 思考档位 ──
	effort, err := convertReasoning(in["reasoning"])
	if err != nil {
		return nil, err
	}

	// ── text.format → response_format ──
	//
	// ⚠️ Responses 把结构化输出放在 text.format 里，Chat 用 response_format。
	//	形状基本一致，直接透传内部对象。
	if text, ok := in["text"].(map[string]any); ok {
		if format, has := text["format"]; has && !isEmptyValue(format) {
			out["response_format"] = format
		}
	}

	// ── 明确报错的字段 ──
	//
	// 这些字段一旦被静默忽略，客户端会以为生效了。
	if _, has := in["store"]; has && asBool(in["store"]) {
		return nil, fmt.Errorf(
			"不支持 store=true（服务端保存响应）：本服务不保存任何会话数据")
	}
	if v := strings.TrimSpace(asString(in["truncation"])); v != "" && v != "disabled" {
		return nil, fmt.Errorf(
			"不支持 truncation=%q：本服务不做服务端上下文截断，请自行裁剪 input", v)
	}
	// ── include：**接受并忽略**（绝不能报错！）──
	//
	// 🔴 这里原本写的是"显式报错"，那是**错的** —— 它会让 DSH 的 Responses
	//	路径**每一个带思考的请求都失败**。
	//
	//	依据（2026-10-09 从 DSH 的 app.asar 提取的真实契约，
	//	pi-ai/dist/api/openai-responses.js:230-270）：
	//
	//	  if (model.reasoning) {
	//	      if (options?.reasoningEffort || options?.reasoningSummary) {
	//	          params.reasoning = { effort, summary: "auto" };
	//	          params.include = ["reasoning.encrypted_content"];  // ← 总是带上
	//	      }
	//	  }
	//
	//	⇒ 只要客户端开了思考，DSH 就**必然**发 include。
	//
	//	为什么"忽略"不算违反委托方第 3 条边界（做不到的明确报错）：
	//
	//	  include 的语义是"额外**可选**地附带这些数据"，不是"我需要这个能力
	//	  才能工作"。客户端要的 encrypted_content 我们不产出，它就只是拿不到
	//	  那段加密串；DSH 只用它做 store:false 下的多轮重放兜底，
	//	  缺失既不会让请求失败，也不会让模型少看到任何内容。
	//	  ⇒ 不产出它**不是谎言**，所以正确做法是接受而非拒绝。
	//
	//	⚠️ 反例对照（为什么 tools 里的服务端工具**必须**报错）：
	//	  那会让客户端**一直等一个永远不会来的工具调用**，属于"错误的成功"。
	//	  两者的区别是：include 是可选附加数据，服务端工具是功能承诺。
	if inc, ok := in["include"].([]any); ok && len(inc) > 0 {
		// 只校验形状，不因内容报错（见上）。
		for i, v := range inc {
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("include[%d] 必须是字符串", i)
			}
		}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("序列化翻译结果失败: %w", err)
	}

	return &Inbound{
		RawBody:         raw,
		Model:           model,
		Stream:          asBool(in["stream"]),
		ReasoningEffort: effort,
		HasTools:        hasTools,
	}, nil
}

// convertInput 把 Responses 的 input 翻译为 Chat 的 messages。
//
// 两种形状：
//   - 字符串：直接当一条 user 消息
//   - item 数组：逐个翻译，可能产出多条 Chat 消息
//
// 🔴 关键结构差异（这是最容易写错的地方）：
//
//	Responses 的 input 是**扁平的 item 列表**，function_call 与
//	function_call_output 是与 message **平级**的独立 item：
//
//	  [{type:"message",role:"user",content:[{type:"input_text",...}]},
//	   {type:"function_call",call_id:"c1",name:"f",arguments:"{}"},
//	   {type:"function_call_output",call_id:"c1",output:"..."}]
//
//	而 Chat 要求工具调用**挂在 assistant 消息里**（tool_calls 数组），
//	工具结果单独成一条 role=tool 消息。
//
//	⇒ 必须把"平级的 function_call item"**归并进前一条 assistant 消息**。
//	  若照搬成独立消息，上游会因为没有 tool_calls 的 assistant 而报错。
func convertInput(v any) ([]any, error) {
	// 形状一：纯字符串
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}

	items, ok := v.([]any)
	if !ok {
		if v == nil {
			return nil, fmt.Errorf("`input` 必填")
		}
		return nil, fmt.Errorf("`input` 必须是字符串或 item 数组")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("`input` 数组不能为空")
	}

	var out []any

	// pendingCalls 累积"尚未挂到 assistant 消息上的"函数调用。
	//
	// 🔴 为什么要缓冲（而不是每遇到一个 function_call 就立即产出一条
	//	assistant 消息）：
	//
	//	Responses 允许一个模型回合产出**多个** function_call，它们连续出现。
	//	若每个都单独成一条 assistant 消息，会变成 N 条只带一个 tool_call
	//	的 assistant 消息 —— 虽然形状合法，但与真实对话结构不符，
	//	且某些上游对"assistant 消息不含 content"较敏感。
	//	缓冲到下一个非 function_call item 时一次性挂上，最贴近原始语义。
	var pendingCalls []any

	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		out = append(out, map[string]any{
			"role": "assistant", "content": "", "tool_calls": pendingCalls,
		})
		pendingCalls = nil
	}

	for i, raw := range items {
		im, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input[%d] 必须是对象", i)
		}
		typ := strings.ToLower(strings.TrimSpace(asString(im["type"])))

		switch typ {
		case "function_call":
			callID := strings.TrimSpace(asString(im["call_id"]))
			if callID == "" {
				// 🔴 不能凭空造：function_call_output 靠 call_id 关联回来，
				//	造一个 id 会让后续的工具结果永远匹配不上。
				return nil, fmt.Errorf(
					"input[%d]: function_call 缺少 call_id（无法与 function_call_output 关联）", i)
			}
			name := strings.TrimSpace(asString(im["name"]))
			if name == "" {
				return nil, fmt.Errorf("input[%d]: function_call 缺少 name", i)
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": firstNonEmpty(asString(im["arguments"]), "{}"),
				},
			})

		case "function_call_output":
			flushCalls() // assistant 必须排在工具结果之前
			callID := strings.TrimSpace(asString(im["call_id"]))
			if callID == "" {
				return nil, fmt.Errorf(
					"input[%d]: function_call_output 缺少 call_id（无法关联到函数调用）", i)
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      stringify(im["output"]),
			})

		case "reasoning":
			// 历史思考项**有意丢弃**：上游不接受历史 thinking 内容，
			// 且它对后续推理没有信息增量（与 anthropic 包同样的取舍）。
			//
			// ⚠️ 但不能连它后面的 function_call 一起丢 —— reasoning item
			//	本身不带 call_id，不需要 flush。
			continue

		case "message", "":
			flushCalls() // 消息必须排在它引用的工具调用之后
			msg, err := convertMessageItem(im, i)
			if err != nil {
				return nil, err
			}
			if msg != nil {
				out = append(out, msg)
			}

		case "item_reference", "computer_call", "computer_call_output",
			"web_search_call", "file_search_call", "code_interpreter_call",
			"image_generation_call", "local_shell_call", "mcp_call", "mcp_approval_response":
			// 🔴 服务端工具产生的 item：明确报错，绝不静默丢弃。
			//
			//	静默跳过会让模型看到一段"凭空缺失"的上下文，
			//	表现为答非所问，而用户完全不知道为什么。
			return nil, fmt.Errorf(
				"input[%d]: 不支持 item 类型 %q（服务端工具/引用）—— "+
					"本服务无法代为执行这些工具", i, typ)

		default:
			return nil, fmt.Errorf("input[%d]: 不支持的 item 类型 %q", i, typ)
		}
	}
	flushCalls()

	if len(out) == 0 {
		return nil, fmt.Errorf("`input` 里没有可转换的内容")
	}
	return out, nil
}

// convertMessageItem 转换一条 message item。
func convertMessageItem(im map[string]any, idx int) (any, error) {
	role := strings.ToLower(strings.TrimSpace(asString(im["role"])))
	if role == "" {
		role = "user"
	}
	// Responses 的 role 取值：user / assistant / system / developer
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil, fmt.Errorf("input[%d]: 不支持的 role %q", idx, role)
	}

	content := im["content"]

	// content 允许纯字符串简写
	if s, ok := content.(string); ok {
		return map[string]any{"role": normalizeRole(role), "content": s}, nil
	}
	parts, ok := content.([]any)
	if !ok {
		if content == nil {
			// assistant 消息可以没有 content（例如只带工具调用）
			return nil, nil
		}
		return nil, fmt.Errorf("input[%d].content 必须是字符串或分片数组", idx)
	}

	var (
		texts []string
		imgs  []any
	)
	for pi, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input[%d].content[%d] 必须是对象", idx, pi)
		}
		switch strings.ToLower(strings.TrimSpace(asString(pm["type"]))) {
		case ItemInputText, ItemOutputText, "text":
			if t := asString(pm["text"]); t != "" {
				texts = append(texts, t)
			}
		case ItemInputImage:
			url, err := imageURL(pm)
			if err != nil {
				return nil, fmt.Errorf("input[%d].content[%d]: %w", idx, pi, err)
			}
			imgs = append(imgs, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
		case "refusal":
			// 模型拒答的历史记录：当成普通文本回传，保住上下文语义
			if t := asString(pm["refusal"]); t != "" {
				texts = append(texts, t)
			}
		default:
			return nil, fmt.Errorf(
				"input[%d].content[%d]: 不支持的 content 分片类型 %q",
				idx, pi, asString(pm["type"]))
		}
	}

	if len(texts) == 0 && len(imgs) == 0 {
		return nil, nil
	}

	var c any
	if len(imgs) > 0 {
		mixed := make([]any, 0, len(texts)+len(imgs))
		for _, t := range texts {
			mixed = append(mixed, map[string]any{"type": "text", "text": t})
		}
		mixed = append(mixed, imgs...)
		c = mixed
	} else {
		c = strings.Join(texts, "\n")
	}
	return map[string]any{"role": normalizeRole(role), "content": c}, nil
}

// normalizeRole 把 Responses 的 role 归一为 Chat 角色。
//
// developer → system：上游白名单不含 developer，会触发内容过滤误杀
// （见 docs/upstream-contract.md §九）。
func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return "assistant"
	case "system", "developer":
		return "system"
	}
	return "user"
}

// imageURL 从 input_image 分片里取出可用 URL。
//
// Responses 的形状与 Anthropic 不同：图片地址直接在 `image_url` 字段
// （不是嵌套的 source 对象）。
func imageURL(part map[string]any) (string, error) {
	// 形状一：image_url 直接是字符串
	if u := strings.TrimSpace(asString(part["image_url"])); u != "" {
		return u, nil
	}
	// 形状二：image_url 是对象 {url: "..."}
	if m, ok := part["image_url"].(map[string]any); ok {
		if u := strings.TrimSpace(asString(m["url"])); u != "" {
			return u, nil
		}
	}
	// 形状三：file_id 引用（需要先上传文件，本服务不支持）
	if fid := strings.TrimSpace(asString(part["file_id"])); fid != "" {
		return "", fmt.Errorf(
			"不支持 image 的 file_id 引用（%s）：请改用 image_url 直接给图片地址", fid)
	}
	return "", fmt.Errorf("image 分片缺少可用的 image_url")
}

// convertTools 把 Responses 的扁平工具定义翻译为 Chat 的嵌套形状。
//
//	Responses: {type:"function", name:"f", description:"...", parameters:{...}, strict:true}
//	Chat:      {type:"function", function:{name:"f", description:"...", parameters:{...}}}
//
// 🔴 服务端内置工具（web_search / file_search / computer_use / code_interpreter
//
//	/ image_generation / mcp）**明确报错**：它们没有 Chat 等价物，
//	静默跳过会让客户端一直等一个永远不会来的工具调用。
func convertTools(tools []any) ([]any, error) {
	converted := make([]any, 0, len(tools))
	for i, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d] 必须是对象", i)
		}
		typ := strings.ToLower(strings.TrimSpace(asString(tm["type"])))

		// 内置服务端工具：有 type 但不是 function
		if typ != "" && typ != "function" && typ != "custom" {
			return nil, fmt.Errorf(
				"tools[%d]: 不支持内置工具 type=%q —— 本服务无法代为执行，"+
					"静默跳过会让客户端一直等一个不会到来的工具调用", i, typ)
		}
		// 兼容 Chat 风格的嵌套定义（部分客户端混用）
		if fn, ok := tm["function"].(map[string]any); ok {
			name := strings.TrimSpace(asString(fn["name"]))
			if name == "" {
				return nil, fmt.Errorf("tools[%d].function 缺少 name", i)
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": asString(fn["description"]),
					"parameters":  orEmptyObject(fn["parameters"]),
				},
			})
			continue
		}

		name := strings.TrimSpace(asString(tm["name"]))
		if name == "" {
			return nil, fmt.Errorf("tools[%d] 缺少 name", i)
		}
		converted = append(converted, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": asString(tm["description"]),
				"parameters":  orEmptyObject(tm["parameters"]),
			},
		})
	}
	return converted, nil
}

// convertToolChoice 把 Responses 的 tool_choice 翻译为 Chat 形状。
//
//	"auto" / "none" / "required"            → 原样
//	{type:"function", name:"f"}             → 显式报错（见下）
//	{type:"allowed_tools", ...}             → 显式报错
//
// 🔴 为什么指定函数名要报错：上游要求 tool_choice 是**字符串**，
//
//	传对象直接 400（code=11101，见 docs/upstream-contract.md §九）。
//	降级为 "required" 会改变语义（客户端要求"必须调 f"，变成"必须调某个工具"），
//	属于委托方明令禁止的静默改变行为。
func convertToolChoice(v any) (any, error) {
	if v == nil || isEmptyValue(v) {
		return nil, nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	tc, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	typ := strings.ToLower(strings.TrimSpace(asString(tc["type"])))
	switch typ {
	case "function":
		name := asString(tc["name"])
		if name == "" {
			return nil, fmt.Errorf("tool_choice.type=function 但缺少 name")
		}
		return nil, fmt.Errorf(
			"不支持 tool_choice 指定具体函数（name=%q）：上游只接受字符串形式的 "+
				"tool_choice，降级为 required 会改变语义（可能调用别的工具）", name)
	case "allowed_tools":
		return nil, fmt.Errorf(
			"不支持 tool_choice.type=allowed_tools（上游无等价物，且需服务端强制约束）")
	case "auto", "none", "required":
		return typ, nil
	}
	// 未知取值：不拦，交给上游判定（我们不做比上游更严的校验）
	return nil, nil
}

// convertReasoning 把 Responses 的 reasoning.effort 翻译为思考档位。
//
// 🔴 这里复用 provider 层的**唯一权威定义**，不再自己维护一份白名单。
//
//	起因：`off` 曾在本项目里被当作合法档位对外声明，而实测上游对它返回
//	400 code=11150 —— 根因就是"哪些档位上游真的接受"这个事实存在多份副本，
//	修一处漏一处。现在只有 provider.IsUpstreamEffort / DisablesThinking 一份。
//
// 映射：
//
//	reasoning.effort = minimal/low/medium/high/xhigh/max/ultra → 原样透传
//	reasoning.effort = off / none                             → "off"
//	                      （provider 会翻译成「不传该字段」——上游对
//	                        字面值 off 返回 400，唯一能关掉思考的方式是不传）
//	reasoning.effort 缺失 / reasoning={}                       → ""
//	                      （交由 provider 注入模型默认档）
//
// ⚠️ 未知档位值**显式报错**：与其发上去换一个上游 400，不如当场点名。
func convertReasoning(v any) (string, error) {
	if v == nil || isEmptyValue(v) {
		return "", nil
	}
	rm, ok := v.(map[string]any)
	if !ok {
		return "", nil
	}
	effort := strings.ToLower(strings.TrimSpace(asString(rm["effort"])))
	if effort == "" {
		return "", nil
	}
	if provider.DisablesThinking(effort) {
		return "off", nil
	}
	if !provider.IsUpstreamEffort(effort) {
		return "", fmt.Errorf(
			"reasoning.effort = %q 不被支持（可用：%s）",
			effort, strings.Join(provider.UpstreamEfforts(), "/"))
	}
	return effort, nil
}
