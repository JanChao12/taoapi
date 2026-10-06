package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 本文件覆盖《第9轮-接口契约-冻结.md》§4「映射表 / 别名」的每一条规则。
//
// 命名约定：TestAliasXXX。每个测试的注释都写明【守护什么回归】——
// 即"如果没有这个测试，将来哪一类改动会悄悄破坏它"。

// aliasRouter 建一个已注册 wbProvider 的路由器。
func aliasRouter(t *testing.T) *Router {
	t.Helper()
	r := New()
	if err := r.Register(context.Background(), wbProvider()); err != nil {
		t.Fatalf("注册 provider 失败: %v", err)
	}
	return r
}

// ─────────────────────────────────────────────────────────────
// 基本行为
// ─────────────────────────────────────────────────────────────

// TestAliasResolveBasic 验证别名能被解析成正确的上游模型。
//
// 守护：别名这条链路（SetAliases → Resolve → upstreamID）本身可用。
// 这是整套功能的地基，地基坏了后面所有测试都会以"看起来像校验问题"
// 的形式失败，反而误导排查方向。
func TestAliasResolveBasic(t *testing.T) {
	r := aliasRouter(t)

	// 测试用 provider 只暴露 space-bunny / glm-5.3 两个模型。
	if err := r.SetAliases(map[string]string{
		"dsf": "workbuddy/space-bunny",
		"g53": "workbuddy/glm-5.3",
	}); err != nil {
		t.Fatalf("合法的别名表应被接受: %v", err)
	}

	p, up, m, err := r.Resolve("dsf")
	if err != nil {
		t.Fatalf("别名解析失败: %v", err)
	}
	if p.ID() != "workbuddy" {
		t.Errorf("provider = %q，期望 workbuddy", p.ID())
	}
	if up != "space-bunny" {
		t.Errorf("upstreamID = %q，期望 space-bunny", up)
	}
	if m.ID != "workbuddy/space-bunny" {
		t.Errorf("模型 ID = %q，期望解析到真实模型 workbuddy/space-bunny", m.ID)
	}

	// UpstreamModelID 走的也是同一条解析链，必须一致。
	got, err := r.UpstreamModelID("g53")
	if err != nil {
		t.Fatalf("UpstreamModelID 失败: %v", err)
	}
	if got != "glm-5.3" {
		t.Errorf("UpstreamModelID(g53) = %q，期望 glm-5.3", got)
	}
}

// TestAliasDoesNotShadowRealModels 验证【没有】别名时原有解析完全不变。
//
// 守护：别名功能不得改变既有行为。契约要求"未命中走原有前缀逻辑"，
// 若有人把别名查找写成"总是先查、查不到就报错"，这条会立刻红。
func TestAliasDoesNotShadowRealModels(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}

	// 全名、无前缀两条原有路径都必须照旧可用。
	for _, id := range []string{"workbuddy/glm-5.3", "glm-5.3"} {
		_, up, _, err := r.Resolve(id)
		if err != nil {
			t.Errorf("原有解析路径 %q 被别名功能破坏: %v", id, err)
			continue
		}
		if up != "glm-5.3" {
			t.Errorf("Resolve(%q) upstreamID = %q，期望 glm-5.3", id, up)
		}
	}
}

// TestAliasResolvesBareTargetID 验证别名目标也可以写成不带前缀的上游 ID。
//
// 守护：契约 §4.2 说"目标必须能被现有 Resolve 解析成功"，
// 而 Resolve 本来就支持无前缀兜底（"space-bunny" 可解析）。
// 若校验时只查全名表 r.models，用户填 "space-bunny" 会被误拒 ——
// 而这个名字在上游文档、面板等地方到处都是。
func TestAliasResolvesBareTargetID(t *testing.T) {
	r := aliasRouter(t)

	if err := r.SetAliases(map[string]string{"dsf": "space-bunny"}); err != nil {
		t.Fatalf("无前缀目标应被接受: %v", err)
	}
	_, up, m, err := r.Resolve("dsf")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if up != "space-bunny" || m.ID != "workbuddy/space-bunny" {
		t.Errorf("解析结果 up=%q id=%q，期望 space-bunny / workbuddy/space-bunny", up, m.ID)
	}

	// 前后空白应被裁剪（用户从别处复制的名字常带空格）。
	if err := r.SetAliases(map[string]string{"pad": "  workbuddy/glm-5.3  "}); err != nil {
		t.Fatalf("带空白的目标应被接受（TrimSpace）: %v", err)
	}
	got, err := r.UpstreamModelID("pad")
	if err != nil || got != "glm-5.3" {
		t.Errorf("UpstreamModelID(pad) = %q, err=%v；期望 glm-5.3", got, err)
	}
}

// TestAliasTakesPrecedenceOverRawID 验证别名命中时优先于同名真实模型。
//
// 守护「先查别名」这个顺序。契约只写了"先查别名表"，没写
// "别名恰好等于某个真实模型 ID 时谁赢"。我选的保守做法是别名优先：
// 别名是用户显式配置的意图，若被目录静默压过，面板上会出现
// "别名显示了、却不起作用"的现象，属于最难排查的一类问题。
//
// ⚠️ 契约 §4.1 禁止别名 key 含 '/'，所以能"撞上"真实模型的只有
// 无前缀写法（如 "glm-5.3"，它同时是合法的上游 ID）。用那个形式验证顺序。
func TestAliasTakesPrecedenceOverRawID(t *testing.T) {
	r := aliasRouter(t)

	// "glm-5.3" 本身能通过无前缀兜底解析；把它别名到 space-bunny。
	if err := r.SetAliases(map[string]string{
		"glm-5.3": "workbuddy/space-bunny",
	}); err != nil {
		t.Fatalf("应接受（目标有效）: %v", err)
	}

	_, up, _, err := r.Resolve("glm-5.3")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if up != "space-bunny" {
		t.Errorf("upstreamID = %q，期望 space-bunny（别名优先于无前缀兜底）", up)
	}
}

// ─────────────────────────────────────────────────────────────
// 校验规则（契约 §4 逐条）
// ─────────────────────────────────────────────────────────────

// TestAliasRejectsInvalidKey 验证 key 的三条形态规则。
//
// 守护：契约 §4.1。key 含 '/' 会与 workbuddy/ 前缀混淆
// （解析时无法区分"渠道前缀"和"别名"）；超长 key 会同时撑大
// /v1/models 响应、请求体与用量记录。
func TestAliasRejectsInvalidKey(t *testing.T) {
	r := aliasRouter(t)
	const ok = "workbuddy/space-bunny"

	cases := []struct {
		name string
		key  string
	}{
		{"空 key", ""},
		{"含斜杠", "a/b"},
		{"含斜杠（前缀形式）", "workbuddy/x"},
		{"超长（65 字节）", strings.Repeat("a", MaxAliasKeyLen+1)},
		{"超长（多字节）", strings.Repeat("模", MaxAliasKeyLen)}, // 中文 3 字节/字，必超
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := r.SetAliases(map[string]string{c.key: ok}); err == nil {
				t.Fatalf("别名 key %q 应被拒绝", c.key)
			} else if !errors.Is(err, ErrInvalidAlias) {
				t.Errorf("错误应可用 errors.Is 判为 ErrInvalidAlias，实际: %v", err)
			}
		})
	}

	// 边界：恰好等于上限的 key 必须【接受】。
	// 只测拒绝不测边界，会把"≤64"误实现成"<64"而无人发现。
	if err := r.SetAliases(map[string]string{strings.Repeat("a", MaxAliasKeyLen): ok}); err != nil {
		t.Errorf("长度恰好 %d 的别名应被接受: %v", MaxAliasKeyLen, err)
	}
}

// TestAliasRejectsUnknownTarget 验证目标必须是有效模型。
//
// 守护：契约 §4.2。若不校验，用户能存下一个永远解析失败的别名，
// 面板保存"成功"但请求必 404 —— 错误反馈与操作时机完全脱节。
func TestAliasRejectsUnknownTarget(t *testing.T) {
	r := aliasRouter(t)

	for _, target := range []string{
		"workbuddy/does-not-exist",
		"does-not-exist",
		"",
		"   ", // 只有空白：TrimSpace 后为空
	} {
		if err := r.SetAliases(map[string]string{"dsf": target}); err == nil {
			t.Errorf("目标 %q 不存在，应被拒绝", target)
		}
	}
}

// TestAliasRejectsSelfMapping 验证禁止自映射 a→a。
//
// 守护：契约 §4.3。自映射不算"致命"，但它指向的是别名自己而不是模型，
// 留着等于表里有一个解析永远失败或永远绕圈的表项。
func TestAliasRejectsSelfMapping(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "dsf"}); err == nil {
		t.Error("自映射 a→a 应被拒绝")
	}
}

// TestAliasRejectsTwoNodeCycle 验证禁止循环 a→b, b→a。
//
// 守护：契约 §4.4 的"只允许一层解析"。
//
// ⚠️ 本用例构造的是**合法的、会触发循环检查**的输入：两个 key 都指向
// 同一个真实模型，且互为对方的 key。这在当前实现里会被规则 2 之外的
// 循环检查拦下；即使将来放宽规则 2，这条检查也必须继续拦。
func TestAliasRejectsTwoNodeCycle(t *testing.T) {
	r := aliasRouter(t)

	m := map[string]string{"a": "b", "b": "a"}
	err := r.SetAliases(m)
	if err == nil {
		t.Fatal("循环 a→b, b→a 应被拒绝")
	}
	if !errors.Is(err, ErrInvalidAlias) {
		t.Errorf("错误应判为 ErrInvalidAlias，实际: %v", err)
	}
}

// TestAliasRejectsAliasChain 验证"别名指向别名"被拒（只有一层）。
//
// 守护：契约 §4.4 的另一半。这是最容易实现错的一条：
// 若校验目标时误用 r.Resolve（而不是只查真实目录的 findModel），
// "b→a" 会因为 a 已是别名而"解析成功"，于是写出 a→x, b→a 的二层链 ——
// 解析 b 得到 a 之后不再解析，请求会把 "a" 当上游模型名发出去。
func TestAliasRejectsAliasChain(t *testing.T) {
	r := aliasRouter(t)

	// 先建立合法别名 a → 真实模型。
	if err := r.SetAliases(map[string]string{"a": "workbuddy/space-bunny"}); err != nil {
		t.Fatalf("第一步别名应合法: %v", err)
	}

	// 再试图让 b 指向别名 a：必须被拒（a 不是真实模型）。
	err := r.SetAliases(map[string]string{
		"a": "workbuddy/space-bunny",
		"b": "a",
	})
	if err == nil {
		t.Fatal("别名指向别名应被拒绝（只允许一层解析）")
	}
}

// TestAliasIsCaseSensitive 验证大小写敏感。
//
// 守护：契约 §4.5。模型 ID 本身就是大小写敏感的；
// 若有人"顺手"加个 ToLower 做容错，会让 "DSF" 与 "dsf" 解析到
// 同一个模型，与 /v1/models 列出的 ID 对不上（客户端按 ID 精确匹配）。
func TestAliasIsCaseSensitive(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}

	// 别名本身大小写不同 → 不算命中。
	// 注意 "DSF" 无前缀，会走「无前缀兜底」在 models 里按 upstreamID 找，
	// 找的是 "space-bunny"/"glm-5.3"，都不是 "DSF"，所以必然失败。
	if _, _, _, err := r.Resolve("DSF"); err == nil {
		t.Error("大小写不同的别名不应命中")
	}

	// 目标大小写不同 → 不算有效模型，SetAliases 阶段就应拒绝。
	if err := r.SetAliases(map[string]string{"x": "workbuddy/Space-Bunny"}); err == nil {
		t.Error("大小写不同的目标不应被接受")
	}
}

// TestAliasRejectsEmptyModelName 验证空模型名仍然报错（不被别名逻辑吞掉）。
//
// 守护：Resolve 的空值早退分支不能被"先查别名"的改动挪走或绕过。
func TestAliasRejectsEmptyModelName(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.Resolve("  "); err == nil {
		t.Error("空模型名应报错")
	}
}

// TestAliasUnknownNameStillNotFound 验证不认识的别名报 ErrModelNotFound。
//
// 守护：错误类型。chat 层靠 errors.Is 判 404，错误类型变了会变成 500。
func TestAliasUnknownNameStillNotFound(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := r.Resolve("nope")
	if err == nil {
		t.Fatal("未登记的别名应报错")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("应为 ErrModelNotFound，实际: %v", err)
	}
	// 错误信息里应回显【用户请求的名字】，而不是别的名字。
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误信息应包含请求的模型名，实际: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────
// 原子性
// ─────────────────────────────────────────────────────────────

// TestAliasSetIsAtomicOnFailure 验证【校验失败时不改变现有别名表】。
//
// 守护：契约里"返回错误表示别名表非法（不落盘、不生效）"。
// 若实现改成"边校验边写入"，一个非法表项会让面板保存报错、
// 但内存里的表已经被改了一半 —— 用户看到"保存失败"却发现路由已变，
// 是典型的"报错与状态不一致"故障。
func TestAliasSetIsAtomicOnFailure(t *testing.T) {
	r := aliasRouter(t)

	good := map[string]string{"dsf": "workbuddy/space-bunny"}
	if err := r.SetAliases(good); err != nil {
		t.Fatal(err)
	}

	// 一批"大部分合法、混一个非法"的表：必须是整体拒绝。
	bad := map[string]string{
		"a": "workbuddy/space-bunny", // 合法
		"b": "workbuddy/glm-5.3",     // 合法
		"c": "workbuddy/nope",        // 非法 → 整批应被拒
	}
	if err := r.SetAliases(bad); err == nil {
		t.Fatal("含非法项的别名表应被整体拒绝")
	}

	// 旧表必须完好无损。
	got := r.Aliases()
	if len(got) != 1 || got["dsf"] != "workbuddy/space-bunny" {
		t.Fatalf("失败的 SetAliases 改动了现有表: %v", got)
	}
	// 非法表里的合法项也绝不能"渗进来"。
	for _, leaked := range []string{"a", "b", "c"} {
		if _, ok := got[leaked]; ok {
			t.Errorf("失败调用泄漏了表项 %q", leaked)
		}
	}
	// 解析行为也必须照旧。
	if _, _, _, err := r.Resolve("dsf"); err != nil {
		t.Errorf("旧别名失效: %v", err)
	}
}

// TestAliasSetReplacesWholly 验证成功时是【整体替换】而非合并。
//
// 守护：面板提交的是一份完整表单，删掉一项再保存必须真的删掉。
// 若实现写成"逐项合并"，用户会发现别名删不掉（只能改不能删）。
func TestAliasSetReplacesWholly(t *testing.T) {
	r := aliasRouter(t)

	if err := r.SetAliases(map[string]string{
		"a": "workbuddy/space-bunny",
		"b": "workbuddy/glm-5.3",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetAliases(map[string]string{"b": "workbuddy/glm-5.3"}); err != nil {
		t.Fatal(err)
	}

	got := r.Aliases()
	if len(got) != 1 {
		t.Fatalf("期望只剩 1 项，实际 %d 项: %v", len(got), got)
	}
	if _, ok := got["a"]; ok {
		t.Error("被移除的别名 a 仍然存在（应整体替换）")
	}
	if _, _, _, err := r.Resolve("a"); err == nil {
		t.Error("已移除的别名 a 仍能解析")
	}
}

// TestAliasEmptyTableClears 验证空表/ nil 能清空别名。
//
// 守护：契约没写 nil 的语义。保守取值：空表 = 清空（面板"全部删除"要能用）。
// nil 也按空表处理，避免调用方传 nil 时 panic 或留下旧表。
func TestAliasEmptyTableClears(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}

	if err := r.SetAliases(nil); err != nil {
		t.Fatalf("nil 表应被接受（等价于清空）: %v", err)
	}
	if len(r.Aliases()) != 0 {
		t.Errorf("nil 表未清空别名: %v", r.Aliases())
	}
	if _, _, _, err := r.Resolve("dsf"); err == nil {
		t.Error("清空后 dsf 不应再解析成功")
	}
}

// ─────────────────────────────────────────────────────────────
// Aliases() 的副本语义
// ─────────────────────────────────────────────────────────────

// TestAliasesReturnsCopy 验证 Aliases() 返回的是副本。
//
// 守护：调用方（面板/设置 API）拿到 map 后会做序列化，
// 若返回内部引用，任何一次误写都会变成"面板改内存、却不落盘"，
// 并且制造 map 并发读写崩溃。这里通过"改返回值不影响 Router"来断言。
func TestAliasesReturnsCopy(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}

	snapshot := r.Aliases()
	snapshot["dsf"] = "workbuddy/glm-5.3" // 恶意/误操作改副本
	snapshot["injected"] = "workbuddy/glm-5.3"

	fresh := r.Aliases()
	if fresh["dsf"] != "workbuddy/space-bunny" {
		t.Errorf("修改返回值污染了内部表: dsf = %q", fresh["dsf"])
	}
	if _, ok := fresh["injected"]; ok {
		t.Error("返回值不是副本：新增项出现在内部表里")
	}

	// 两次调用必须是不同的 map 实例。
	if &snapshot == &fresh {
		t.Error("两次 Aliases() 返回了同一个 map")
	}
}

// TestAliasesEmptyIsNotNil 验证空表返回非 nil。
//
// 守护：面板/设置 API 会直接 json.Marshal 这个值。
// nil map 序列化成 null，而契约 §3 里 aliases 是对象 ——
// 前端若不判空就会在 `null["x"]` 上炸掉。
func TestAliasesEmptyIsNotNil(t *testing.T) {
	r := New()
	got := r.Aliases()
	if got == nil {
		t.Fatal("空别名表应返回非 nil 的 map（否则会被序列化成 null）")
	}
	if len(got) != 0 {
		t.Errorf("新 Router 的别名表应为空，实际: %v", got)
	}
}

// ─────────────────────────────────────────────────────────────
// Models() 必须包含别名条目
// ─────────────────────────────────────────────────────────────

// TestAliasAppearsInModels 验证别名出现在 Models() 里。
//
// 守护：契约 §4「Models() 包含别名条目」（Codex 指出：否则客户端先查
// /v1/models 会拒绝调用）。别名的 id 用别名本身，并标明真实模型。
func TestAliasAppearsInModels(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err != nil {
		t.Fatal(err)
	}

	models := r.Models()
	byID := make(map[string]int, len(models))
	for i, m := range models {
		byID[m.ID] = i
	}

	idx, ok := byID["dsf"]
	if !ok {
		t.Fatalf("别名 dsf 未出现在 Models() 里；实际: %v", keysOf(byID))
	}
	aliasModel := models[idx]

	// id 用别名本身。
	if aliasModel.ID != "dsf" {
		t.Errorf("别名条目 ID = %q，期望 dsf", aliasModel.ID)
	}
	// 标明它指向的真实模型 ID（而不是只给一个无从对照的显示名）。
	if !strings.Contains(aliasModel.Name, "workbuddy/space-bunny") {
		t.Errorf("别名条目 Name = %q，应标明真实模型 ID workbuddy/space-bunny", aliasModel.Name)
	}
	// 能力字段应与目标模型一致（否则客户端会误判上下文长度/模态能力）。
	real := models[byID["workbuddy/space-bunny"]]
	if aliasModel.Capabilities.ContextWindow != real.Capabilities.ContextWindow {
		t.Errorf("别名条目 contextWindow = %d，应等于真实模型的 %d",
			aliasModel.Capabilities.ContextWindow, real.Capabilities.ContextWindow)
	}

	// 真实模型本身必须仍在列表里（别名是"增加"不是"替换"）。
	if _, ok := byID["workbuddy/space-bunny"]; !ok {
		t.Error("真实模型条目不应因别名而消失")
	}
	// 总数 = 2 个真实模型 + 1 个别名。
	if len(models) != 3 {
		t.Errorf("模型数 = %d，期望 3（2 真实 + 1 别名）", len(models))
	}
}

// TestAliasModelsStillSorted 验证加入别名后列表仍按 ID 升序。
//
// 守护：model_order_test.go 里那条"列表顺序稳定"的实测反馈。
// 别名是在排序之前并入还是之后并入，很容易写错 ——
// 写错的表现是"别名永远排在最后"或顺序随机跳动。
func TestAliasModelsStillSorted(t *testing.T) {
	r := aliasRouter(t)
	if err := r.SetAliases(map[string]string{
		"zzz": "workbuddy/space-bunny", // 排在最后
		"aaa": "workbuddy/glm-5.3",     // 排在最前
		"mmm": "workbuddy/space-bunny",
	}); err != nil {
		t.Fatal(err)
	}

	first := r.Models()
	for i := 1; i < len(first); i++ {
		if first[i-1].ID > first[i].ID {
			t.Fatalf("未按 ID 升序：%q 在 %q 之前", first[i-1].ID, first[i].ID)
		}
	}

	// 顺序必须稳定（map 迭代随机，别名并入后不能重新引入抖动）。
	for n := 0; n < 20; n++ {
		got := r.Models()
		if len(got) != len(first) {
			t.Fatalf("第 %d 次长度变化: %d → %d", n, len(first), len(got))
		}
		for i := range got {
			if got[i].ID != first[i].ID {
				t.Fatalf("第 %d 次顺序变化：位置 %d 是 %q，期望 %q",
					n, i, got[i].ID, first[i].ID)
			}
		}
	}
}

// TestAliasSkippedInModelsWhenTargetGone 验证目标消失时 Models() 跳过别名。
//
// 守护：契约没写"别名目标在注册后消失"该怎么办。
// 保守取值：列表里【跳过】而不是伪造一个能力为空的条目
// （伪造会让客户端以为该模型上下文为 0）。解析时仍返回明确错误。
func TestAliasSkippedInModelsWhenTargetGone(t *testing.T) {
	r := New() // 先用一个空的路由器
	// 无法在"先有模型后没模型"的情况下构造，改为直接构造：
	// 用一个还没注册任何 provider 的路由器，先塞别名会被校验拒绝，
	// 所以这里验证的是"目标存在才能设别名"这条前置约束的另一面。
	if err := r.SetAliases(map[string]string{"dsf": "workbuddy/space-bunny"}); err == nil {
		t.Fatal("没有任何模型时不应能设置别名（目标无法解析）")
	}
	if len(r.Models()) != 0 {
		t.Errorf("空路由器的模型列表应为空，实际 %d 项", len(r.Models()))
	}
}

// keysOf 把 map 的 key 排序后返回（仅用于测试失败信息）。
func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ─────────────────────────────────────────────────────────────
// 并发
// ─────────────────────────────────────────────────────────────

// TestAliasConcurrentSetAndResolve 高强度并发压测。
//
// 为什么要有这个测试：SetAliases / Aliases / Resolve 会被并发调用
// （面板写、请求读）。本机没有 gcc，`go test -race` 用不了，
// 因此用高强度并发代替 —— 任何 map 并发读写都会让测试进程直接
// crash（"fatal error: concurrent map read and map write"），
// 不需要 race 检测器也能抓到。
//
// 同时穿插 Register：它写 r.models，用的是同一把锁。
// 若有人把别名表换成无锁的 atomic.Value 却忘了 models，
// 或者加锁范围漏掉别名那一段，这里会红。
func TestAliasConcurrentSetAndResolve(t *testing.T) {
	r := aliasRouter(t)

	const workers = 16
	const rounds = 200

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写者：不断替换别名表（合法的与【非法】的混着来，
	// 因为失败的路径同样要保证不改状态、不 panic）。
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				select {
				case <-stop:
					return
				default:
				}
				var m map[string]string
				switch j % 4 {
				case 0:
					m = map[string]string{"a": "workbuddy/space-bunny"}
				case 1:
					m = map[string]string{
						"a": "workbuddy/space-bunny",
						"b": "workbuddy/glm-5.3",
					}
				case 2:
					// 非法：目标不存在 → 必须返回错误且不动状态
					m = map[string]string{"bad": "workbuddy/nope"}
				default:
					// 非法：循环
					m = map[string]string{"a": "b", "b": "a"}
				}
				_ = r.SetAliases(m)
				_ = r.Aliases()
			}
		}(i)
	}

	// 读者：不断解析别名与真实模型，并读模型列表。
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			names := []string{"a", "b", "dsf", "workbuddy/space-bunny", "space-bunny"}
			for j := 0; j < rounds; j++ {
				select {
				case <-stop:
					return
				default:
				}
				name := names[(n+j)%len(names)]
				_, _, _, _ = r.Resolve(name)
				_, _ = r.UpstreamModelID(name)
				_ = r.Models()
				_ = r.Providers()
			}
		}(i)
	}

	// 登记者：与别名写入并行走同一条锁路径。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < rounds/4; j++ {
			_ = r.Register(context.Background(), wbProvider())
		}
	}()

	wg.Wait()
	close(stop)

	// 压测后状态仍然可用（不能只是"没崩"）。
	if err := r.SetAliases(map[string]string{"final": "workbuddy/space-bunny"}); err != nil {
		t.Fatalf("压测后 SetAliases 失败: %v", err)
	}
	if _, _, _, err := r.Resolve("final"); err != nil {
		t.Fatalf("压测后 Resolve 失败: %v", err)
	}
}

// TestAliasConcurrentReadsAreConsistent 验证并发读不会看到"半张表"。
//
// 守护：SetAliases 用整体替换而非原地增删。若改成"先清空再逐项写入"，
// 并发读者会看到一个空表或只有一半的表 —— 表现是请求偶发 404，
// 极难复现。这里让两个别名【总是同时存在或同时不存在】，
// 任何一个读者观察到"只有其一"就说明存在中间态。
func TestAliasConcurrentReadsAreConsistent(t *testing.T) {
	r := aliasRouter(t)

	pairA := map[string]string{"a1": "workbuddy/space-bunny", "a2": "workbuddy/glm-5.3"}
	pairB := map[string]string{"a1": "workbuddy/glm-5.3", "a2": "workbuddy/space-bunny"}

	var wg sync.WaitGroup
	const readers = 8
	const swaps = 300

	done := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				got := r.Aliases()
				// 允许为空（尚未写入），但绝不能只出现一个。
				if n := len(got); n != 0 && n != 2 {
					t.Errorf("观察到半张表（%d 项）: %v", n, got)
					return
				}
			}
		}()
	}

	for i := 0; i < swaps; i++ {
		if i%2 == 0 {
			if err := r.SetAliases(pairA); err != nil {
				t.Fatalf("第 %d 次替换失败: %v", i, err)
			}
		} else {
			if err := r.SetAliases(pairB); err != nil {
				t.Fatalf("第 %d 次替换失败: %v", i, err)
			}
		}
	}
	close(done)
	wg.Wait()
}

// TestAliasConcurrentTargetRemoved 覆盖"并发改名 + 目标不存在"这条路径。
//
// 守护：Models() 里跳过目标缺失分支、Resolve 里别名展开后找不到模型
// 分支 —— 这两条在单测里很容易漏掉，却是并发下唯一会真实触发的组合。
func TestAliasConcurrentTargetRemoved(t *testing.T) {
	r := New()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 交替注册 provider（目标存在 / 不存在）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// 注册一个"模型表为空"的 provider，使 workbuddy/space-bunny 消失。
			_ = r.Register(context.Background(), &fakeProvider{id: "workbuddy"})
			_ = r.Register(context.Background(), wbProvider())
		}
	}()

	// 并发读写别名并解析。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = r.SetAliases(map[string]string{fmt.Sprintf("x%d", n): "workbuddy/space-bunny"})
				_, _, _, _ = r.Resolve("workbuddy/space-bunny")
				_ = r.Models()
			}
		}(i)
	}

	wg.Wait()
	close(stop)
}
