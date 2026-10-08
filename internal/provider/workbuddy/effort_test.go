package workbuddy

import (
	"encoding/json"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// 本文件守「思考档位」的翻译规则。
//
// 🔴 为什么需要它（2026-10-09 实测发现）：`/v1/models` 把 "off" 作为合法档位
// 对外声明，而 buildChatBody 会把客户端传来的档位**原样发上去** ——
// 直连实测结果是：
//
//	reasoning_effort:"off"  → HTTP 400 code=11150
//	                          "the reasoning effort value is not supported
//	                           by the current model"
//	reasoning_effort:"none" → HTTP 200，但**仍然产出思考**
//	【不传该字段】           → HTTP 200，思考分片 0
//
// ⇒ 上游没有"关闭思考"的档位取值；唯一能关掉的方式是**不传该字段**。
// 这正是 DSH 侧的做法（配置写 `off: null` = 留空什么都不发送）。
//
// DSH 没踩到这个坑，只是因为它的 off 留空、根本不发这个值；
// 但任何真的选「关闭思考」的客户端都会吃 400。属于既存缺陷。

// buildBody 走真实的 buildChatBody，返回解析后的请求体。
func buildBody(t *testing.T, model, effort string) map[string]any {
	t.Helper()
	p := &Provider{}
	out, err := p.buildChatBody(provider.ChatRequest{
		RawBody:         []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
		Model:           model,
		Stream:          true,
		ReasoningEffort: effort,
	})
	if err != nil {
		t.Fatalf("buildChatBody 失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("buildChatBody 输出不是合法 JSON: %v", err)
	}
	return got
}

// TestBuildChatBodyOffOmitsEffort 守：off / none 必须**删掉**该字段。
//
// 🔴 反向对照过的点：改成 `obj["reasoning_effort"] = "off"` 后本测试变红，
// 且真实上游会返回 400 code=11150。
func TestBuildChatBodyOffOmitsEffort(t *testing.T) {
	for _, effort := range []string{"off", "none"} {
		got := buildBody(t, "deepseek-v4.1-flash", effort)
		if v, has := got["reasoning_effort"]; has {
			t.Errorf("reasoning_effort=%q 时仍发送了 reasoning_effort=%v —— "+
				"实测上游对 off 返回 400 code=11150，唯一能关闭思考的方式是不传该字段",
				effort, v)
		}
	}
}

// TestBuildChatBodyOffRemovesClientValue 守：客户端在原始请求体里
// 带了 reasoning_effort:"off" 时也必须被删掉。
//
// 🔴 这条是真实路径：handleChat 会把【客户端原始字节】透传下来，
// 所以 obj 里可能已经有这个字段 —— 只在 struct 层面处理是不够的。
func TestBuildChatBodyOffRemovesClientValue(t *testing.T) {
	p := &Provider{}
	out, err := p.buildChatBody(provider.ChatRequest{
		RawBody: []byte(`{"model":"x","reasoning_effort":"off",` +
			`"messages":[{"role":"user","content":"hi"}]}`),
		Model:           "deepseek-v4.1-flash",
		Stream:          true,
		ReasoningEffort: "off",
	})
	if err != nil {
		t.Fatalf("buildChatBody 失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if v, has := got["reasoning_effort"]; has {
		t.Errorf("原始请求体里的 reasoning_effort=%v 未被删除 —— "+
			"客户端真实路径是透传原始字节，必须在 obj 上删除", v)
	}
}

// TestBuildChatBodyOffDoesNotFallBackToDefault 守：要求关闭思考时
// **不能**回落默认档。
//
// 🔴 这是最容易犯的错：把 "off" 当成"未指定"处理，于是注入 high ——
// 与用户意图**完全相反**（用户明确要求不思考，却得到最耗时的思考档）。
func TestBuildChatBodyOffDoesNotFallBackToDefault(t *testing.T) {
	got := buildBody(t, "deepseek-v4.1-flash", "off")
	if v, has := got["reasoning_effort"]; has {
		t.Errorf("要求关闭思考却注入了档位 %v —— "+
			"off 不能被当成「未指定」而回落默认 high", v)
	}
}

// TestBuildChatBodyInjectsDefaultWhenAbsent 守：未指定时仍注入默认 high。
//
// 依据：实测 deepseek 系「不传 = 完全不思考」，服务端必须显式注入，
// 否则用户以为在用思考模型。
func TestBuildChatBodyInjectsDefaultWhenAbsent(t *testing.T) {
	got := buildBody(t, "deepseek-v4.1-flash", "")
	if got["reasoning_effort"] != "high" {
		t.Errorf("未指定时 reasoning_effort = %v，期望 high（委托人要求默认 high）",
			got["reasoning_effort"])
	}
}

// TestBuildChatBodyPreservesValidEffort 守：合法档位原样透传。
func TestBuildChatBodyPreservesValidEffort(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		got := buildBody(t, "space-bunny", effort)
		if got["reasoning_effort"] != effort {
			t.Errorf("客户端档位 %q 被改写为 %v，应原样透传",
				effort, got["reasoning_effort"])
		}
	}
}

// TestModelsNeverAdvertiseOffAsUpstreamValue 守一个**契约一致性**要求。
//
// 🔴 问题：/v1/models 的 reasoning_efforts 里含 "off"，但上游**不接受**
// 这个字面值（400）。客户端照列表发就会失败。
//
// 现状与取舍：保留 "off" 在列表里是**有意**的 —— 它是给客户端的
// "关闭思考"语义档，服务端负责把它翻译成"不传字段"（见 buildChatBody）。
// 本测试固化这个契约：只要列表里出现 "off"，服务端就必须有对应的翻译。
//
// ⚠️ 反向断言：确保没有人把 "off" 加进 validUpstreamEfforts ——
// 那会让 cleanEfforts 认为它是上游认的值，从而把它透传上去（400）。
func TestModelsNeverAdvertiseOffAsUpstreamValue(t *testing.T) {
	if validUpstreamEfforts["off"] {
		t.Error("validUpstreamEfforts 含 \"off\" —— 上游实测不接受该值（400 code=11150）；" +
			"它只能作为对外语义档，由 buildChatBody 翻译成「不传字段」")
	}
	if validUpstreamEfforts["none"] {
		t.Error("validUpstreamEfforts 含 \"none\" —— 实测它虽被接受但仍产出思考，" +
			"不能当成「关闭思考」的合法值")
	}
}
