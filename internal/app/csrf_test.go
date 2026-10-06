package app

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
)

// 本文件守住管理接口的 CSRF 防护（Codex 第 11、12 轮连续要求）。
//
// 🔴 威胁模型：`/api/*` 不校验 API key（只监听回环 + 面板要能在未设 key
// 时打开）。于是"你浏览器里打开的恶意网页"可以 fetch 到本接口改 key、
// 触发重启、禁用账号。
//
// 🔴 主防线是**自定义头 `X-WBAPI-Panel`**（不是 Origin —— 我最初的
// "Origin 缺失就放行"方案被 Codex 否决，理由见 csrf.go 顶部注释）。
// 浏览器跨站请求带自定义头会先触发 CORS 预检，而我们不应答预检，
// 于是请求被浏览器自己拦下。
//
// 明确接受：本机其他进程【知道 token 的话】能改配置 ——
// 能跑本机进程的攻击者能做的事远不止这些。我们要防的是浏览器里的网页。

// testPanelToken 是测试用的固定面板令牌。
//
// 真实实现每次启动随机生成（见 newPanelToken）；测试用固定值
// 便于构造"带对 / 带错 / 不带"三种请求。
const testPanelToken = "test-panel-token-0123456789abcdef"

// portOf 从 httptest 的 URL 里取出端口。
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", rawURL, err)
	}
	p := splitHostPort(u.Host)
	if p == 0 {
		t.Fatalf("从 %q 取不到端口", rawURL)
	}
	return p
}

// newCSRFEnv 起一个"监听端口已知"的服务，便于构造自身 origin。
//
// ⚠️ Deps 是【按值传递】给 newMux 的，所以 ListenPort 必须在建 mux 之前
// 填好 —— 否则 origin 校验拿到 0，会走"不校验端口"的分支，
// "端口不匹配应拒绝"那个测试就测不到东西了。
func newCSRFEnv(t *testing.T) (*httptest.Server, *config.Store) {
	t.Helper()
	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	port := splitHostPort(addr)
	_ = probe.Close()

	deps := Deps{
		Logger:     log.New(io.Discard, "", 0),
		Settings:   store,
		Router:     router.New(),
		ListenPort: port, // 必须在 newMux 之前填
		panelToken: testPanelToken,
	}

	srv := httptest.NewUnstartedServer(newMux(deps))
	_ = srv.Listener.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("复用端口 %s 失败: %v", addr, err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, store
}

// doWithHeaders 发请求并返回状态码。
func doWithHeaders(t *testing.T, method, url, body string, h map[string]string) int {
	t.Helper()
	body = autoIfRevision(t, method, url, body)
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestCSRFBlocksCrossOriginWrite 是核心：跨站写请求必须被 403 拒绝。
func TestCSRFBlocksCrossOriginWrite(t *testing.T) {
	srv, store := newCSRFEnv(t)
	before := store.Get().APIKey

	evil := []string{
		"http://evil.example.com",
		"https://attacker.test",
		"http://127.0.0.1.evil.com", // 前缀伪装
		"http://localhost.evil.com",
		"null", // 沙箱 iframe / file:// 场景
	}
	for _, origin := range evil {
		code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
			`{"apiKey":"pwned"}`,
			map[string]string{"Origin": origin, "Content-Type": "application/json", panelTokenHeader: testPanelToken})
		if code != http.StatusForbidden {
			t.Errorf("Origin=%q 状态码 = %d，期望 403（跨站写必须被拒）", origin, code)
		}
	}
	if store.Get().APIKey != before {
		t.Fatalf("跨站请求改动了配置！key=%q", store.Get().APIKey)
	}
}

// TestCSRFBlocksWrongPortOrigin 守：端口不匹配也拒绝。
//
// 改了端口但还没重启时，旧 origin 不应被信任。
func TestCSRFBlocksWrongPortOrigin(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9999}`,
		map[string]string{"Origin": "http://127.0.0.1:1", "Content-Type": "application/json", panelTokenHeader: testPanelToken})
	if code != http.StatusForbidden {
		t.Errorf("端口不符的 Origin 状态码 = %d，期望 403", code)
	}
}

// TestCSRFAllowsSameOriginWrite 守：面板自己发起的写请求必须放行。
//
// 若这条红了，说明面板设置页会完全无法保存 —— 功能被打死。
func TestCSRFAllowsSameOriginWrite(t *testing.T) {
	srv, store := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9100}`,
		map[string]string{"Origin": srv.URL, "Content-Type": "application/json", panelTokenHeader: testPanelToken})
	if code != http.StatusOK {
		t.Fatalf("同源写请求状态码 = %d，期望 200（否则面板无法保存）", code)
	}
	if store.Get().Port != 9100 {
		t.Errorf("同源写请求未生效，port=%d", store.Get().Port)
	}

	// localhost 形式也应接受（用户可能用 localhost 打开面板）
	code = doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"autoCheckin":true}`,
		map[string]string{
			"Origin":         "http://localhost:" + itoa(portOf(t, srv.URL)),
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusOK {
		t.Errorf("localhost 形式的同源请求状态码 = %d，期望 200", code)
	}
}

// TestCSRFRejectsRefererMismatch 守：只带 Referer 且不匹配时也拒绝。
func TestCSRFRejectsRefererMismatch(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"autoCheckin":true}`,
		map[string]string{
			"Referer":      "http://evil.example.com/page.html",
			"Content-Type": "application/json", panelTokenHeader: testPanelToken,
		})
	if code != http.StatusForbidden {
		t.Errorf("恶意 Referer 状态码 = %d，期望 403", code)
	}
}

// TestCSRFAllowsNoOriginForCLI 守：脚本（无 Origin 无 Referer）**带对令牌**时放行。
//
// 注意语义变化（Codex 第 12 轮要求）：脚本放行的前提是**带对令牌**，
// 而不是"没有 Origin 所以放行"。后者是我最初的方案，已被否决 ——
// 把"通常不带"当成"一定不带"是防线漏洞。
func TestCSRFAllowsNoOriginForCLI(t *testing.T) {
	srv, store := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9200}`,
		map[string]string{"Content-Type": "application/json", panelTokenHeader: testPanelToken})
	if code != http.StatusOK {
		t.Fatalf("带对令牌的脚本请求状态码 = %d，期望 200", code)
	}
	if store.Get().Port != 9200 {
		t.Errorf("脚本请求未生效")
	}
}

// TestCSRFRequiresTokenEvenWithoutOrigin 是 Codex 第 12 轮要求的核心测试。
//
// 🔴 我最初的方案是"Origin 缺失就放行"。Codex 否决：跨站表单、隐私策略、
// 特殊 WebView、Origin: null 都可能不按预期出现，"通常带"不等于"一定带"。
// 现在改成**必须带令牌**：即使不带任何 Origin，没有令牌也必须 403。
func TestCSRFRequiresTokenEvenWithoutOrigin(t *testing.T) {
	srv, store := newCSRFEnv(t)
	before := store.Get().APIKey

	// 完全没有 Origin / Referer，也没有令牌 —— 这是"我以为安全的脚本请求"
	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"apiKey":"pwned"}`,
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusForbidden {
		t.Errorf("无 Origin 且无令牌 状态码 = %d，期望 403（不能靠'没有 Origin'获得写权限）", code)
	}
	if store.Get().APIKey != before {
		t.Fatal("无令牌请求改动了配置！")
	}
}

// TestCSRFRejectsWrongToken 守：令牌不对必须拒绝。
func TestCSRFRejectsWrongToken(t *testing.T) {
	srv, store := newCSRFEnv(t)
	before := store.Get().APIKey

	for _, bad := range []string{
		"",
		"wrong-token",
		testPanelToken + "x", // 前缀正确但多了字符
		testPanelToken[:8],   // 只给一半
	} {
		code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
			`{"apiKey":"pwned"}`,
			map[string]string{
				"Content-Type":   "application/json",
				panelTokenHeader: bad,
			})
		if code != http.StatusForbidden {
			t.Errorf("令牌=%q 状态码 = %d，期望 403", bad, code)
		}
	}
	if store.Get().APIKey != before {
		t.Fatal("错误令牌改动了配置！")
	}
}

// TestCSRFBlocksTokenViaCrossOrigin 守：即使令牌对了，跨站来源仍拒绝。
//
// 双层防护：万一令牌泄露（例如日志），来源校验还能挡一层。
func TestCSRFBlocksTokenViaCrossOrigin(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9300}`,
		map[string]string{
			"Origin":         "http://evil.example.com",
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusForbidden {
		t.Errorf("令牌正确但来源跨站 状态码 = %d，期望 403", code)
	}
}

// TestCSRFRejectsNullOrigin 守：Origin: null 即使令牌正确也拒绝。
//
// null 来自沙箱 iframe / file:// —— 正常面板不可能从那里发起请求。
func TestCSRFRejectsNullOrigin(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9400}`,
		map[string]string{
			"Origin":         "null",
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusForbidden {
		t.Errorf("Origin: null 状态码 = %d，期望 403", code)
	}
}

// TestPanelTokenEndpoint 守：面板能取到令牌（否则面板无法保存）。
func TestPanelTokenEndpoint(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	resp, err := http.Get(srv.URL + "/api/panel-token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["token"] != testPanelToken {
		t.Errorf("令牌 = %q，期望 %q", got["token"], testPanelToken)
	}
}

// TestCSRFAllowsCrossOriginRead 守：GET 不拦。
//
// 读操作无副作用，且不应影响本机脚本与监控；
// 跨站读也读不到内容（同源策略会挡住响应）。
func TestCSRFAllowsCrossOriginRead(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodGet, srv.URL+"/api/settings", "",
		map[string]string{"Origin": "http://evil.example.com"})
	if code != http.StatusOK {
		t.Errorf("跨站 GET 状态码 = %d，期望 200（读操作不拦）", code)
	}
}

// TestCSRFRequiresJSONContentType 守：写请求要求 JSON Content-Type。
//
// 这是一道不依赖 Origin 的防线：跨站的"简单请求"只能用
// form-urlencoded / multipart / text-plain，改不了 Content-Type。
func TestCSRFRequiresJSONContentType(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"multipart/form-data",
		"text/plain",
	} {
		code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
			`{"autoCheckin":true}`,
			map[string]string{"Content-Type": ct, panelTokenHeader: testPanelToken})
		if code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type=%q 状态码 = %d，期望 415", ct, code)
		}
	}

	// 无 body 的 POST（如 restart）允许不写 Content-Type
	code := doWithHeaders(t, http.MethodPost, srv.URL+"/api/settings/restart", "",
		map[string]string{panelTokenHeader: testPanelToken})
	if code == http.StatusUnsupportedMediaType {
		t.Error("无 body 的 POST 不应因缺少 Content-Type 被拒")
	}
}

// TestCSRFBlocksAccountAPI 守：账号接口同样受保护（不只是 settings）。
//
// action 能禁用账号、import 能写凭据 —— 漏掉它们等于防护形同虚设。
func TestCSRFBlocksAccountAPI(t *testing.T) {
	srv, _ := newCSRFEnv(t)

	code := doWithHeaders(t, http.MethodPost, srv.URL+"/api/accounts/action",
		`{"action":"checkin_all"}`,
		map[string]string{"Origin": "http://evil.example.com",
			"Content-Type": "application/json", panelTokenHeader: testPanelToken})
	if code != http.StatusForbidden {
		t.Errorf("跨站调用账号 action 状态码 = %d，期望 403", code)
	}

	code = doWithHeaders(t, http.MethodPost, srv.URL+"/api/accounts/import",
		`{"accounts":[]}`,
		map[string]string{"Origin": "http://evil.example.com",
			"Content-Type": "application/json", panelTokenHeader: testPanelToken})
	if code != http.StatusForbidden {
		t.Errorf("跨站调用账号 import 状态码 = %d，期望 403", code)
	}
}

// TestOriginMatchesRules 单测 origin 判定规则（边界）。
func TestOriginMatchesRules(t *testing.T) {
	cases := []struct {
		origin string
		port   int
		want   bool
	}{
		{"http://127.0.0.1:8787", 8787, true},
		{"http://localhost:8787", 8787, true},
		{"http://[::1]:8787", 8787, true},
		{"http://127.0.0.1:9999", 8787, false},
		{"http://evil.com:8787", 8787, false},
		{"http://127.0.0.1.evil.com:8787", 8787, false},
		{"http://127.0.0.1", 80, true},
		{"not a url", 8787, false},
		{"", 8787, false},
	}
	for _, c := range cases {
		if got := originMatches(c.origin, c.port); got != c.want {
			t.Errorf("originMatches(%q, %d) = %v，期望 %v", c.origin, c.port, got, c.want)
		}
	}
}
