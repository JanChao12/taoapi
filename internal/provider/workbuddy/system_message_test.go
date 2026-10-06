package workbuddy

import (
	"encoding/json"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// ─────────────────────────────────────────────────────────────
// ensureSystemFirst 的护栏测试（2026-10-06）
//
// 🔴 背景（实测）：国际版要求首条消息是 system，否则返回
//
//	HTTP 400 {"code":11-128,"msg":"first message is not system prompt"}
//
// 国内版没有这个约束。所以补 system 只在国际版生效。
// ─────────────────────────────────────────────────────────────

// TestEnsureSystemFirstInserts 守：首条不是 system 时插入一条。
func TestEnsureSystemFirstInserts(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	ensureSystemFirst(obj)

	list, _ := obj["messages"].([]any)
	if len(list) != 2 {
		t.Fatalf("插入后应有 2 条消息，实际 %d", len(list))
	}
	first, _ := list[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Errorf("首条 role = %q，期望 system", role)
	}
	// 原来的 user 消息必须还在（且顺序不变）
	second, _ := list[1].(map[string]any)
	if role, _ := second["role"].(string); role != "user" {
		t.Errorf("第二条 role = %q，期望 user（原消息不能被顶掉）", role)
	}
	if c, _ := second["content"].(string); c != "hi" {
		t.Errorf("原消息内容被改动: %v", second["content"])
	}
}

// TestEnsureSystemFirstKeepsExisting 守：首条已是 system 时**不动**。
//
// 为什么要测：客户端明确写了自己的 system（可能含重要指令），
// 我们再加一条会把它挤到第二位 —— 而且改变了客户端的行为。
func TestEnsureSystemFirstKeepsExisting(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "Be terse."},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	ensureSystemFirst(obj)

	list, _ := obj["messages"].([]any)
	if len(list) != 2 {
		t.Fatalf("不该插入新消息，实际 %d 条", len(list))
	}
	first, _ := list[0].(map[string]any)
	if c, _ := first["content"].(string); c != "Be terse." {
		t.Errorf("客户端自己的 system 被改动了: %v", first["content"])
	}
}

// TestEnsureSystemFirstHandlesMissingMessages 守边界：没有 messages 时不崩。
//
// 这种请求本来就无效，交给上游报错即可 —— 但**不能 panic**。
func TestEnsureSystemFirstHandlesMissingMessages(t *testing.T) {
	cases := []map[string]any{
		{},                           // 没有 messages
		{"messages": nil},            // nil
		{"messages": []any{}},        // 空数组
		{"messages": "not-an-array"}, // 类型不对
	}
	for i, obj := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d 发生 panic: %v", i, r)
				}
			}()
			ensureSystemFirst(obj)
		}()
	}
}

// TestBuildChatBodyAddsSystemOnlyForIntl 守：只有国际版补 system。
//
// 这是端到端验证（走真实的 buildChatBody），比单测 ensureSystemFirst
// 更能说明"接进流程里生效了"。
func TestBuildChatBodyAddsSystemOnlyForIntl(t *testing.T) {
	rawBody := []byte(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	// 国际版：应补 system
	intlProv := NewProviderFor(NewClient(), testCred(), PlatformIntl)
	out, err := intlProv.buildChatBody(provider.ChatRequest{RawBody: rawBody, Model: "glm-5.3", Stream: true})
	if err != nil {
		t.Fatalf("国际版构建失败: %v", err)
	}
	if !firstRoleIs(out, "system") {
		t.Errorf("国际版请求体首条不是 system —— 上游会回 400\nbody=%s", out)
	}

	// 国内版：不该补（保持原样）
	cnProv := NewProviderFor(NewClient(), testCred(), PlatformCN)
	out2, err := cnProv.buildChatBody(provider.ChatRequest{RawBody: rawBody, Model: "glm-5.3", Stream: true})
	if err != nil {
		t.Fatalf("国内版构建失败: %v", err)
	}
	if firstRoleIs(out2, "system") {
		t.Errorf("国内版不该补 system（会改变原有行为）\nbody=%s", out2)
	}
}

// firstRoleIs 判断请求体的首条消息角色。
func firstRoleIs(body []byte, want string) bool {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	list, _ := m["messages"].([]any)
	if len(list) == 0 {
		return false
	}
	first, _ := list[0].(map[string]any)
	role, _ := first["role"].(string)
	return role == want
}
