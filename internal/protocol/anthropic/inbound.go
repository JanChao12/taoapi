package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件实现入站翻译：Anthropic Messages 请求 → OpenAI Chat 形状。
//
// 🔴 为什么产出的是 **OpenAI Chat 形状的原始字节**而不是自定义结构体：
//
//	现有整条链路（provider.buildChatBody → 上游）吃的就是
//	provider.ChatRequest.RawBody（OpenAI Chat 形状的 JSON）。翻译成同一形状
//	意味着 provider 层、换号调度、额度、记账【一行都不用改】——
//	这正是第 57 轮交接文档 §6.2 定的架构。
//
// ⚠️ 与 OpenAI 入站路径的一个关键差别：OpenAI 路径把客户端原始字节
//	【原样透传】只改 model（因为有损往返会丢字段）。Anthropic 路径做不到
//	—— 形状本来就不同，必须真正转换。因此这里的转换要尽可能保守：
//	只输出上游认识的字段，不认识的显式报错（委托方第三条边界）。

// Inbound 是翻译后的入站请求。
type Inbound struct {
	// RawBody 是 OpenAI Chat 形状的请求体（model 仍为客户端原名，
	// 由调用方用 router 解析后改写）。
	RawBody []byte

	// Model 客户端请求的模型名（原始，可能命中别名）。
	Model string

	// Stream 客户端是否要求流式。
	Stream bool

	// ReasoningEffort 由 thinking 字段翻译来的思考档位。
	//
	// ⚠️ 刻意【不】写进 RawBody：provider.buildChatBody 会用
	//	ChatRequest.ReasoningEffort 覆盖该字段，写两处反而容易不一致。
	//	空串表示"未指定"，由 provider 注入模型默认档。
	ReasoningEffort string

	// MaxTokens 客户端要求的输出上限（已含默认值）。
	MaxTokens int

	// HasTools 请求里是否带工具定义（流式收尾时用于判定 stop_reason）。
	HasTools bool
}

// Convert 把 Anthropic Messages 请求体翻译为 OpenAI Chat 形状。
//
// 失败一律返回 error，调用方应转成 invalid_request_error 返回客户端 ——
// 绝不"尽力而为"地丢字段（委托方 2026-10-09 明确要求）。
func Convert(body []byte) (*Inbound, error) {
	// 用 UseNumber 避免大整数被 float64 精度截断（tool input 里的 id 等）。
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

	out := map[string]any{
		"model":  model,
		"stream": asBool(in["stream"]),
	}

	// ── system：顶层字段 → 首条 system 消息 ──
	//
	// 🔴 Anthropic 的 system 是**顶层字段**（string 或 block 数组），
	//	OpenAI 把它放在 messages 里。这是 6 个翻译点里的第一个。
	var msgs []any
	if sys := flattenText(in["system"]); strings.TrimSpace(sys) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	// ── messages ──
	rawMsgs, ok := in["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		return nil, fmt.Errorf("`messages` 必填且必须是非空数组")
	}
	for i, m := range rawMsgs {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d] 必须是对象", i)
		}
		role := strings.ToLower(strings.TrimSpace(asString(mm["role"])))
		converted, err := convertMessage(role, mm["content"], i)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	// ── max_tokens：Anthropic 必填，缺失时给保守默认（不报错）──
	//
	// 🔴 为什么不报错：用户发的是普通对话请求，不该因为客户端漏发一个
	//	字段被拒。规范说必填，但实测有客户端不发。
	maxTokens := DefaultMaxTokens
	if n, has := asInt(in["max_tokens"]); has && n > 0 {
		maxTokens = n
	}
	out["max_tokens"] = maxTokens

	// ── 采样参数 ──
	if f, ok := asFloat(in["temperature"]); ok {
		out["temperature"] = f
	}
	if f, ok := asFloat(in["top_p"]); ok {
		out["top_p"] = f
	}
	// top_k 上游可能不认识；未知字段会被忽略，透传无害（实测上游对
	// 未知字段宽松）。保留它比丢掉更接近客户端意图。
	if n, ok := asInt(in["top_k"]); ok && n > 0 {
		out["top_k"] = n
	}

	// ── stop_sequences → stop ──
	if stop, ok := in["stop_sequences"].([]any); ok && len(stop) > 0 {
		out["stop"] = stop
	}

	// ── tools：input_schema → function.parameters ──
	hasTools := false
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		converted, err := convertTools(tools)
		if err != nil {
			return nil, err
		}
		if len(converted) > 0 {
			out["tools"] = converted
			hasTools = true
		}
	}

	// ── tool_choice ──
	if tc, err := convertToolChoice(in["tool_choice"]); err != nil {
		return nil, err
	} else if tc != nil {
		out["tool_choice"] = tc
	}

	// ── thinking → reasoning_effort ──
	effort, err := convertThinking(in["thinking"], in["output_config"])
	if err != nil {
		return nil, err
	}

	// ── metadata.user_id → user ──
	if mm, ok := in["metadata"].(map[string]any); ok {
		if u := strings.TrimSpace(asString(mm["user_id"])); u != "" {
			out["user"] = u
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
		MaxTokens:       maxTokens,
		HasTools:        hasTools,
	}, nil
}

// convertMessage 转换单条 Anthropic 消息，可能产出**多条** Chat 消息。
//
// 🔴 核心规则（参考实现的做法，很干净）：
//
//	assistant 里的 tool_use 块   → Chat 的 tool_calls（同一个 assistant 消息）
//	user 里的 tool_result 块     → 独立成 role=tool 消息
//	⚠️ assistant 消息必须排在 tool 结果【之前】——
//	   顺序错了上游会报错（Chat 规范要求 tool 结果紧跟其调用）。
//
// 因此这里把 assistant 消息用 append([]any{msg}, out...) 插到最前面。
func convertMessage(role string, content any, idx int) ([]any, error) {
	// content 允许纯字符串简写
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": normalizeRole(role), "content": s}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("messages[%d].content 必须是字符串或 block 数组", idx)
	}

	var (
		out       []any
		texts     []string
		imgBlocks []any
		toolCalls []any
	)

	for bi, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d].content[%d] 必须是对象", idx, bi)
		}
		switch strings.ToLower(asString(bm["type"])) {
		case "text":
			if t := asString(bm["text"]); t != "" {
				texts = append(texts, t)
			}

		case "image":
			url, err := imageURL(bm)
			if err != nil {
				return nil, fmt.Errorf("messages[%d].content[%d]: %w", idx, bi, err)
			}
			if url != "" {
				imgBlocks = append(imgBlocks, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}

		case "tool_use":
			args := bm["input"]
			if args == nil {
				args = map[string]any{}
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				argsJSON = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   firstNonEmpty(asString(bm["id"]), "call_"+randSuffix()),
				"type": "function",
				"function": map[string]any{
					"name":      asString(bm["name"]),
					"arguments": string(argsJSON),
				},
			})

		case "tool_result":
			callID := firstNonEmpty(asString(bm["tool_use_id"]), asString(bm["tool_call_id"]))
			if callID == "" {
				// 🔴 不能静默造一个 id：上游会用 tool_call_id 关联到
				// 具体的 tool_calls 条目，凭空造的 id 匹配不上，
				// 结果是"看起来成功但模型拿不到工具结果"。
				return nil, fmt.Errorf(
					"messages[%d].content[%d]: tool_result 缺少 tool_use_id（无法关联到工具调用）", idx, bi)
			}
			res := flattenText(bm["content"])
			if strings.TrimSpace(res) == "" {
				res = stringify(bm["content"])
			}
			if asBool(bm["is_error"]) {
				res = "ERROR: " + res
			}
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": callID, "content": res,
			})

		case "thinking", "redacted_thinking":
			// 历史思考块【有意丢弃】：上游不接受历史 thinking 内容，
			// 且它对后续推理没有信息增量。这不属于"静默丢字段"——
			// 思考是过程性内容，不是用户数据（参考实现同样处理）。

		case "document":
			// 🔴 显式报错而不是丢弃（委托方第三条边界）。
			return nil, fmt.Errorf(
				"messages[%d].content[%d]: 不支持 document（PDF）块 —— "+
					"本服务上游只接受文本与图片输入，静默丢弃会让模型看不到你上传的文档", idx, bi)

		default:
			// 🔴 未知块类型一律显式报错。静默跳过是"错误的成功"：
			// 用户以为内容发出去了，实际模型没看到。
			return nil, fmt.Errorf(
				"messages[%d].content[%d]: 不支持的 content block 类型 %q",
				idx, bi, asString(bm["type"]))
		}
	}

	base := normalizeRole(role)

	if len(toolCalls) > 0 {
		// 工具调用与文本可以并存（模型边说边调工具）
		msg := map[string]any{"role": "assistant", "tool_calls": toolCalls}
		msg["content"] = strings.Join(texts, "\n")
		// assistant 必须排在 tool 结果之前
		out = append([]any{msg}, out...)
	} else if len(texts) > 0 || len(imgBlocks) > 0 {
		var c any
		if len(imgBlocks) > 0 {
			mixed := make([]any, 0, len(texts)+len(imgBlocks))
			for _, t := range texts {
				mixed = append(mixed, map[string]any{"type": "text", "text": t})
			}
			mixed = append(mixed, imgBlocks...)
			c = mixed
		} else {
			c = strings.Join(texts, "\n")
		}
		out = append([]any{map[string]any{"role": base, "content": c}}, out...)
	}
	return out, nil
}

// normalizeRole 把 Anthropic 角色归一为 Chat 角色。
//
// Anthropic 只有 user/assistant；任何其它值（含缺失）按 user 处理。
func normalizeRole(role string) string {
	if strings.EqualFold(role, "assistant") {
		return "assistant"
	}
	return "user"
}

// imageURL 从 image block 提取可用的 URL。
//
// 支持两种 source：
//   - source.type=url    → 直链
//   - source.type=base64 → 内联 data URI
//
// 🔴 不支持的 source 类型（如未来的 file 引用）显式报错 ——
// 静默返回空串会让模型收到一条"没有图片"的消息。
func imageURL(block map[string]any) (string, error) {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return "", fmt.Errorf("image 块缺少 source")
	}
	typ := strings.ToLower(strings.TrimSpace(asString(src["type"])))
	switch typ {
	case "url":
		u := strings.TrimSpace(asString(src["url"]))
		if u == "" {
			return "", fmt.Errorf("image.source.type=url 但 url 为空")
		}
		return u, nil
	case "base64", "":
		data := asString(src["data"])
		if data == "" {
			return "", fmt.Errorf("image.source.type=base64 但 data 为空")
		}
		media := firstNonEmpty(asString(src["media_type"]), "image/png")
		return "data:" + media + ";base64," + data, nil
	}
	return "", fmt.Errorf("不支持的 image.source.type %q", typ)
}

// convertTools 把 Anthropic 工具定义翻译为 Chat 形状。
//
// 🔴 服务端工具（web_search_20250305 / computer_20250124 等）**显式报错**。
//
//	它们没有 Chat 等价物 —— 上游不会替你执行搜索。
//	静默跳过会让客户端以为工具可用，然后一直等一个永远不会来的 tool_use。
func convertTools(tools []any) ([]any, error) {
	converted := make([]any, 0, len(tools))
	for i, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d] 必须是对象", i)
		}
		typ := strings.TrimSpace(asString(tm["type"]))
		// 自定义工具（含 type 缺失或 "custom"）必须带 input_schema
		isServerTool := typ != "" && !strings.HasPrefix(typ, "custom") && tm["input_schema"] == nil
		if isServerTool {
			return nil, fmt.Errorf(
				"tools[%d]: 不支持服务端工具 type=%q（name=%q）—— "+
					"本服务无法代为执行搜索/计算机操作，静默跳过会让客户端一直等一个不会到来的工具调用",
				i, typ, asString(tm["name"]))
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
				"parameters":  orEmptyObject(tm["input_schema"]),
			},
		})
	}
	return converted, nil
}

// convertToolChoice 把 Anthropic tool_choice 映射为 Chat tool_choice。
//
//	auto → "auto"
//	any  → "required"
//	none → "none"
//	tool → **显式报错**（见下）
//
// 🔴 为什么 {"type":"tool","name":"x"} 不翻译而要报错：
//
//	· 上游要求 tool_choice 是【字符串】，传对象直接 400（code=11101，
//	  见 docs/upstream-contract.md §九）。
//	· 最省事的"修法"是把它降级成 "required"，但那是**改变语义**：
//	  客户端要求"必须调用 x"，变成"必须调用某个工具"，模型可能去调 y。
//	  这属于委托方明令禁止的"静默改变行为"。
//	⇒ 宁可明确告诉用户"本服务不支持指定工具"，也不要假装成功。
func convertToolChoice(v any) (any, error) {
	if v == nil || isEmptyValue(v) {
		return nil, nil
	}
	tc, ok := v.(map[string]any)
	if !ok {
		// 宽容：已经是字符串就直接用（部分客户端这么发）
		if s, ok := v.(string); ok {
			return s, nil
		}
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(asString(tc["type"]))) {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		return nil, fmt.Errorf(
			"不支持 tool_choice.type=\"tool\"（强制指定某个工具）：" +
				"上游只接受字符串形式的 tool_choice，降级为 required 会改变语义（可能调用别的工具）")
	}
	// 未知取值：不拦，交给上游判定（我们不做比上游更严的校验）
	return nil, nil
}

// convertThinking 把 Anthropic 的 thinking / output_config 翻译为思考档位。
//
// 取值来源（两条，优先级从高到低）：
//
//  1. **output_config.effort** —— DSH 真正用来表达档位的字段
//  2. thinking.type —— 只表达"开/关"
//
// 🔴 为什么必须读 output_config（2026-10-09 从 DSH 的 app.asar 提取的真实契约）：
//
//	DSH 的 Anthropic 适配器发的是：
//	  thinking:      { type: effort==="off" ? "disabled" : "enabled" }
//	  output_config: { effort }            ← effort ∈ off/low/high/max
//
//	**档位值只在 output_config.effort 里**，thinking.type 只区分开关。
//	只读 thinking.type 会把 low/high/max 全部抹平成一个默认档 ——
//	用户在 DSH 里选 "max" 却拿到 high，属于静默改变行为。
//
// 映射表：
//
//	output_config.effort = minimal/low/medium/high/xhigh/max/ultra → 原样透传
//	output_config.effort = off / none                             → "off"
//	                                 （provider 会翻译成「不传该字段」——
//	                                  上游对字面值 off 返回 400 code=11150）
//	thinking.type = disabled                                      → "off"
//	thinking.type = enabled / adaptive（且无 output_config）       → ""
//	                                                                 （交由 provider 注入默认档）
//
// ⚠️ 未知档位值**显式报错**（委托方第 3 条边界）：与其把
//
//	"veryhigh" 这类值发上去换一个 400，不如当场说清楚是哪个值不被支持。
//
// 🔴 为什么不按 budget_tokens 映射档位（如 >8000 → "max"）：
//
//	那是**编造**的对应关系。上游的档位与 Anthropic 的 token 预算是
//	两套完全不同的机制，没有任何实测依据能把 N 个 token 映射成某个档位。
func convertThinking(thinking, outputConfig any) (string, error) {
	// ① 优先取 output_config.effort（DSH 的真实档位来源）
	if oc, ok := outputConfig.(map[string]any); ok {
		raw := strings.ToLower(strings.TrimSpace(asString(oc["effort"])))
		if raw != "" {
			if provider.DisablesThinking(raw) {
				return "off", nil
			}
			if !validAnthropicEffort(raw) {
				return "", fmt.Errorf(
					"output_config.effort = %q 不被支持（可用：%s）",
					raw, strings.Join(provider.UpstreamEfforts(), "/"))
			}
			return raw, nil
		}
	}

	// ② 退回 thinking.type（只表达开关）
	if thinking == nil || isEmptyValue(thinking) {
		return "", nil
	}
	tm, ok := thinking.(map[string]any)
	if !ok {
		return "", nil
	}
	switch strings.ToLower(strings.TrimSpace(asString(tm["type"]))) {
	case "disabled":
		return "off", nil
	case "enabled", "adaptive", "auto", "":
		return "", nil
	}
	return "", nil
}

// validAnthropicEffort 判定上游是否接受某档位。
//
// 🔴 2026-10-09 起改为转发 provider 层的**唯一权威定义**。
//
//	起因：`off` 曾被本项目当作合法档位对外声明，而直连实测上游对它返回
//	HTTP 400 code=11150。根因是"哪些档位上游真的接受"这个事实存在
//	多份副本 —— 白名单在 workbuddy/models.go、翻译逻辑在 workbuddy/chat.go，
//	而我写本包时又抄了第三份。修一处漏一处。
//
//	现在唯一来源是 provider.IsUpstreamEffort / provider.DisablesThinking，
//	本包只做「协议字段 → 语义档位」的翻译，不自己维护白名单。
func validAnthropicEffort(e string) bool {
	return provider.IsUpstreamEffort(e)
}

// CountTokens 估算请求的输入 token 数（POST /v1/messages/count_tokens）。
//
// 🔴 为什么必须实现它：Claude Code 等客户端用它做上下文预算 ——
//
//	没有这个端点，客户端会以为服务不支持而放弃/降级。
//
// 本项目无 tokenizer 依赖（纯标准库、零第三方），采用**字符启发式**：
// 中文 1 字 ≈ 1 token，其余 4 字符 ≈ 1 token，再对消息条数与内容块数取
// 结构开销下限。数值刻意偏**保守（略高）**：客户端据此预留上下文，
// 估高了只是少用一点窗口，估低了会直接把请求撑爆。
func CountTokens(body []byte) (int64, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var in map[string]any
	if err := dec.Decode(&in); err != nil {
		return 0, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}

	var cjk, other, blocks int
	countText := func(s string) {
		for _, r := range s {
			// 0x2E80 起是 CJK 部首/汉字/全角符号：约 1 字 1 token
			if r > 0x2E80 {
				cjk++
			} else {
				other++
			}
		}
	}

	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			countText(t)
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			if s, ok := t["type"].(string); ok {
				// 只统计内容类块，避免把 name/id 等短标识符重复计入
				switch s {
				case "text", "tool_result", "input_text", "output_text", "image":
					blocks++
				}
			}
			// 只递归有文本语义的键，避免把 id/role 也算进去
			for _, k := range []string{"text", "content", "input", "system", "tools", "name", "description", "input_schema"} {
				if x, ok := t[k]; ok {
					walk(x)
				}
			}
		}
	}
	walk(in["system"])
	walk(in["messages"])
	walk(in["tools"])

	tokens := cjk + other/4 + 1
	if min := blocks * 2; tokens < min { // 每个内容块至少有结构开销
		tokens = min
	}
	if msgs, ok := in["messages"].([]any); ok {
		if min := len(msgs) * 2; tokens < min {
			tokens = min
		}
	}
	return int64(tokens), nil
}
