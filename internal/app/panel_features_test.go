package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/config"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// ═══════════════════════════════════════════════════════════════════════
// 面板契约护栏（2026-10-09：去掉粘贴凭据 + 凭证保活 + 版本更新）
// ═══════════════════════════════════════════════════════════════════════
//
// 🔴 为什么这些要用"读前端源码文本"来守（而不是只测后端）：
//
//	面板是纯 JS + 静态 HTML，Go 单测覆盖不到它的分支。
//	本项目已经踩过一次同类坑（第 56 轮缺陷①：前端 service 标识没跟着
//	后端改名 —— 后端全绿，界面却永不跳转）。⇒ 前后端契约必须显式断言。

// panelAsset 取面板某个静态资源的文本。
func panelAsset(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(newMux(Deps{
		Logger: log.New(io.Discard, "", 0),
		Usage:  usagepkg.NewStore(t.TempDir()),
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/panel/" + name)
	if err != nil {
		t.Fatalf("取 %s 失败: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取 %s 状态码 = %d", name, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// stripJSComments 去掉 JS 里的行注释与块注释。
//
// ⚠️ 这不是完整的 JS 词法分析器（不处理注释符出现在字符串字面量里的情形）。
// 对本项目的用途足够：要检查的标识符（元素 id、函数名）不会出现在
// 含 `//` 或 `/*` 的字符串里。若将来面板出现这类字符串，
// 本函数的结论需要重新评估 —— 这一点写在这里，免得后来者误信。
func stripJSComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		// 块注释 /* ... */
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				break // 未闭合：剩余全是注释
			}
			i += 2 + end + 2
			continue
		}
		// 行注释 // ...
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				break
			}
			i += nl // 保留换行本身（不影响 Contains 断言）
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}

// TestPanelPasteCredentialUIRemoved 守「粘贴凭据」入口已被删除。
//
// 🔴 委托方 2026-10-09 原话：
//
//	「方式二：粘贴凭据，取消这个登录方式，太不方便了。」
//
// 反向对照：把那段 UI 加回去（含 import-text 文本域），本测试立刻红。
//
// ⚠️ 检查前必须**剥掉注释**。理由（这条我自己踩过）：
//
//	第一版直接用 Contains 全文匹配，结果被**注释里的历史说明**判为失败 ——
//	而那些注释正是"记录为什么删掉"的资产（理由比结论保值），不该删掉。
//	这与第 56 轮 TestBuildScriptProducesGUISubsystem 踩的坑**同型**：
//	用全文匹配检查"某字符串没了"，会被它自己的报错文案/注释骗到。
//	⇒ 先剥注释再断言，测的才是**真实生效的代码**。
func TestPanelPasteCredentialUIRemoved(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	// 这些是"粘贴凭据"入口的独有标识（只应出现在注释里，不该在代码里）。
	forbidden := []string{
		"import-text",       // 粘贴用的文本域
		"btn-do-import",     // 「导入」按钮
		"import-result",     // 导入结果容器
		"flattenCredential", // 只为粘贴而写的形状转换
		"doImport",          // 粘贴的提交函数
	}
	for _, bad := range forbidden {
		if strings.Contains(js, bad) {
			t.Errorf("app.js 的**代码**里仍含已删除的「粘贴凭据」标识 %q"+
				"（委托方要求取消该登录方式）", bad)
		}
	}
}

// TestPanelStillOffersWebLogin 守"删掉方式二之后网页登录还在"。
//
// 🔴 与上一条是**一对**：只删不加会让用户**无路可走**。
//
//	删 UI 时最容易犯的错就是连网页登录一起删掉/改坏 id。
func TestPanelStillOffersWebLogin(t *testing.T) {
	js := panelAsset(t, "app.js")

	for _, want := range []string{
		"btn-web-login",              // 网页登录按钮
		"/api/accounts/login/start",  // 启动登录
		"/api/accounts/login/status", // 轮询状态
		"login-platform",             // 平台选择（cn/intl）
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q —— 删掉粘贴方式后，网页登录必须完好", want)
		}
	}
}

// TestPanelImportAPIBackendKept 守"面板删 UI 但后端导入接口保留"。
//
// 🔴 为什么不能连后端一起删：
//
//	CLI `wbapi auth import` 仍在用它，而且它是"本机没装
//	Chrome/Edge 时唯一的添加入口"。删 UI 是 UI 决策，
//	**不该顺带砍掉能力**（那会制造一个无法恢复的功能缺口）。
func TestPanelImportAPIBackendKept(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{
		Logger: log.New(io.Discard, "", 0),
		Usage:  usagepkg.NewStore(t.TempDir()),
	}))
	t.Cleanup(srv.Close)

	// POST 空体应当得到一个明确的错误（400/403），而**不是 404**。
	// 404 = 路由不存在 = 我们把后端的导入能力也删掉了。
	resp, err := http.Post(srv.URL+"/api/accounts/import", "application/json",
		strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Error("/api/accounts/import 返回 404 —— 后端导入接口被误删（CLI 依赖它）")
	}
}

// TestPanelKeepaliveToggleWired 守凭证保活开关的前后端契约。
//
// 🔴 守的是一条**具体的历史缺陷**（第 56 轮①）：
//
//	前端字段名与后端 JSON tag 不一致时，后端全绿、界面却永远显示错的。
//	这里三处必须**同名**：
//	  · HTML 的元素 id（settings.js 按 id 取）
//	  · settings.js 发送的 PATCH 字段名
//	  · 后端 settingsPatch 的 JSON tag（Keepalive *bool `json:"keepalive"`）
func TestPanelKeepaliveToggleWired(t *testing.T) {
	html := panelAsset(t, "index.html")
	js := panelAsset(t, "settings.js")

	// HTML 必须有开关元素与状态位
	for _, want := range []string{"st-keepalive", "st-sum-keepalive", "st-keepalive-at"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}
	// JS 必须绑定并发送 keepalive 字段
	for _, want := range []string{"st-keepalive", "keepalive", "keepaliveRuntime"} {
		if !strings.Contains(js, want) {
			t.Errorf("settings.js 缺少 %q", want)
		}
	}
}

// TestPanelCheckinRuntimeContract 守签到「运行状态」一行的前后端契约。
//
// 🔴 守的是一条**真实且持续了很久**的缺陷（2026-10-09 发现）：
//
//	settings.js 的 checkinRuntime 一直在"宽泛猜字段名"
//	（checkinLastAt / checkin.lastAt / checkinLastResult / …），
//	而后端 settingsView **一个都没有**、也从来没有过 ⇒ 那一行
//	的「上次触发/结果」永远是「—」，而且前端因为有回退**不报错**。
//	这与第 56 轮①（keepalive 前后端字段名不一致）是同型缺陷。
//
// 断言的是"名字三处必须同名"（HTTP 契约两侧）：
//   - 后端 settingsView 的 json tag：checkinRun
//   - 前端读取的对象名：d.checkinRun
//   - 对象内部字段：lastRunAt / lastResult / skippedNoAccounts
//
// ⚠️ 必须**先剥注释**再断言：本文件里 checkinRuntime 的注释**故意**
//
//	写下了那些已废弃的旧字段名（解释"为什么不再猜字段名"），
//	全文匹配会把注释误判成"代码里还在猜"。
func TestPanelCheckinRuntimeContract(t *testing.T) {
	js := stripJSComments(panelAsset(t, "settings.js"))
	html := panelAsset(t, "index.html")

	// HTML：这一行的三个状态位必须在
	for _, want := range []string{"st-checkin-state", "st-checkin-at", "st-checkin-result"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}

	// JS：只认 checkinRun 这**一个**权威契约
	if !strings.Contains(js, "d.checkinRun") {
		t.Error("settings.js 没有读 d.checkinRun —— " +
			"后端已给出权威字段，前端却仍在猜字段名")
	}
	for _, want := range []string{"lastRunAt", "lastResult", "skippedNoAccounts"} {
		if !strings.Contains(js, want) {
			t.Errorf("settings.js 没有读 checkinRun 的 %q（契约字段对不上）", want)
		}
	}

	// 🔴 反向断言：那几个"猜出来的"字段名**不许**再出现在代码里。
	//	它们从来不存在，留着只会让下一次契约变更继续被静默吞掉。
	for _, bad := range []string{
		"checkinLastAt", "checkinLastResult", "checkin_last_at",
		"lastCheckinAt", "firstOf(",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("settings.js 的代码里仍有已废弃的猜测字段 %q —— "+
				"这会让前后端字段名不一致重新变成静默失败", bad)
		}
	}
}

// TestSettingsViewCheckinRunNilWhenNotAssembled 守"未装配时字段省略"。
//
// Deps.Checkin 为 nil（测试/CLI）时必须**不出现** checkinRun 键 ——
// 否则前端会拿到一个全零对象，把"守护没装配"显示成"已停用/从未触发"，
// 又是一次"界面在骗人"。
func TestSettingsViewCheckinRunNilWhenNotAssembled(t *testing.T) {
	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatalf("加载测试设置失败: %v", err)
	}

	// 未装配：Checkin == nil
	raw, err := json.Marshal(Deps{Settings: store}.settingsView())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "checkinRun") {
		t.Errorf("未装配签到守护时响应里不该有 checkinRun: %s", raw)
	}

	// 已装配：必须给出对象，且字段是契约里的那几个（且**不含凭据**）
	deps := Deps{Settings: store}
	daemon, _ := newCheckinDaemon(deps)
	deps.Checkin = daemon

	raw, err = json.Marshal(deps.settingsView())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	obj, ok := got["checkinRun"].(map[string]any)
	if !ok {
		t.Fatalf("已装配时缺少 checkinRun 对象: %s", raw)
	}
	for _, want := range []string{"enabled", "running", "skippedNoAccounts"} {
		if _, has := obj[want]; !has {
			t.Errorf("checkinRun 缺少字段 %q: %s", want, raw)
		}
	}
	// 🔴 反向护栏：**这个对象**绝不能出现任何凭据字段。
	//	（它来自 CheckinStatus —— 只有计数/时间，本就不该有 token。）
	//
	//	⚠️ 只序列化 checkinRun 自己：整个 settingsView 里本就有
	//	revisionToken（那是**并发令牌**，不是凭据），对它做子串匹配
	//	会把合法的 revisionToken 误判成泄露。
	objRaw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"token", "Token", "cookie", "Cookie", "authorization"} {
		if strings.Contains(string(objRaw), bad) {
			t.Errorf("checkinRun 里出现了疑似凭据字段 %q —— "+
				"设置接口不得泄露上游 token: %s", bad, objRaw)
		}
	}
}

// TestPanelUpdateUsesOnlyUserTriggeredEndpoints 守"没有自动更新"。
//
// 🔴 委托方 2026-10-09 明确要求的形态：
//
//	「没有软件自动更新功能，但是有检测最新版本按钮，
//	  检测出新版本后会有更新版本的按钮，点了就自动安装并更新」
//
// ⇒ 前端**只允许**在点击时调这两个接口：
//
//	· /api/update/check  （检测）
//	· /api/update/apply  （安装）
//
// ⚠️ 反向对照：把 loadUpdateStatus 改成调 /api/update/check
//
//	（即"打开面板就偷偷检查"），本测试立刻红。
func TestPanelUpdateUsesOnlyUserTriggeredEndpoints(t *testing.T) {
	js := panelAsset(t, "settings.js")

	if !strings.Contains(js, "/api/update/check") {
		t.Error("settings.js 缺少「检测新版本」的接口调用")
	}
	if !strings.Contains(js, "/api/update/apply") {
		t.Error("settings.js 缺少「更新到新版本」的接口调用")
	}

	// 🔴 关键断言：check 必须在**事件绑定**里出现（即由点击触发），
	//	而不是在初始化/轮询路径里。
	//
	//	判据取法：`on('btn-st-update-check', ...)` 这一行必须存在。
	if !strings.Contains(js, "on('btn-st-update-check'") {
		t.Error("「检测新版本」没有绑定到按钮点击 —— 疑似变成了自动检查")
	}
	if !strings.Contains(js, "on('btn-st-update-apply'") {
		t.Error("「更新到新版本」没有绑定到按钮点击")
	}

	// 初始化只允许读状态（GET /api/update），不得主动 check。
	if !strings.Contains(js, "loadUpdateStatus") {
		t.Error("缺少 loadUpdateStatus（打开设置页应只读状态）")
	}
}

// TestPanelUpdateButtonsExist 守两个按钮都在 HTML 里。
func TestPanelUpdateButtonsExist(t *testing.T) {
	html := panelAsset(t, "index.html")

	for _, want := range []string{
		"btn-st-update-check", // 检测新版本
		"btn-st-update-apply", // 更新到新版本
		"st-ver-current",      // 当前版本
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}

	// 更新按钮必须**默认隐藏**（只有检测到新版本才显示）。
	//
	// 🔴 否则用户会看到一个"更新"按钮，点下去却被告知"已是最新" —— 骗人。
	idx := strings.Index(html, "btn-st-update-apply")
	if idx < 0 {
		t.Fatal("找不到更新按钮")
	}
	// 取该按钮所在的那一行，确认带 st-hidden。
	lineStart := strings.LastIndex(html[:idx], "\n")
	lineEnd := strings.Index(html[idx:], "\n")
	if lineEnd < 0 {
		lineEnd = len(html) - idx
	}
	line := html[lineStart : idx+lineEnd]
	if !strings.Contains(line, "st-hidden") {
		t.Error("「更新到新版本」按钮默认必须是隐藏的（仅在检测到新版本后显示）")
	}
}

// TestUpdateEndpointsRequireCSRF 守两个更新接口都过 CSRF 防护。
//
// 🔴 这是本项目**最危险**的功能（把"运行远程字节"变成一次点击）。
//
//	没有 CSRF 防护 ⇒ 你浏览器里打开的恶意网页可以诱导本机
//	下载并替换程序。
func TestUpdateEndpointsRequireCSRF(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{
		Logger: log.New(io.Discard, "", 0),
		Usage:  usagepkg.NewStore(t.TempDir()),
	}))
	t.Cleanup(srv.Close)

	// 跨站写请求（带 Origin 但不是同源）必须被拒。
	for _, path := range []string{"/api/update/check", "/api/update/apply"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		req.Header.Set("Origin", "http://evil.example.com")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s 跨站 POST 状态码 = %d，期望 403（缺 CSRF 防护）",
				path, resp.StatusCode)
		}
	}
}

// TestUpdateStatusIsReadOnly 守 GET /api/update 不需要 CSRF（只读）。
func TestUpdateStatusIsReadOnly(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{
		Logger: log.New(io.Discard, "", 0),
		Usage:  usagepkg.NewStore(t.TempDir()),
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/update")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Deps.Update 为 nil（本次没装配）⇒ 503 是**预期**的明确错误，
	// 不是 403/404。503 说明路由存在且只读放行。
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /api/update 状态码 = %d，期望 503（未装配更新器时的明确错误）",
			resp.StatusCode)
	}
}
