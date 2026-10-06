package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// health.go 的护栏（Codex 第 19 轮定稿的判据与 CORS 白名单）
//
// 这些测试守的是**真实抓到的坑**，不是想象中的边界：
//   - 坑 1/2：探活判据不能用"200 就算通"（实测被通配 CORS 的无关服务击穿）
//   - 坑 3：新服务只允许自己的 origin → 旧页面读不到 → 自动重连全废
//   - 坑 4：localhost 与 127.0.0.1 是不同的 Origin 字符串
// 详见 docs/维护备忘.md §六之三。
// ═══════════════════════════════════════════════════════════════════

// TestNormalizeLoopbackOriginAcceptsLoopback 守：合法回环 origin 被正确规范化。
func TestNormalizeLoopbackOriginAcceptsLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:8787", "http://127.0.0.1:8787"},
		{"http://localhost:8787", "http://localhost:8787"},
		// 🔴 localhost 与 127.0.0.1 必须**各自保持原样**，不能互相归一 ——
		// 它们是两个不同的 Origin 字符串（实测：白名单写错一个就匹配不上）。
		{"http://LOCALHOST:8787", "http://localhost:8787"}, // 大小写规范化
		{"http://127.0.0.2:9999", "http://127.0.0.2:9999"}, // 127.0.0.0/8 整段
		{"http://127.1.2.3:80", "http://127.1.2.3:80"},
		{"http://[::1]:8787", "http://[::1]:8787"},
	}
	for _, c := range cases {
		got, ok := normalizeLoopbackOrigin(c.in)
		if !ok {
			t.Errorf("%q 应被接受，却被拒绝", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("%q 规范化为 %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeLoopbackOriginRejectsBad 守：非法/危险 origin 必须被拒。
//
// 🔴 `Origin: null` 与"前缀相同但主机不同"是 Codex 点名的两类，
// 必须拒绝 —— 尤其后者，用前缀匹配就会放行。
func TestNormalizeLoopbackOriginRejectsBad(t *testing.T) {
	bad := []string{
		"",                        // 空
		"null",                    // Origin: null（沙箱/隐私模式会发）
		"http://evil.com",         // 非回环
		"https://127.0.0.1:8787",  // 协议不对（本服务只有 http）
		"http://127.0.0.1",        // 缺端口
		"http://0.0.0.0:8787",     // 非回环
		"http://[::]:8787",        // 非回环
		"http://192.168.1.1:8787", // 局域网地址
		// 🔴 前缀攻击：前缀是 http://127.0.0.1:8787，但真实主机是 evil.com。
		// 用 strings.HasPrefix 判断就会放行这个 —— 必须靠 URL 解析后的 Host 比对。
		"http://127.0.0.1:8787.evil.com",
		"http://127.0.0.1:8787@evil.com",
		"http://127.0.0.1:8787/path",    // 带 path
		"http://127.0.0.1:8787?x=1",     // 带 query
		"http://127.0.0.1:8787#frag",    // 带 fragment
		"http://user:pw@127.0.0.1:8787", // 带 userinfo
		"http://127.0.0.1:0",            // 端口越界
		"http://127.0.0.1:70000",        // 端口越界
		"http://127.0.0.1:abc",          // 端口非数字
		"http://127.0.0.1.5:8787",       // 段数不对
		"http://127.0.0.1:8787 ",        // 尾随空格（TrimSpace 后合法，见下）
	}
	for _, in := range bad {
		if in == "http://127.0.0.1:8787 " {
			// 尾随空格：TrimSpace 后是合法 origin，属"容忍输入噪声"，
			// 单独断言它被规范化（而不是被拒），避免把意图写反。
			got, ok := normalizeLoopbackOrigin(in)
			if !ok || got != "http://127.0.0.1:8787" {
				t.Errorf("尾随空格应被容忍并规范化，实际 ok=%v got=%q", ok, got)
			}
			continue
		}
		if got, ok := normalizeLoopbackOrigin(in); ok {
			t.Errorf("%q 应被拒绝，却被接受为 %q", in, got)
		}
	}
}

// TestNormalizeLoopbackOriginRejectsNullSpecifically 钉住 `null` 必须被拒。
//
// ⚠️ 一条诚实说明（反向对照实验 + 诊断实测得到的结论）：
//
//	我曾以为这条测试"守住了 `raw == "null"` 那行特判"。**其实没有。**
//	诊断实测：`url.Parse("null")` 得到 scheme="" / host="" / path="null" ——
//	它会被后面的"scheme 必须是 http""host 必须存在"两条拒掉，
//	所以**删掉 null 特判，本测试仍然全绿**。
//
//	也就是说 `raw == "null"` 那行在当前实现下是**冗余防御**（它让意图更明确、
//	将来重构时不易漏，但它不是唯一防线）。
//	我不把测试写成"它守住了那一行"—— 那会是假宣称。
//	这条测试真正守的是**行为契约**：null 及其变体必须被拒绝（无论靠哪一环）。
func TestNormalizeLoopbackOriginRejectsNullSpecifically(t *testing.T) {
	for _, variant := range []string{"null", "NULL", "Null", " null ", "null "} {
		if got, ok := normalizeLoopbackOrigin(variant); ok {
			t.Errorf("%q 必须被拒绝（null 的变体不能绕过），实际接受为 %q", variant, got)
		}
	}
}

// TestNormalizeLoopbackOriginHostIsWhatMatters 钉住"判定依据是回环 Host"。
//
// ⚠️ 又一条诚实说明（同上）：
//
//	我原本想用 `http://127.0.0.1:8787.evil.com` 这类"前缀合法、主机不同"
//	的串来证明"必须解析 Host 而不是比前缀"。**但那个证明不成立**：
//	Go 的 `url.Parse` 会直接把它判为
//	`invalid port ":8787.evil.com" after host` —— 它在**解析阶段**就失败了。
//
//	所以"前缀匹配会被击穿"这个担心对本实现**不成立**（URL 解析先挡住了）。
//	但不代表前缀匹配安全 —— 它安全只是因为恰好用了 url.Parse。
func TestNormalizeLoopbackOriginHostIsWhatMatters(t *testing.T) {
	// 字符串前缀合法、但由 url.Parse 在端口阶段拒绝。
	prefixSame := []string{
		"http://127.0.0.1:8787.evil.com",
		"http://127.0.0.1:8787x",
		"http://127.0.0.1:8787-evil",
	}
	for _, in := range prefixSame {
		if got, ok := normalizeLoopbackOrigin(in); ok {
			t.Errorf("%q 必须被拒绝（实际接受为 %q）", in, got)
		}
	}

	// 🔴 这条才是真正隔离"回环判定"的用例：**能成功解析**、端口合法，
	// 但主机名只是**以 "127." 开头**、并非合法回环 IP。
	//
	// 如果实现写成 `strings.HasPrefix(host, "127.")` 就会**误放行**它们
	// （我的 isLoopbackHostAny 用"四段数字 + 各段 0-255"校验挡住了）。
	deceptive := []string{
		"http://127.0.0.1.evil.com:8787", // Hostname 以 "127." 开头但不是 IP
		"http://127.evil.com:8787",       // 同上
		"http://127.0.0.1.5:8787",        // 五段数字
		"http://127.0.0.256:8787",        // 段值越界
	}
	for _, in := range deceptive {
		if got, ok := normalizeLoopbackOrigin(in); ok {
			t.Errorf("%q 以 \"127.\" 开头但**不是**合法回环 IP，必须拒绝"+
				"（实际接受为 %q）—— 说明判定只看了字符串前缀", in, got)
		}
	}
}

// TestNormalizeOriginEmitsCanonicalPort 守：输出的 ACAO 必须是**规范** origin 串。
//
// 🔴 缺陷来源（对抗式评审实测发现）：
//
//	原实现用 `u.Port()` 的**原始子串**拼输出，于是
//	`http://127.0.0.1:08788` 被"规范化"成 `http://127.0.0.1:08788`。
//	浏览器序列化 origin 时**从不带前导零**（它给的是 `:8788`），
//	而 ACAO 是**逐字节**比对的 —— 这个值永远匹配不上任何真实 origin，
//	白名单条目形同虚设（假阴性，功能静默失效）。
//
// 断言的是"输出等于用 Atoi 后的整数重建的串"，而不是仅"被接受"。
func TestNormalizeOriginEmitsCanonicalPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:08788", "http://127.0.0.1:8788"},
		{"http://127.0.0.1:00080", "http://127.0.0.1:80"},
		{"http://127.0.0.1:8788", "http://127.0.0.1:8788"},
		{"http://localhost:0088", "http://localhost:88"},
	}
	for _, c := range cases {
		got, ok := normalizeLoopbackOrigin(c.in)
		if !ok {
			t.Errorf("%q 应被接受", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("%q 规范化为 %q，期望 %q —— "+
				"端口必须用解析后的整数重建，否则 ACAO 逐字节比对永远不匹配", c.in, got, c.want)
		}
	}
}

// TestAllowedOriginKeysAreCanonical 守：白名单的 key 与查询值都走同一规范化。
//
// 🔴 关键点：`:08788` 与 `:8788` 规范后**必须是同一个 key**，
// 否则用户恰好用了带前导零的形式时会被判为不同 origin。
func TestAllowedOriginKeysAreCanonical(t *testing.T) {
	a := newAllowedOrigins("http://127.0.0.1:08788")
	if !a.Allows("http://127.0.0.1:8788") {
		t.Error("带前导零登记的 origin 应能匹配规范形式（两者规范化后应相同）")
	}
	if !a.Allows("http://127.0.0.1:08788") {
		t.Error("带前导零的查询也应收敛到同一个 key")
	}
}

// TestValidRestartIDRejectsEmptyDocumented 守"空串非法"这一契约。
//
// 🔴 为什么单列一条（对抗式评审发现）：
//
//	`validRestartID` 的注释曾写"空串或 32 位小写 hex"，**与实现矛盾**
//	（实测空串返回 false），且 health_test.go 把 "" 列在 bad 里 —— 三处说法不一。
//	当前无运行时影响（serve.go 先判非空才调用），但注释与实现矛盾
//	会让后来者写出错误假设。这条测试把契约钉死为"空串必须非法"。
func TestValidRestartIDRejectsEmptyDocumented(t *testing.T) {
	if validRestartID("") {
		t.Error("空串必须是非法标识 —— 注释与实现必须一致" +
			"（调用方负责先判非空，本函数不把空串当合法）")
	}
}

func TestAllowedOriginsWhitelist(t *testing.T) {
	a := newAllowedOrigins("http://127.0.0.1:8787", "http://localhost:8787")

	if !a.Allows("http://127.0.0.1:8787") {
		t.Error("登记过的 127.0.0.1 origin 应放行")
	}
	if !a.Allows("http://localhost:8787") {
		t.Error("登记过的 localhost origin 应放行")
	}
	// 关键：这两个是不同的 Origin，只有真的都在白名单里才都放行。
	if a.Allows("http://127.0.0.1:8788") {
		t.Error("未登记的端口不应放行")
	}
	if a.Allows("http://localhost:8788") {
		t.Error("未登记的端口不应放行")
	}
	if a.Allows("") {
		t.Error("空 origin 不应放行")
	}
}

// TestHealthCORSAllowsOldOrigin 守坑 3：**旧** origin 必须能读到健康响应。
//
// 🔴 这是"改端口重启后自动重连"能否工作的关键：
// 面板在旧 origin 上探测新端口，浏览器发的 Origin **仍是旧 origin**。
// 新服务若只允许自己的 origin，浏览器会直接拒绝读取（实测 Failed to fetch），
// 自动重连 100% 失效。
func TestHealthCORSAllowsOldOrigin(t *testing.T) {
	const oldOrigin = "http://localhost:8787" // 旧地址（注意是 localhost）
	const newOrigin = "http://127.0.0.1:8788" // 新地址

	// 新进程的白名单 = {新 origin, 本次交接的旧 origin}
	a := newAllowedOrigins(newOrigin, oldOrigin)
	h := healthHandler(a, "abc")

	srv := httptest.NewServer(h)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", oldOrigin)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	got := rec.Header().Get("Access-Control-Allow-Origin")
	if got != oldOrigin {
		t.Errorf("旧 origin 的 ACAO = %q，期望 %q —— "+
			"不含旧 origin 会让浏览器拒绝读取，自动重连全废", got, oldOrigin)
	}
	if v := rec.Header().Get("Vary"); v != "Origin" {
		t.Errorf("Vary = %q，期望 Origin（否则缓存可能跨 origin 复用）", v)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", cc)
	}
	_ = srv
}

// TestHealthCORSBlocksUnknownOrigin 守：未登记的 origin 拿不到 CORS 头。
//
// 注意断言的是"**没有** ACAO 头"（消极行为），而不是"返回 403" ——
// /healthz 本身是公开的回环健康检查，不该因为带了个陌生 Origin 就 4xx；
// 浏览器会因为缺少 CORS 头而拒绝把响应交给页面，这正是我们要的效果。
func TestHealthCORSBlocksUnknownOrigin(t *testing.T) {
	h := healthHandler(newAllowedOrigins("http://127.0.0.1:8787"), "abc")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("未登记 origin 的 ACAO = %q，必须为空（否则任意网页都能读健康状态）", got)
	}
	// 绝不能是通配 —— Codex 第 12 轮已否决通配 CORS，这里是同一原则。
	if strings.Contains(rec.Header().Get("Access-Control-Allow-Origin"), "*") {
		t.Error("不得使用通配 CORS")
	}
}

// TestHealthNeverUsesWildcardOrCredentials 守：CORS 永不通配、永不启用凭据。
func TestHealthNeverUsesWildcardOrCredentials(t *testing.T) {
	a := newAllowedOrigins("http://127.0.0.1:8787", "http://localhost:8787")
	h := healthHandler(a, "abc")

	for _, origin := range []string{"http://127.0.0.1:8787", "https://evil.example", ""} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h(rec, req)

		if v := rec.Header().Get("Access-Control-Allow-Origin"); v == "*" {
			t.Errorf("origin=%q 时 ACAO 为通配，不允许", origin)
		}
		if v := rec.Header().Get("Access-Control-Allow-Credentials"); v != "" {
			t.Errorf("origin=%q 时不得启用凭据 CORS，实际 %q", origin, v)
		}
	}
}

// TestHealthEchoesServiceAndRestartID 守：健康响应的字段契约。
func TestHealthEchoesServiceAndRestartID(t *testing.T) {
	const id = "00112233445566778899aabbccddeeff"
	h := healthHandler(nil, id)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 JSON（面板要解析 body 判 service/ready）", ct)
	}
	var hr healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hr); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if hr.Service != ProductName {
		t.Errorf("service = %q，期望 wbapi", hr.Service)
	}
	if !hr.Ready {
		t.Error("ready 应为 true")
	}
	if hr.RestartID != id {
		t.Errorf("restartId = %q，期望 %q", hr.RestartID, id)
	}
}

// TestHealthOmitsRestartIDWhenEmpty 守：非重启启动时省略 restartId。
func TestHealthOmitsRestartIDWhenEmpty(t *testing.T) {
	h := healthHandler(nil, "")
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if _, present := raw["restartId"]; present {
		t.Error("非重启启动时不应出现 restartId 字段（omitempty）")
	}
}

// TestValidRestartID 守交接标识的形态校验。
//
// 🔴 "缺失、格式错误或不匹配时健康握手必须失败"（Codex 要求）：
// 一个畸形标识被回显出去，会让面板误判"对面是本次交接的进程"。
func TestValidRestartID(t *testing.T) {
	good := []string{
		"00112233445566778899aabbccddeeff",
		"ffffffffffffffffffffffffffffffff",
		"0123456789abcdef0123456789abcdef",
	}
	for _, s := range good {
		if !validRestartID(s) {
			t.Errorf("%q 应被接受", s)
		}
	}
	bad := []string{
		"",
		"00112233445566778899aabbccddeef",   // 31 位
		"00112233445566778899aabbccddeeff0", // 33 位
		"00112233445566778899AABBCCDDEEFF",  // 大写
		"00112233445566778899aabbccddeefg",  // 非 hex
		"001122334455667 8899aabbccddeeff",  // 含空格
		"../../../etc/passwd",               // 明显恶意
	}
	for _, s := range bad {
		if validRestartID(s) {
			t.Errorf("%q 应被拒绝", s)
		}
	}
}

// TestNewRestartIDIsUnique 守：每次重启生成新标识（不复用）。
func TestNewRestartIDIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id, err := newRestartID()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if !validRestartID(id) {
			t.Fatalf("生成的标识 %q 不合法", id)
		}
		if seen[id] {
			t.Fatalf("标识重复: %q —— 复用会让上次遗留的旧进程也匹配成功", id)
		}
		seen[id] = true
	}
}

// TestAllowedOriginsNotAccumulating 守：白名单每次重建，不累积历次旧 origin。
//
// 🔴 Codex 明确要求：否则白名单会随重启次数无界增长，
// 把历史上用过的所有地址一直留在"可读健康状态"的范围内。
func TestAllowedOriginsNotAccumulating(t *testing.T) {
	// 模拟三次重启，每次都**只**用本次的两个 origin 构造
	rounds := [][2]string{
		{"http://127.0.0.1:8787", "http://127.0.0.1:8788"},
		{"http://127.0.0.1:8788", "http://127.0.0.1:8789"},
		{"http://127.0.0.1:8789", "http://127.0.0.1:8790"},
	}
	for i, r := range rounds {
		a := newAllowedOrigins(r[0], r[1])
		if n := len(a.set); n != 2 {
			t.Errorf("第 %d 轮白名单大小 = %d，期望 2（只含本次两个）", i+1, n)
		}
	}
}
