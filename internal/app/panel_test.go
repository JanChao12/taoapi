package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// newPanelServer 起一个带用量存储的服务。
func newPanelServer(t *testing.T, store *usagepkg.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux(Deps{
		Logger: log.New(io.Discard, "", 0),
		Usage:  store,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPanelIndexServed 验证面板首页可访问。
func TestPanelIndexServed(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	// 面板必须包含几个关键元素
	// 注意：api/stats 的调用在 app.js 里，不在 HTML
	//
	// ⚠️ 品牌名 2026-10-06 由 wbapi 改为 TAOAPI（委托方要求）。
	//	断言 ProductName 而不是字面量 —— 品牌名可能再改，
	//	但"面板上必须显示当前产品名"这个契约不变。
	for _, want := range []string{ProductName, "app.js", "style.css", "model-body"} {
		if !strings.Contains(html, want) {
			t.Errorf("面板 HTML 缺少 %q", want)
		}
	}
}

// TestPanelAppJsCallsStats 验证面板脚本确实调用了统计接口。
func TestPanelAppJsCallsStats(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Get(srv.URL + "/panel/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	js := string(body)

	for _, want := range []string{"/api/stats", "/status", "setInterval"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %q", want)
		}
	}
}

// TestPanelRestartProbeUsesFourConditions 守面板侧的导航判据（Codex 第 19 轮）。
//
// 🔴 为什么必须有这条：面板的 pollRestart 是**纯 JS**，
// Go 的单元测试覆盖不到它的分支 —— 而这正是本轮出问题的地方
// （v1 用 no-cors 无法判断就绪；v2 只查 ok 会被通配 CORS 击穿）。
// 纯 JS 逻辑无法用 Go 测行为，**但可以钉住"判据的四个条件都写在代码里"**，
// 至少防止有人把它退回"只看 resolve/只看 ok"。
//
// 更强的行为验证在 docs/panel-dev/panel_acceptance.js 的真实 CDP 场景里。
func TestPanelRestartProbeUsesFourConditions(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Get(srv.URL + "/panel/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	js := string(body)

	// 1) 四条件的每一项都要在代码里出现
	//
	//    🔴 服务标识这一项**必须由后端常量推导**，不能写死字面量。
	//	本测试原先写死 `"h.service !== 'wbapi'"` ——
	//	产品改名 TAOAPI 后它仍在"守护"旧名字，**等于把缺陷钉死**：
	//	它保证了前端继续用 'wbapi'，于是重启后自动重连永远失败
	//	（2026-10-07 委托人实测：重启明明成功，面板报"未能连接"）。
	//	⇒ **会撒谎的检查器比没有检查器更糟。**
	//	现在改为拼出后端真实值，改名时两侧必须一起改，否则这条立刻红。
	for _, want := range []string{
		"h.service !== '" + healthServiceName + "'", // 条件 2：服务标识（与后端同源）
		"h.ready !== true",                          // 条件 3：就绪
		"h.restartId !== restartId",                 // 条件 4：本次交接标识
		"r.ok",                                      // 条件 1：HTTP 成功
	} {
		if !strings.Contains(js, want) {
			t.Errorf("settings.js 的探活判据缺少 %q —— "+
				"四条件缺一不可（详见 health.go 注释与维护备忘 §六之三）", want)
		}
	}

	// 2) 🔴 绝不能退回 no-cors：实测它让 503/404/200 全部 resolve，
	//    探针会退化成"端口是否有人监听"。
	//
	//    ⚠️ 只查**可执行代码**，不查注释：settings.js 里有三处注释
	//    在解释"当初为什么用 no-cors 是错的"（那是宝贵的背景，不该被删）。
	//    所以先剥离行注释再判断。
	if codeUsesNoCors(js) {
		t.Error("settings.js 的探活代码使用了 no-cors —— " +
			"实测 503/404/200 全部 resolve 且 opaque/status=0，无法区分是否就绪")
	}

	// 3) 必须把面板自身的 origin 带给服务端（否则新进程的 CORS 白名单
	//    缺旧 origin，浏览器拒绝读取 → 自动重连 100% 失效）。
	//
	//    ⚠️ 同样只查**可执行代码**：注释里也提到 location.origin，
	//    直接 Contains 会假阳性（反向对照实验发现的）。
	if !codeSendsPanelOrigin(js) {
		t.Error("settings.js 未把 location.origin 交给服务端 —— " +
			"新进程无法把旧 origin 加进 CORS 白名单，自动重连会失效")
	}
}

// TestPanelServiceNameMatchesBackend 守**跨端**的服务标识一致性。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么单独立一条（2026-10-07 委托人实测缺陷）
// ═══════════════════════════════════════════════════════════════════
//
//	产品 2026-10-06 改名 wbapi → TAOAPI，`healthServiceName` 跟着改了，
//	但**前端 settings.js 的 pollRestart 仍写死 'wbapi'**。
//	⇒ 导航四条件的第 2 条**永远不成立** ⇒ 面板一直探到 30 秒 deadline
//	  ⇒ giveUp() 报「重启后未能连接，请手动访问 http://127.0.0.1:<新端口>/panel/」。
//	  而**新端口其实一直是好的** —— 委托人手动访问就打开了。
//
//	症状极具误导性：面板说"未连接"，用户以为端口/占用出了问题，
//	实际是**重启完全成功、只是前端不认对面的身份**。
//
// ⚠️ 为什么既有测试全都没抓到：
//
//	后端侧的断言写的是 `hr.Service != ProductName`（自己比自己），
//	前端侧的断言写死了 `"h.service !== 'wbapi'"`（把缺陷钉死）。
//	**两边各自自洽，唯独没有一条测试把两侧放在一起比。**
//	本测试补的就是这个缺口 —— 它是唯一能抓住"只改一侧"的检查。
func TestPanelServiceNameMatchesBackend(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Get(srv.URL + "/panel/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	js := string(body)

	want := "h.service !== '" + healthServiceName + "'"

	// 只查可执行代码：注释里会引用旧名解释历史，那是背景，不该导致失败。
	var found bool
	for _, line := range strings.Split(js, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, want) {
			found = true
			break
		}
	}
	if found {
		return
	}

	// 没找到 → 再确认一下是不是"用了别的服务名"（给出精确诊断，别只说没找到）。
	// 这条诊断信息是本测试的价值所在：它能直接指出两侧的具体取值。
	code := stripLineComments(js)
	const marker = "h.service !=="
	if i := strings.Index(code, marker); i >= 0 {
		got := strings.TrimSpace(code[i+len(marker):])
		if j := strings.IndexAny(got, "|)"); j >= 0 {
			got = strings.TrimSpace(got[:j])
		}
		t.Errorf("前端 settings.js 期望 service=%s，而后端 healthServiceName=%q —— "+
			"两侧不一致 ⇒ 导航四条件第 2 条永不成立 ⇒ "+
			"重启后自动重连永远失败（面板会误报「重启后未能连接」）。"+
			"改名时必须两侧一起改。", got, healthServiceName)
		return
	}
	t.Errorf("settings.js 的探活判据里找不到服务标识判断 %q "+
		"（既然后端报 %q，前端必须逐字比对）", want, healthServiceName)
}

// stripLineComments 去掉每行的 `//` 行注释，只留可执行代码。
//
// 面板是手写 ES5，没有块注释包住代码的情况，简单剥离即可。
func stripLineComments(js string) string {
	var b strings.Builder
	for _, line := range strings.Split(js, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// codeSendsPanelOrigin 判断 JS 是否**在请求体里**带了 location.origin。
//
// 只看可执行代码（剥离行注释）：注释里提到 location.origin 不算。
func codeSendsPanelOrigin(js string) bool {
	for _, line := range strings.Split(js, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, "origin: location.origin") {
			return true
		}
	}
	return false
}

// codeUsesNoCors 判断 JS **可执行代码**里是否用了 no-cors（忽略注释）。
//
// 简单剥离 `//` 行注释即可：面板是手写 ES5，没有块注释包住代码的情况；
// 这里只需挡住"注释里提到 no-cors"造成的假阳性。
func codeUsesNoCors(js string) bool {
	for _, line := range strings.Split(js, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, "no-cors") {
			return true
		}
	}
	return false
}

// TestPanelAssetsServed 验证静态资源可访问且类型正确。
func TestPanelAssetsServed(t *testing.T) {

	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	cases := map[string]string{
		"/panel/app.js":    "javascript",
		"/panel/style.css": "text/css",
	}
	for path, wantType := range cases {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d", path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("%s Content-Type = %q，期望含 %q", path, ct, wantType)
		}
		if len(body) == 0 {
			t.Errorf("%s 内容为空", path)
		}
	}
}

// TestPanelRootRedirects 验证访问 / 跳到面板。
func TestPanelRootRedirects(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	// 不自动跟随重定向
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态码 = %d，期望 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/panel/" {
		t.Errorf("Location = %q，期望 /panel/", loc)
	}
}

// TestStatsEmptyStore 验证无数据时返回零值而不是报错。
func TestStatsEmptyStore(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Get(srv.URL + "/api/stats?days=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var got statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total.Requests != 0 {
		t.Errorf("requests = %d，期望 0", got.Total.Requests)
	}
	// 没有数据时 credits 必须为 null（区分"没有"与"消耗为 0"）
	if got.Credits != nil {
		t.Errorf("credits 应为 null，实际 %v", *got.Credits)
	}
	if len(got.Models) != 0 {
		t.Errorf("models 应为空，实际 %d", len(got.Models))
	}
}

// TestStatsAggregates 验证统计聚合正确。
func TestStatsAggregates(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	c1 := 0.5
	entries := []usagepkg.Event{
		{Time: time.Now(), Account: "a1", Model: "workbuddy/space-bunny",
			Protocol: "chat", OK: true, PromptTokens: 100, CompletionTokens: 50,
			TotalTokens: 150, ReasoningTokens: 20, CacheHitTokens: 80, CacheMissTokens: 20, Credit: &c1},
		{Time: time.Now(), Account: "a1", Model: "workbuddy/space-bunny",
			Protocol: "chat", OK: true, PromptTokens: 200, CompletionTokens: 100,
			TotalTokens: 300, ReasoningTokens: 40, CacheHitTokens: 150, CacheMissTokens: 50, Credit: &c1},
		{Time: time.Now(), Account: "a2", Model: "workbuddy/glm-5.3",
			Protocol: "chat", OK: false, Error: "上游 429"},
	}
	for _, e := range entries {
		if err := store.Append(e); err != nil {
			t.Fatal(err)
		}
	}

	srv := newPanelServer(t, store)
	resp, err := http.Get(srv.URL + "/api/stats?days=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	if got.Total.Requests != 3 {
		t.Errorf("requests = %d，期望 3", got.Total.Requests)
	}
	if got.Total.OK != 2 {
		t.Errorf("ok = %d，期望 2", got.Total.OK)
	}
	if got.Total.Failed != 1 {
		t.Errorf("failed = %d，期望 1", got.Total.Failed)
	}
	if got.Total.PromptTokens != 300 {
		t.Errorf("prompt_tokens = %d，期望 300", got.Total.PromptTokens)
	}
	if got.Total.TotalTokens != 450 {
		t.Errorf("total_tokens = %d，期望 450", got.Total.TotalTokens)
	}
	if got.Total.ReasoningTokens != 60 {
		t.Errorf("reasoning_tokens = %d，期望 60", got.Total.ReasoningTokens)
	}
	if got.Credits == nil {
		t.Fatal("credits 不应为 null（有两次带 credit 的记录）")
	}
	if *got.Credits != 1.0 {
		t.Errorf("credits = %v，期望 1.0", *got.Credits)
	}

	// 模型聚合：space-bunny 在前（token 多）
	if len(got.Models) != 2 {
		t.Fatalf("models 数 = %d，期望 2", len(got.Models))
	}
	if got.Models[0].Model != "workbuddy/space-bunny" {
		t.Errorf("第一个模型 = %q（应按 token 降序）", got.Models[0].Model)
	}
	if got.Models[0].TotalTokens != 450 {
		t.Errorf("space-bunny total = %d", got.Models[0].TotalTokens)
	}
	// 占比：450/450 = 1.0
	if got.Models[0].Share != 1.0 {
		t.Errorf("share = %v，期望 1.0", got.Models[0].Share)
	}

	// 账号聚合
	if len(got.Accounts) != 2 {
		t.Fatalf("accounts 数 = %d", len(got.Accounts))
	}
	if got.Accounts[0].Account != "a1" {
		t.Errorf("第一个账号 = %q（应按请求数降序）", got.Accounts[0].Account)
	}
}

// TestStatsCreditsNullWhenNoSample 验证所有记录都无 credit 时返回 null。
//
// 关键：不能用 0 冒充"上游没返回"。
func TestStatsCreditsNullWhenNoSample(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	_ = store.Append(usagepkg.Event{Time: time.Now(), Model: "m", OK: true, TotalTokens: 10})

	srv := newPanelServer(t, store)
	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got statsResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Credits != nil {
		t.Errorf("credits 应为 null（上游从未返回），实际 %v", *got.Credits)
	}
}

// TestStatsDaysParam 验证 days 参数解析。
func TestStatsDaysParam(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	srv := newPanelServer(t, store)

	cases := []struct {
		query string
		want  int
	}{
		{"?days=7", 7},
		{"?days=30", 30},
		{"?days=0", 1},       // 非法 → 默认 1
		{"?days=-5", 1},      // 负数 → 默认 1
		{"?days=abc", 1},     // 非数字 → 默认 1
		{"?days=99999", 365}, // 超上限 → 钳到 365
		{"", 1},              // 缺省
	}
	for _, tc := range cases {
		resp, err := http.Get(srv.URL + "/api/stats" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var got statsResponse
		_ = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()

		if got.RangeDays != tc.want {
			t.Errorf("days%q → %d，期望 %d", tc.query, got.RangeDays, tc.want)
		}
	}
}

// TestStatsMethodNotAllowed 验证非 GET 被拒。
func TestStatsMethodNotAllowed(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	resp, err := http.Post(srv.URL+"/api/stats", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestStatsNoStore 验证未配置存储时不崩溃。
func TestStatsNoStore(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", resp.StatusCode)
	}
}

// TestParseIntParam 验证参数解析边界。
func TestParseIntParam(t *testing.T) {
	cases := map[string]int{
		"":    99,
		"abc": 99,
		"-1":  99,
		"0":   0,
		"7":   7,
		"365": 365,
		"१२३": 99, // 非 ASCII 数字
	}
	for in, want := range cases {
		if got := parseIntParam(in, 99); got != want {
			t.Errorf("parseIntParam(%q) = %d，期望 %d", in, got, want)
		}
	}
}

// TestPanelEmbedHasNoDevFiles 守：开发用文件不得混进 go:embed。
//
// 背景（2026-10-05）：panel 目录曾同时放正式资源与开发用文件
// （接线片段 md、离线自测脚本 js），而 //go:embed panel/* 会把它们
// 一并编进 exe（约 34KB），并能通过 HTTP 被本机任何程序取到 —— 无谓的暴露。
//
// 现在开发用文件放在 docs/panel-dev/。这个测试守住边界：
// 一旦有人把开发文件放回 panel/，它会立刻变红。
func TestPanelEmbedHasNoDevFiles(t *testing.T) {
	allowed := map[string]bool{
		"index.html":   true,
		"app.js":       true,
		"style.css":    true,
		"settings.js":  true,
		"settings.css": true,
	}

	entries, err := panelFS.ReadDir("panel")
	if err != nil {
		t.Fatalf("读取内嵌面板目录失败: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			t.Errorf("内嵌面板目录不应有子目录: %s（应放到 docs/panel-dev/）", name)
			continue
		}
		if !allowed[name] {
			t.Errorf("内嵌面板目录出现非运行必需文件 %q；"+
				"开发用文件请放 docs/panel-dev/，避免被编进 exe 并经 HTTP 暴露", name)
		}
	}

	// 反向确认：必要资源确实都在（防止"清干净了但把要用的也删了"）
	for name := range allowed {
		if _, err := panelFS.ReadFile("panel/" + name); err != nil {
			t.Errorf("内嵌面板缺少必要资源 %s: %v", name, err)
		}
	}
}

// TestPanelServesSettingsAssets 守：设置页两个资源可访问且类型正确。
//
// 设置页是第 9 轮新增功能，前端与后端是分开开发的，
// 这个测试确保"后端把前端资源真的发出去了"这一环没断。
func TestPanelServesSettingsAssets(t *testing.T) {
	srv := newPanelServer(t, usagepkg.NewStore(t.TempDir()))

	cases := map[string]string{
		"/panel/settings.js":  "javascript",
		"/panel/settings.css": "text/css",
	}
	for path, wantType := range cases {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200", path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("%s Content-Type = %q，期望含 %q", path, ct, wantType)
		}
		if len(body) == 0 {
			t.Errorf("%s 内容为空", path)
		}
	}
}
