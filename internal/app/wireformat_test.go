package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
)

// 本文件守「重构没有改变已上线的对外字节」。
//
// 🔴 为什么必须有它（2026-10-09 加两种协议时）：
//
//	为了让 handleChat 支持三种协议，我把它重构成了
//	handleChatWith(codec) + 协议编解码器。**OpenAI Chat 路径的代码被搬动了** ——
//	搬动就可能改变行为，而它已经被委托人日常使用、且是 DSH 的默认协议。
//
//	单测全绿不代表字节没变（第 17/19 轮的教训）：所以这里断言的是
//	**逐帧的线格式**，不是"没报错"。
//
// ⚠️ 这里刻意断言几个"看起来不优雅但必须保留"的行为：
//	错误帧之后仍然发 [DONE]。那是重构前就有的行为，OpenAI 客户端
//	把它当"SSE 传输结束"标记。顺手"修正"它会破坏既有客户端。

// TestOpenAIStreamWireFormatUnchanged 守 OpenAI 流式线格式未被重构改变。
func TestOpenAIStreamWireFormatUnchanged(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}],"stream":true}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/chat/completions", body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	// ① 每帧必须是 `data: {...}\n\n`，且**不带** event: 行
	//
	// 🔴 OpenAI 的 SSE 只有 data 行。若重构时误用了 Anthropic 的
	//	具名事件格式，客户端会解不出来。
	if strings.Contains(text, "event: ") {
		t.Error("OpenAI 流里出现了 `event: ` 行 —— " +
			"OpenAI SSE 只有 data 行，混入具名事件会破坏既有客户端")
	}

	// ② 必须以 data: [DONE] 结束
	if !strings.HasSuffix(strings.TrimRight(text, "\n"), "data: [DONE]") {
		t.Errorf("流未以 `data: [DONE]` 结束 —— OpenAI 客户端据此判定传输结束。\n尾部: %q",
			tail(text, 200))
	}

	// ③ 每个 data 帧都必须是合法 JSON（[DONE] 除外）
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("分片不是合法 JSON: %v\n内容: %s", err, payload)
		}
		// ④ 分片形状必须是 chat.completion.chunk
		if chunk["object"] != "chat.completion.chunk" {
			t.Errorf("分片 object = %v，期望 chat.completion.chunk", chunk["object"])
		}
		// ⑤ model 必须是**客户端请求的名字**（不是上游 ID）
		//
		// 🔴 这是 2026-10-05 的真实故障：回给客户端的是上游 ID，
		//	用户对不上自己请求的名字。
		if chunk["model"] != "workbuddy/space-bunny" {
			t.Errorf("分片 model = %v，期望客户端请求的名字 workbuddy/space-bunny",
				chunk["model"])
		}
		// ⑥ 必须有 choices 数组
		if _, has := chunk["choices"]; !has {
			t.Errorf("分片缺少 choices: %s", payload)
		}
	}

	// ⑦ 思考走 delta.reasoning_content（OpenAI 规范外字段，必须透传）
	var reasoning, content strings.Builder
	var finishSeen bool
	var usageFrame *map[string]any
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usageFrame = &u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		if delta != nil {
			reasoning.WriteString(asStr(delta["reasoning_content"]))
			content.WriteString(asStr(delta["content"]))
		}
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			finishSeen = true
		}
	}
	if reasoning.Len() == 0 {
		t.Error("思考增量丢失 —— 客户端将看不到思考过程")
	}
	if content.String() != "可用" {
		t.Errorf("正文 = %q，期望 可用", content.String())
	}
	if !finishSeen {
		t.Error("没有任何分片带 finish_reason")
	}

	// ⑧ usage 帧必须保留 OpenAI 字段名与思考/缓存明细
	if usageFrame == nil {
		t.Fatal("没有收到带 usage 的分片 —— 上游 fixture 明确带 usage")
	}
	u := *usageFrame
	if _, has := u["prompt_tokens"]; !has {
		t.Errorf("usage 缺 prompt_tokens（OpenAI 字段名）: %v", u)
	}
	if _, has := u["completion_tokens"]; !has {
		t.Errorf("usage 缺 completion_tokens: %v", u)
	}
	if _, has := u["input_tokens"]; has {
		t.Error("usage 里出现了 Anthropic 字段名 input_tokens —— 字段名被串了")
	}
	// 🔴 思考 token 必须一路传到客户端（2026-10-06 修过的缺口）
	if u["completion_thinking_tokens"] != float64(25) {
		t.Errorf("completion_thinking_tokens = %v，期望 25（fixture 实测值）",
			u["completion_thinking_tokens"])
	}

	// 🔴 缓存命中/未命中必须出现在对外 usage 里（2026-10-09 补的缺口）。
	//
	//	此前 protocol/openai/sse.go 的 toOpenAIUsage 没映射这两个字段，
	//	于是上游给了（fixture 实测 149/12）但客户端拿不到。
	//	注意这与面板的命中率是**两条链路**：面板走记账（早已修好），
	//	这里修的是"客户端自己从流里读"。
	if u["prompt_cache_hit_tokens"] != float64(149) {
		t.Errorf("prompt_cache_hit_tokens = %v，期望 149（fixture 实测值）—— "+
			"上游确实给了这个值，映射漏了会让客户端读不到缓存命中",
			u["prompt_cache_hit_tokens"])
	}
	if u["prompt_cache_miss_tokens"] != float64(12) {
		t.Errorf("prompt_cache_miss_tokens = %v，期望 12",
			u["prompt_cache_miss_tokens"])
	}
}

// TestOpenAIUsageCacheFieldsAreAdditiveOnly 守：补缓存字段是**纯增量**改动。
//
// 🔴 为什么必须守这一点：委托方要求「/v1/chat/completions 完全不变」。
//
//	我判断补这两个字段安全，**理由是它们都是 omitempty** ——
//	上游没报告缓存时值为 0 ⇒ 字段根本不出现 ⇒ 输出与改动前逐字节相同。
//
//	本测试把那个理由变成断言：构造一个"上游没给缓存"的 usage，
//	断言序列化结果里**不含**这两个键。若有人把 omitempty 去掉，
//	每个响应都会多出 `"prompt_cache_hit_tokens":0` —— 那就真的改变了
//	既有端点的输出，本测试会红。
func TestOpenAIUsageCacheFieldsAreAdditiveOnly(t *testing.T) {
	// 上游没报告缓存（两个字段都是 0）
	chunk := openai.BuildChunk("id", "m", 1, openai.Event{
		Type:  openai.EvUsage,
		Usage: &openai.EventUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "prompt_cache_hit_tokens") {
		t.Errorf("上游没给缓存却输出了 prompt_cache_hit_tokens —— "+
			"omitempty 被去掉了？那会改变既有端点的输出（委托方要求不变）\n%s", s)
	}
	if strings.Contains(s, "prompt_cache_miss_tokens") {
		t.Errorf("上游没给缓存却输出了 prompt_cache_miss_tokens\n%s", s)
	}

	// 上游报告了缓存 → 必须出现
	chunk2 := openai.BuildChunk("id", "m", 1, openai.Event{
		Type: openai.EvUsage,
		Usage: &openai.EventUsage{
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			PromptCacheHitTokens: 149, PromptCacheMissTokens: 12,
		},
	})
	b2, _ := json.Marshal(chunk2)
	if !strings.Contains(string(b2), `"prompt_cache_hit_tokens":149`) {
		t.Errorf("上游报告了缓存却没输出 —— 映射又漏了\n%s", b2)
	}
}

// TestOpenAINonStreamWireFormatUnchanged 守 OpenAI 非流式响应形状未被改变。
func TestOpenAINonStreamWireFormatUnchanged(t *testing.T) {
	srv, _ := newChatTestServer(t, "sse-space-bunny-max.txt")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/chat/completions", body, nil)
	defer resp.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v，期望 chat.completion", got["object"])
	}
	if got["model"] != "workbuddy/space-bunny" {
		t.Errorf("model = %v，期望客户端请求的名字", got["model"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 数 = %d，期望 1", len(choices))
	}
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	if msg == nil {
		t.Fatal("choices[0].message 缺失")
	}
	if msg["role"] != "assistant" {
		t.Errorf("message.role = %v", msg["role"])
	}
	if msg["content"] != "可用" {
		t.Errorf("message.content = %v，期望 可用", msg["content"])
	}
	// reasoning_content 是规范外字段，必须保留
	if rc, _ := msg["reasoning_content"].(string); rc == "" {
		t.Error("message.reasoning_content 丢失 —— 客户端将看不到思考过程")
	}
	if ch["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v，期望 stop", ch["finish_reason"])
	}
}

// TestOpenAIStreamErrorStillSendsDone 守一个"必须保留的既有行为"。
//
// 🔴 上游流中途失败时，OpenAI 路径发错误帧**之后仍然发 [DONE]**。
//
//	这是重构前就有的行为，刻意保留：OpenAI 客户端把 [DONE] 当作
//	"SSE 传输结束"的标记，不发它会让部分客户端一直等。
//
//	⚠️ 与 Anthropic / Responses 的取舍**不同**（那两者绝不能补
//	message_stop / response.completed），因为协议语义不同：
//	[DONE] 只表示"传输结束"，而 message_stop 表示"正常完成"。
//
//	本测试的作用是防止有人"顺手统一"三种协议的错误收尾 —— 那会
//	破坏既有 OpenAI 客户端。
func TestOpenAIStreamErrorStillSendsDone(t *testing.T) {
	// 假上游返回 200 + SSE 头，但流里没有 [DONE] → provider 会报
	// "上游流异常结束"。这个错误发生在首个事件之后，属于流中途失败。
	srv, fake := newChatTestServer(t, "sse-space-bunny-max.txt")
	// 让上游在流中断（去掉 [DONE]）：用一个不含 DONE 的样本
	fake.SSEBody = strings.ReplaceAll(fake.SSEBody, "data: [DONE]", "")

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}],"stream":true}`
	resp := postJSONWithHeaders(t, srv.URL+"/v1/chat/completions", body, nil)
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)

	// 流已经开始，所以状态码是 200（无法改）
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（流已开始，状态码改不了）", resp.StatusCode)
	}
	// 必须有错误帧告知客户端
	if !strings.Contains(text, `"error"`) {
		t.Errorf("流中断却没有错误帧。尾部: %q", tail(text, 300))
	}
	// 🔴 仍必须发 [DONE]（既有行为，见上）
	if !strings.Contains(text, "data: [DONE]") {
		t.Errorf("[DONE] 缺失 —— OpenAI 客户端会一直等传输结束标记。"+
			"⚠️ 注意：Anthropic/Responses 的错误收尾**不该**发各自的完成事件，"+
			"但 OpenAI 的 [DONE] 只表示传输结束，必须保留。\n尾部: %q", tail(text, 300))
	}
}

// tail 返回字符串尾部 n 个字符（便于错误信息定位）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
