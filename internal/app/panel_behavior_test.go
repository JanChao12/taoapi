package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────
// 面板显示逻辑的护栏测试（2026-10-06，第 37 轮 Codex 评审后）
//
// 这一组守的是**两个真实被委托方抓到的 bug**：
//
//	A. key「更换」按钮点了完全没反应（无报错）
//	B. 「上游限流」红字与「反代使用中」自相矛盾
//
// ⚠️ 局限（必须说清）：这些测试**读的是下发的前端源码**，
// 断言"写法符合约定"，**不执行浏览器、不验证真实点击行为**。
// 真正的行为验收要靠真实服务 + 浏览器（交接文档 §3.2）。
// 且它们跑在源码上：改了 panel/ 却没重编 exe 时测试仍会通过 ——
// 这个盲区无法用 Go 测试发现。
// ─────────────────────────────────────────────────────────────

// TestPanelBusyIsSingleSourceOfTruth 守：按钮 disabled 只有一个写入源。
//
// 🔴 这是 bug A 的根因（Codex 第 37 轮 A2 裁定）：
//
//	原来 renderSettings 每次轮询都写 `changeBtn.disabled = false`，
//	而 setBusy 又按 busy 写 `disabled = true` —— 两个来源打架。
//	按钮一旦 disabled，浏览器**根本不派发 click**，
//	用户看到的就是"点了完全没反应且不报错"（与委托方现象完全吻合）。
//
// 断言两件事：
//  1. renderSettings 里**不得**再出现 `changeBtn.disabled` / `saveKey.disabled`
//     / `keyInput.disabled` 这类散落写入；
//  2. setBusy 仍然管理这批按钮（否则按钮永远可点，破坏防重入）。
func TestPanelBusyIsSingleSourceOfTruth(t *testing.T) {
	js := fetchSettingsJS(t)

	// 1) renderSettings 内不得散落设置这些控件的 disabled
	//    （剥注释：说明性注释里保留了旧写法，直接搜会误报）
	body := stripComments(extractFunc(t, js, "function renderSettings(d)"))
	for _, forbidden := range []string{
		"changeBtn.disabled = false",
		"saveKey.disabled = false",
		"clearBtn.disabled = false",
		"keyInput.disabled = false",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("renderSettings 里仍有 %q —— "+
				"按钮禁用必须只由 setBusy 决定（否则与 busy 打架，"+
				"表现为点按钮完全无反应且不报错）", forbidden)
		}
	}

	// 2) setBusy 仍然管理这批按钮
	if !strings.Contains(js, "function setBusy(") {
		t.Fatal("setBusy 不见了")
	}
	busyBody := stripComments(extractFunc(t, js, "function setBusy(b)"))
	for _, want := range []string{"btn-st-key-save", "btn-st-port-save"} {
		if !strings.Contains(busyBody, want) {
			t.Errorf("setBusy 未管理 %q —— 防重入会失效", want)
		}
	}
}

// TestPanelInitResetsBusyState 守：初始化时显式恢复空闲态。
//
// Codex 第 37 轮 A2 第 4 条要求。若初始化没跑这一步，
// 任何残留的禁用态（bfcache 恢复、上次异常结束）会让按钮永久不可点。
func TestPanelInitResetsBusyState(t *testing.T) {
	js := fetchSettingsJS(t)

	initBody := extractFunc(t, js, "function init()")
	if !strings.Contains(initBody, "setBusy(false)") {
		t.Error("init() 没有显式调用 setBusy(false) —— " +
			"残留禁用态会让按钮永久不可点，且用户无法自救")
	}
}

// TestPanelKeyInputIsDirectlyEditable 守：密钥是"直接可编辑输入框"这一设计。
//
// 🔴 背景（2026-10-06 委托方要求）：
//
//	旧的「更换按钮 + 点击展开表单」交互反复修不好（点了没反应）。
//	委托方明确要求改成和「端口」一样：
//	  「将 key 的框里设置为可直接输入的类型框，我直接在框里改为我想要
//	    的 key 然后点一下更换就直接生效，就像修改端口一样」
//
// 这条测试守住新设计的要点，防止有人"好心改回"旧交互：
//  1. 输入框类型是 text（明文可见），不是 password；
//  2. 不再有「更换」「取消」按钮，也没有需要展开的表单容器；
//  3. 保存与复制按钮存在。
func TestPanelKeyInputIsDirectlyEditable(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := string(raw)

	// 1) 明文输入框
	idx := strings.Index(html, `id="st-key-input"`)
	if idx < 0 {
		t.Fatal("找不到密钥输入框 st-key-input")
	}
	// 往前找这个标签的开头（<input），往后找标签结束（>）
	segStart := strings.LastIndex(html[:idx], "<input")
	seg := html[segStart:]
	if end := strings.Index(seg, ">"); end > 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, `type="text"`) {
		t.Errorf("密钥输入框不是 type=text —— 委托方要求明文可直接编辑\n实际: %s", seg)
	}

	// 2) 旧交互的产物必须消失
	for _, gone := range []string{
		`id="btn-st-key-change"`,
		`id="btn-st-key-cancel"`,
		`id="st-key-form"`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("仍存在 %s —— 新交互已取消「更换/取消/展开表单」，"+
				"它正是「点了没反应」的来源", gone)
		}
	}

	// 3) 保存与清空按钮必须在
	for _, want := range []string{`id="btn-st-key-save"`, `id="btn-st-key-clear"`} {
		if !strings.Contains(html, want) {
			t.Errorf("缺少 %s", want)
		}
	}

	// 4) 复制按钮应已移除（2026-10-06 委托方要求）：
	//    「现在key已经明文显示了，不需要复制按钮了」
	//    明文就在输入框里，可直接选中，多一个按钮反而占地方。
	if strings.Contains(html, `id="btn-st-key-copy"`) {
		t.Error("复制按钮仍在 —— 委托方已要求移除（明文可见，可直接选中）")
	}
}

// TestPanelKeyInputNotOverwrittenWhileTyping 守：轮询不得冲掉用户正在输入的 key。
//
// 🔴 新交互下输入框常驻，轮询每 5 秒会用服务端值同步它 ——
// 不做保护的话，用户打了一半的新 key 会被清掉
// （Codex 第 36 轮 T4 第 4 条）。
func TestPanelKeyInputNotOverwrittenWhileTyping(t *testing.T) {
	js := fetchSettingsJS(t)

	if !strings.Contains(js, "function isKeyInputDirty(") {
		t.Fatal("缺少 isKeyInputDirty —— 轮询会冲掉用户正在输入的密钥")
	}
	dirtyBody := stripComments(extractFunc(t, js, "function isKeyInputDirty()"))
	if !strings.Contains(dirtyBody, "activeElement") {
		t.Error("isKeyInputDirty 没看焦点 —— 用户正在输入时会被覆盖")
	}
	if !strings.Contains(dirtyBody, "keyDirty") {
		t.Error("isKeyInputDirty 没看 dirty 标记 —— 用户改过但已失焦时会被覆盖")
	}

	renderBody := stripComments(extractFunc(t, js, "function renderSettings(d)"))
	if !strings.Contains(renderBody, "isKeyInputDirty()") {
		t.Error("renderSettings 同步密钥输入框前没问 isKeyInputDirty —— " +
			"会把用户正在输入的内容冲掉")
	}
}

// TestPanelModelTableHasNewColumns 守：模型表有倍率与最大输出两列。
//
// 委托方 2026-10-06 要求（原话）：
//
//	「我不要两列排版……我是说你还要加倍率和 maxOutputTokens 我同意你再加两列」
//	「你的列之间有很宽的空隙完全可以多加两列」
func TestPanelModelTableHasNewColumns(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := string(raw)

	// 表头必须含这两列
	for _, col := range []string{"倍率", "最大输出"} {
		if !strings.Contains(html, "<th class=\"num\">"+col+"</th>") &&
			!strings.Contains(html, "<th>"+col+"</th>") {
			t.Errorf("模型表缺少「%s」列", col)
		}
	}
	// 表体占位也要跟着改成 6 列（否则"加载中"那行会错位）
	if !strings.Contains(html, `colspan="6"`) {
		t.Error("模型表的 colspan 未更新为 6 —— 加载/空态行会错位")
	}
}

// TestPanelModelIDHidesPrefix 守：表格里不显示 workbuddy/ 前缀。
//
// 委托方 2026-10-06 要求（原话）：
//
//	「我建议模型id不显示 workbuddy/ 只显示 deepseek-v4.1-flash，
//	  因为我是多平台聚合的反代」
//
// ⚠️ 但**调用时仍必须带前缀** —— 所以页面必须有提示文案说明这一点，
// 否则用户照着表格里的 ID 去调用会报"模型不存在"。
// 这条测试同时守住"去前缀"与"有提示"两件事，缺一不可。
func TestPanelModelIDHidesPrefix(t *testing.T) {
	js := fetchPanelAppJS(t)

	if !strings.Contains(js, "function stripPrefix(") {
		t.Fatal("缺少 stripPrefix —— 模型 ID 仍会显示 workbuddy/ 前缀")
	}
	body := stripComments(extractFunc(t, js, "function renderApiModels(list, aliasCount)"))
	if !strings.Contains(body, "stripPrefix(") {
		t.Error("渲染模型 ID 时没有调用 stripPrefix")
	}

	// 提示文案必须存在（去前缀是**显示**行为，调用仍需前缀）
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `id="api-prefix"`) {
		t.Error("缺少前缀提示文案 —— 用户会照着表格里的 ID 调用然后报模型不存在")
	}
}

// TestPanelCreditsThreeStates 守：倍率的三种形态区分开。
//
// 🔴 最容易搞错、后果最严重的一处：
//
//	credits 缺字段 ⇒ 上游**未定价** ⇒ unknown
//	credits === 0  ⇒ 明确**免费**   ⇒ Free
//
// 把"没定价"显示成"免费"会直接误导用户以为不花钱。
// （后端 Model.Credits 用指针表达这个区别，前端必须跟着区分。）
func TestPanelCreditsThreeStates(t *testing.T) {
	js := fetchPanelAppJS(t)

	if !strings.Contains(js, "function creditsHtml(") {
		t.Fatal("缺少 creditsHtml")
	}
	body := stripComments(extractFunc(t, js, "function creditsHtml(m)"))
	if !strings.Contains(body, "credits-unknown") {
		t.Error("缺少 unknown 形态 —— 上游未定价会被显示成别的（可能误显示为免费）")
	}
	if !strings.Contains(body, "credits-free") {
		t.Error("缺少 Free 形态")
	}
	// 必须显式判断 null/undefined（与 0 区分）
	if !strings.Contains(body, "null") || !strings.Contains(body, "undefined") {
		t.Error("没有显式区分 null/undefined 与 0 —— " +
			"会把「上游未定价」误当成「免费」")
	}
}

// TestPanelBadgeUsesUpstreamColor 守：运营标签用上游给的颜色。
//
// 上游在 tags 里下发 `badge:夜间折扣:#1E90FF`，颜色是运营侧刻意选的
// （红=促销、蓝=折扣）。自己另配一套会和官方客户端对不上。
func TestPanelBadgeUsesUpstreamColor(t *testing.T) {
	js := fetchPanelAppJS(t)

	if !strings.Contains(js, "function badgeHtml(") {
		t.Fatal("缺少 badgeHtml —— 运营标签（限时免费/夜间折扣）不会显示")
	}
	body := stripComments(extractFunc(t, js, "function badgeHtml(m)"))
	if !strings.Contains(body, "b.color") {
		t.Error("badge 没有使用上游给的颜色")
	}
}

// stripComments 去掉行注释与块注释。
//
// 为什么要它：本项目的代码注释里**故意保留**了被禁止的旧写法
// （作为"为什么不能这么写"的说明）。若断言不剥注释，会把
// 说明文字当成违规代码 —— 那是测试的错，不是代码的错。
//
// ⚠️ 只处理注释，不处理字符串字面量（本项目前端没有把
//
//	"//" 写进字符串的情况；若将来有，此函数需升级）。
func stripComments(s string) string {
	// 块注释
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "*/")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + s[i+j+2:]
	}
	// 行注释
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "//"); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// TestPanelCooldownGatesRateLimitBadge 守：限流红字只在**真的冷却中**才显示。
//
// 🔴 这是 bug B 的护栏。委托方原话：
//
//	「上游限流了为什么还会显示反代优先使用，本就是逻辑互斥……
//	  这两个肯定有一个显示错了。」
//
// 根因：后端 EffectiveStatus 惰性恢复（冷却过后 status 回 normal），
// 但 StatusReason/last_error 被刻意保留作历史 —— 于是卡片同时出现
// 「正常」「⚡反代使用中」「上游限流」，自相矛盾。
//
// Codex 第 37 轮 B2 裁定：卡片只表达当前状态，冷却已过**完全隐藏**。
// 断言：红色徽标的显示条件必须经过 isCoolingDown，而不是看 last_error 有没有值。
func TestPanelCooldownGatesRateLimitBadge(t *testing.T) {
	js := fetchPanelAppJS(t)

	if !strings.Contains(js, "function isCoolingDown(") {
		t.Fatal("缺少 isCoolingDown —— 限流显示无法按「是否真的冷却」判断")
	}

	// 冷却判断必须真的比较时间
	coolBody := extractFunc(t, js, "function isCoolingDown(cooldownUntil)")
	if !strings.Contains(coolBody, "Date.parse") {
		t.Error("isCoolingDown 没有解析时间")
	}

	// noteHtml 必须由 cooling 决定，不能直接看 a.last_error
	cardBody := extractFunc(t, js, "function renderAcctCard(a, today)")
	if !strings.Contains(cardBody, "isCoolingDown(a.cooldown_until)") {
		t.Error("卡片没有用 isCoolingDown(cooldown_until) 判断限流 —— " +
			"会把过期的历史限流当成当前故障，与「反代使用中」矛盾")
	}
	// 不能再出现"有 last_error 就标红"的写法
	if strings.Contains(cardBody, "a.last_error\n") &&
		strings.Contains(cardBody, "acct-err") &&
		!strings.Contains(cardBody, "cooling") {
		t.Error("卡片仍按 last_error 直接显示红色告警")
	}
}

// TestPanelAliasNoteExists 守：面板能说明"为什么比客户端少几条"。
//
// 后端 /api/models 已过滤别名，而 /v1/models 保留别名（客户端依赖）。
// 两者数量不一致是刻意的 —— 不解释的话用户会以为模型丢了。
func TestPanelAliasNoteExists(t *testing.T) {
	srv := panelServerURL(t)

	resp, err := http.Get(srv + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `id="api-alias-note"`) {
		t.Error("index.html 缺少 id=api-alias-note 的说明节点 —— " +
			"用户会疑惑面板模型数比 API 少")
	}
}

// ── 取源码的小工具 ──

func fetchPanelAppJS(t *testing.T) string {
	t.Helper()
	return fetchPanelFile(t, "/panel/app.js")
}

func fetchSettingsJS(t *testing.T) string {
	t.Helper()
	return fetchPanelFile(t, "/panel/settings.js")
}

func fetchPanelFile(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get(panelServerURL(t) + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// extractFunc 粗略截取一个函数的源码体（到下一个顶层 function 为止）。
//
// 为什么要它：直接在整个文件里 strings.Contains 会**误判** ——
// 例如注释里提到被禁止的写法也会命中。截取函数体后断言才可靠。
// （本项目已踩过一次：注释里留着旧代码，字符串搜索误报"修复未生效"。）
func extractFunc(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("找不到函数签名 %q", sig)
	}
	rest := src[start+len(sig):]
	// 到下一个顶层 "  function " 为止
	if idx := strings.Index(rest, "\n  function "); idx >= 0 {
		return rest[:idx]
	}
	return rest
}
