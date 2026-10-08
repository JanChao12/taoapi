package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// chatMaxRequestBytes 上报给上游的请求体上限。
const chatMaxRequestBytes = 8 << 20

// Chat 实现 provider.Provider：向上游发起对话并逐事件回调。
//
// ⚠️ 核心约束（实测）：上游【强制 stream:true】，发 false 会返回 400。
// 因此无论客户端要流式还是非流式，我们都向上游发流式；
// 非流式由协议层消费同一批事件后聚合，见 protocol/openai/chat.go。
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest, emit func(provider.Event) error) error {
	body, err := p.buildChatBody(req)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.ChatURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造对话请求失败: %w", err)
	}
	applyChatHeaders(httpReq, p.cred, newMessageID())

	resp, err := p.client.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("请求上游对话失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 读一小段用于诊断（不读全量，避免异常响应撑内存）
		snippetBuf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       snippet(snippetBuf),
			Op:         "chat",
		}
	}

	return parseSSEStream(resp.Body, emit)
}

// buildChatBody 把 canonical 请求改写为上游认的请求体。
//
// 改写规则（全部来自实测，见 docs/upstream-contract.md）：
//  1. model 去掉渠道前缀
//  2. stream 强制为 true（上游拒绝非流式）
//  3. max_completion_tokens → max_tokens（上游只认后者）
//  4. developer 角色 → system（上游白名单不含 developer，会触发内容过滤误杀）
//  5. stream_options.include_usage = true（末帧才返回 usage）
//  6. reasoning_effort 显式注入默认值（deepseek 系"不传 = 完全不思考"）
func (p *Provider) buildChatBody(req provider.ChatRequest) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(req.RawBody, &obj); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}

	// 1) 模型名去前缀
	obj["model"] = req.Model

	// 2) 强制 stream
	obj["stream"] = true

	// 3) max_completion_tokens → max_tokens
	normalizeMaxTokens(obj)

	// 4) developer → system
	normalizeRoles(obj)

	// 4b) 国际版：确保首条消息是 system。
	//
	// 🔴 实测（2026-10-06）：国际版无 system 首条会返回
	//	HTTP 400 {"code":11-128,"msg":"first message is not system prompt"}，
	//	国内版没有这个约束。
	//
	// 处理方式：**补一条 system 而不是报错给用户** ——
	// 用户发的是普通的对话请求，不该因为"没写 system"就被拒。
	// 补的内容保持中性，不引入任何行为约束。
	if p.platform.NeedsSystemMessage() {
		ensureSystemFirst(obj)
	}

	// 5) 确保末帧带 usage
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}

	// 6) 思考档位：客户端指定就用，否则注入模型默认档
	//
	// 🔴 "off" 必须翻译成【不发这个字段】，不能原样透传（2026-10-09 实测）：
	//
	//	直连实测（模型 deepseek-v4.1-flash，经本服务打上游）：
	//	  reasoning_effort:"off"   → HTTP 400 code=11150
	//	                             "the reasoning effort value is not
	//	                              supported by the current model"
	//	  reasoning_effort:"none"  → HTTP 200，但**仍然产出思考**
	//	  【不传该字段】            → HTTP 200，思考分片 0（fixture 铁证）
	//
	//	⇒ 上游根本没有"关闭思考"的档位取值：唯一能关掉思考的方式是**不传**。
	//	  这也正是 DSH 侧的做法（它的配置写 `off: null`，即"留空什么都不发送"，
	//	  见 DSH 官方文档 providers.zh.md 第 134 行）。
	//
	//	⚠️ 此前 `/v1/models` 把 "off" 作为合法档位对外声明，而这里会把
	//	  "off" 原样发上去 ⇒ 任何客户端真的选「关闭思考」都会吃 400。
	//	  DSH 没踩到只是因为它的 `off` 留空、根本不发这个值。
	//
	//	⚠️ 语义边界：对 ModeSwitch 模型（deepseek 系）不传 = 完全不思考，
	//	  与用户意图一致。对 canDisableThinking=false 的模型（space-bunny）
	//	  上游本就无法关闭思考，不传只是回到默认档 —— 这是能做到的最好结果，
	//	  比发一个必然 400 的值诚实。
	effort := req.ReasoningEffort
	switch {
	case provider.DisablesThinking(effort):
		// 显式要求关闭思考：删掉字段（**不要**回落默认档，那与用户意图相反）
		delete(obj, "reasoning_effort")
	default:
		if effort == "" {
			effort = p.defaultEffortFor(req.Model)
		}
		if effort != "" {
			obj["reasoning_effort"] = effort
		}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}
	if len(out) > chatMaxRequestBytes {
		return nil, fmt.Errorf("请求体过大: %d 字节（上限 %d）", len(out), chatMaxRequestBytes)
	}
	return out, nil
}

// ensureSystemFirst 保证 messages 的第一条是 system（国际版要求）。
//
// 行为：
//   - 首条已经是 system → 不动（尊重客户端的选择）
//   - 首条不是 system   → **在前面插入**一条中性的 system
//
// 🔴 为什么不直接返回 400 让用户改：
//
//	用户发的是标准 OpenAI 形状的请求，绝大多数客户端都不会主动写
//	system 首条（国内版也不需要）。把它当错误会让"换到国际版就报错"，
//	而错误信息（"first message is not system prompt"）对用户毫无指导意义。
//
// 🔴 为什么不追加到末尾而要插到最前：
//
//	上游的判据是"**first** message is system"，放别处无效。
//
// 内容刻意保持中性 —— 只声明身份，不添加任何指令，
// 免得改变了用户原本的对话行为。
func ensureSystemFirst(obj map[string]any) {
	raw, ok := obj["messages"]
	if !ok {
		return // 没有 messages 字段：交给上游自己报错（那种请求本来就无效）
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	// 首条已是 system 就不动
	if first, ok := list[0].(map[string]any); ok {
		if role, _ := first["role"].(string); role == "system" {
			return
		}
	}
	sys := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{sys}, list...)
}

// normalizeMaxTokens 把新别名翻译为上游认的 max_tokens。
//
// 规则：显式 max_tokens 优先；别名一律删除（无论是否翻译），
// 因为上游 Go struct 对未知字段宽松，留着只增体积与排障噪音。
func normalizeMaxTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式优先
	}
	if v, ok := alias.(float64); ok && v > 0 && v == float64(int64(v)) {
		obj["max_tokens"] = int64(v)
	}
}

// normalizeRoles 把 developer 角色归一为 system。
//
// 实测：上游对 messages 的 role 做白名单校验，developer 不在其中，
// 且会触发内容过滤误杀。DSH 等新客户端会发 developer。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if r, _ := mm["role"].(string); r == "developer" {
				mm["role"] = "system"
			}
		}
	}
}

// defaultEffortFor 返回某模型的默认思考档位。
//
// 依据委托人要求「默认思考 high」，且实测 deepseek 系"不传 = 完全不思考"，
// 所以必须显式注入，不能依赖上游默认。
func (p *Provider) defaultEffortFor(model string) string {
	if known, ok := probedReasoning[model]; ok {
		return known.Default
	}
	return defaultReasoningEffort
}
