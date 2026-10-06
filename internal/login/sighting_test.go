// sighting_test.go：钉住"登录失败原因必须可诊断"。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要这些测试（2026-10-05 排障的真实代价）
// ═══════════════════════════════════════════════════════════════════
//
// 凭据捕获链路的每种失败原先都是**静默的**，外部只看到"超时"。
// 我因此在同一次排障里**连续误判三次**：
//
//	① 把 `http` 被 `https` 校验拒绝 → 当成生产缺陷
//	② 把自己诊断工具的竞态（钩子注册晚于事件） → 当成生产缺陷
//	③ 把 `已登记候选=0` 当成匹配逻辑坏了 → 其实是①
//
// 每次误判都要重新抓包、重新猜。若当时超时错误能带上
// "见到凭据端点形态的请求但被拒绝（非 https×1）"，
// 这三轮弯路全都可以避免。
//
// ⇒ 这些测试守住"诊断信息存在、准确、且不泄露"。
package login

import (
	"strings"
	"testing"
)

// TestSightingIdentifiesNonHTTPS 钉住最常见的一种拒绝原因。
//
// 这**正是** 2026-10-05 那次误判的场景：本地假服务端是 http，
// 生产校验要求 https，于是被正确拒绝 —— 但当时没有诊断信息，
// 表现为"事件收到了却匹配数=0"，被误读成缺陷。
func TestSightingIdentifiesNonHTTPS(t *testing.T) {
	s := newSightingLog()
	s.observe(newProdCredMatcher(), "http://127.0.0.1:1234/console/login/enterprise?state=abc", credentialEndpointMethod, "abc")

	got := s.summary()
	if got == "" {
		t.Fatal("http 端点被拒绝时应产生诊断信息（否则又会表现为静默超时）")
	}
	if !strings.Contains(got, rejectScheme) {
		t.Errorf("诊断应指出『%s』，实际: %s", rejectScheme, got)
	}
}

// TestSightingIdentifiesStateMismatch 钉住 state 不一致的诊断。
func TestSightingIdentifiesStateMismatch(t *testing.T) {
	s := newSightingLog()
	s.observe(newProdCredMatcher(), "https://www.codebuddy.cn/console/login/enterprise?state=other", credentialEndpointMethod, "want")

	got := s.summary()
	if !strings.Contains(got, rejectState) {
		t.Errorf("应指出『%s』，实际: %s", rejectState, got)
	}
}

// TestSightingIdentifiesHostMismatch 钉住 host 不一致的诊断。
func TestSightingIdentifiesHostMismatch(t *testing.T) {
	s := newSightingLog()
	s.observe(newProdCredMatcher(), "https://evil.example.com/console/login/enterprise?state=want", credentialEndpointMethod, "want")

	got := s.summary()
	if !strings.Contains(got, rejectHost) {
		t.Errorf("应指出『%s』，实际: %s", rejectHost, got)
	}
}

// TestSightingIgnoresUnrelatedTraffic 断言无关请求不产生**拒绝类**噪音。
//
// 登录页会发大量 CDN/埋点请求；若它们都进"被拒绝"统计，
// 真正的原因会被淹没 —— 那等于没有诊断。
//
// ⚠️ 2026-10-05 行为变更：summary 现在会在**完全没收到事件**时
// 返回"未收到任何 CDP 事件（Network 域可能未生效）"——
// 因为那本身就是必须看得见的异常（否则只看到"超时"）。
// 所以这里断言的是"**不报拒绝原因**"，而不是"摘要为空"。
func TestSightingIgnoresUnrelatedTraffic(t *testing.T) {
	s := newSightingLog()
	s.observe(newProdCredMatcher(), "https://download.codebuddy.cn/web/login/x/assets/index.js", credentialEndpointMethod, "want")
	s.observe(newProdCredMatcher(), "https://www.google.com/rmkt/collect/123", credentialEndpointMethod, "want")
	s.observe(newProdCredMatcher(), "https://www.codebuddy.cn/console/accounts", credentialEndpointMethod, "want")

	got := s.summary()
	if strings.Contains(got, "被拒绝") {
		t.Errorf("无关请求不应被记成『被拒绝』，实际: %s", got)
	}
	if strings.Contains(got, "方法校验") {
		t.Errorf("无关请求不应触发方法校验统计，实际: %s", got)
	}
}

// TestSightingReportsNoEventsAtAll 钉住"一个事件都没收到"必须可见。
//
// 这是 Codex 第 31 轮精神的延伸：失败不能退化成一句无法定位的"超时"。
// 若 Network 域没生效，摘要必须**明说**，而不是留空。
func TestSightingReportsNoEventsAtAll(t *testing.T) {
	s := newSightingLog()
	got := s.summary()
	if !strings.Contains(got, "未收到任何 CDP 事件") {
		t.Errorf("一个事件都没有时应明确报告，实际: %q", got)
	}
}

// TestSightingReportsEventsWithoutTarget 钉住"收到事件但没见到目标端点"。
func TestSightingReportsEventsWithoutTarget(t *testing.T) {
	s := newSightingLog()
	s.noteEvent("Network.responseReceived")
	s.noteRespURL("https://www.codebuddy.cn/console/accounts")

	got := s.summary()
	if !strings.Contains(got, "未见到目标端点") {
		t.Errorf("应报告『收到事件但未见到目标端点』，实际: %s", got)
	}
	// 必须列出实际见过的 path —— 用于区分"路径不同"与"识别写错"。
	if !strings.Contains(got, "/console/accounts") {
		t.Errorf("应列出实际见过的 path，实际: %s", got)
	}
}

// TestSightingReportsActualMethod 钉住 Codex 第 31 轮的核心要求：
// 方法校验失败时必须能区分"元数据缺失"与"真实方法不符"。
func TestSightingReportsActualMethod(t *testing.T) {
	// 情形①：元数据缺失（方法为空）。
	s1 := newSightingLog()
	s1.observeRejected(rejectMethod)
	s1.observeRejectedMethod("")
	got1 := s1.summary()
	if !strings.Contains(got1, "(缺失)") {
		t.Errorf("元数据缺失应显式标成『(缺失)』，实际: %s", got1)
	}

	// 情形②：真实方法不符（OPTIONS 预检）。
	s2 := newSightingLog()
	s2.observeRejected(rejectMethod)
	s2.observeRejectedMethod("OPTIONS")
	got2 := s2.summary()
	if !strings.Contains(got2, "OPTIONS") {
		t.Errorf("应报出实际方法 OPTIONS，实际: %s", got2)
	}

	// 两者必须**可区分** —— 这正是 Codex 要求的点。
	if got1 == got2 {
		t.Error("『元数据缺失』与『真实方法不符』的摘要完全相同 —— 无法区分")
	}
}

// TestSightingAcceptPathProducedNoRejection 断言"能匹配"时不报拒绝。
//
// ⚠️ 这条很重要：`rejectReason` 必须与 `isCredentialEndpoint`
// **判定一致**。若两者不一致，诊断会指向错误原因 —— 比没有诊断更糟。
func TestSightingAcceptPathProducedNoRejection(t *testing.T) {
	const state = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	u := "https://www.codebuddy.cn/console/login/enterprise?state=" + state

	s := newSightingLog()
	s.observe(newProdCredMatcher(), u, credentialEndpointMethod, state)

	if got := s.summary(); strings.Contains(got, "拒绝") {
		t.Errorf("该 URL 本应能匹配，诊断却说被拒绝: %s", got)
	}

	// 交叉验证：真正用于判定的函数也必须接受它。
	if !isCredentialEndpoint(u, state) {
		t.Fatal("isCredentialEndpoint 拒绝了它 —— 两个判定不一致")
	}
}

// TestSightingSummaryNeverLeaksURL 是**安全断言**：
// 诊断信息绝不能含完整 URL 或 state。
//
// 🔴 安全红线：token 不进日志/事件/API 响应。
// 这里额外收紧：连 URL 与 state 也不进 —— 它们不是凭据，
// 但没有理由留，留着只会增加将来泄露的机会。
func TestSightingSummaryNeverLeaksURL(t *testing.T) {
	s := newSightingLog()
	s.observe(newProdCredMatcher(), "http://127.0.0.1:9999/console/login/enterprise?state=SECRETSTATEVALUE", credentialEndpointMethod, "SECRETSTATEVALUE")

	got := s.summary()
	for _, leak := range []string{"SECRETSTATEVALUE", "127.0.0.1", "9999", "http://"} {
		if strings.Contains(got, leak) {
			t.Errorf("诊断信息泄露了 %q：%s", leak, got)
		}
	}
}

// TestSightingCountsAreAggregated 断言同类原因被合并计数。
//
// 登录页可能重复请求；逐条列出会淹没信息。
func TestSightingCountsAreAggregated(t *testing.T) {
	s := newSightingLog()
	for i := 0; i < 3; i++ {
		s.observe(newProdCredMatcher(), "http://127.0.0.1:1/console/login/enterprise?state=s", credentialEndpointMethod, "s")
	}
	if got := s.summary(); !strings.Contains(got, "×3") {
		t.Errorf("同类原因应合并计数为 ×3，实际: %s", got)
	}
}

// TestSightingFetchFailureIsReported 断言"匹配成功但取 body 失败"也被报告。
//
// 这类失败与"没匹配上"的修法完全不同，必须能区分。
func TestSightingFetchFailureIsReported(t *testing.T) {
	s := newSightingLog()
	s.noteFetchFailed("CDP 调用超时")

	got := s.summary()
	if !strings.Contains(got, "取响应体失败") {
		t.Errorf("应报告取 body 失败，实际: %s", got)
	}
	if !strings.Contains(got, "CDP 调用超时") {
		t.Errorf("应报告具体原因，实际: %s", got)
	}
}

// TestRejectReasonAgreesWithMatcher 是**元测试**：
// 对一组 URL，`rejectReason` 为空 ⟺ `isCredentialEndpoint` 为真。
//
// 这个不变量一旦破坏，诊断就会误导排障方向 —— 而这正是
// 本轮排障最贵的教训（三次误判里有两次源于"看到的不是真相"）。
func TestRejectReasonAgreesWithMatcher(t *testing.T) {
	const want = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	cases := []string{
		"https://www.codebuddy.cn/console/login/enterprise?state=" + want,
		"https://www.codebuddy.cn/console/login/enterprise?state=other",
		"https://www.codebuddy.cn/console/login/enterprise",
		"http://www.codebuddy.cn/console/login/enterprise?state=" + want,
		"https://evil.com/console/login/enterprise?state=" + want,
		"https://www.codebuddy.cn/console/login/enterprisX?state=" + want,
		"https://www.codebuddy.cn/console/login/enterprise?state=" + want + "&state=" + want,
	}

	for _, u := range cases {
		m := newProdCredMatcher()
		matched := m.matches(u, credentialEndpointMethod, want)
		reason := m.rejectReason(u, credentialEndpointMethod, want)

		if matched && reason != "" {
			t.Errorf("不一致：匹配成功却报拒绝原因 %q\n  url=%s", reason, u)
		}
		if !matched && reason == "" {
			t.Errorf("不一致：匹配失败却没给出拒绝原因\n  url=%s", u)
		}
	}
}

// TestProdMatcherRejectsLocalHTTP 是 Codex 第 29 轮**点名要求**的断言：
// 生产匹配器必须拒绝本地 HTTP 端点（接缝不能污染生产严格性）。
//
// 为什么单独钉：我加了一个测试接缝让捕获器能连本地假服务端。
// 若接缝"顺手"放宽了生产校验，就会出现"测试全绿但生产抓不到凭据"
// —— 而那正是本次事故的形态。
func TestProdMatcherRejectsLocalHTTP(t *testing.T) {
	const state = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	m := newProdCredMatcher()

	local := "http://127.0.0.1:8080/console/login/enterprise?state=" + state
	if m.matches(local, credentialEndpointMethod, state) {
		t.Error("❌ 生产匹配器接受了本地 HTTP 端点 —— 严格性被破坏了")
	}
	if got := m.rejectReason(local, credentialEndpointMethod, state); got != rejectScheme {
		t.Errorf("拒绝原因应为 %q，得到 %q", rejectScheme, got)
	}
	t.Log("✅ 生产匹配器正确拒绝本地 HTTP（诊断原因：非 https）")
}

// TestProdMatcherRequiresPOST 钉住 Codex 第 29 轮的方法校验要求。
//
// 实测端点是 POST；GET 必须被拒绝 ——
// 否则可能把无关响应当成凭据（Codex：「不能因为同 URL 可能存在
// 其他方法就全部接受」）。
func TestProdMatcherRequiresPOST(t *testing.T) {
	const state = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	u := "https://www.codebuddy.cn/console/login/enterprise?state=" + state
	m := newProdCredMatcher()

	if !m.matches(u, "POST", state) {
		t.Error("POST 应被接受（实测端点方法）")
	}
	if m.matches(u, "GET", state) {
		t.Error("❌ GET 被接受了 —— 方法校验缺失，可能误取无关响应")
	}
	if got := m.rejectReason(u, "GET", state); got != rejectMethod {
		t.Errorf("拒绝原因应为 %q，得到 %q", rejectMethod, got)
	}
	t.Log("✅ 方法校验生效：仅 POST 被接受")
}

// TestTestSeamDoesNotAffectProdMatcher 断言接缝只影响"被替换的那个匹配器"，
// 不污染生产匹配器（Codex 第 29 轮要求接缝为实例级）。
func TestTestSeamDoesNotAffectProdMatcher(t *testing.T) {
	const state = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	local := "http://127.0.0.1:9999/console/login/enterprise?state=" + state

	restore := setCredEndpointForTest("http://127.0.0.1:9999/console/login/enterprise")
	defer restore()

	// 有接缝时：捕获用的匹配器接受它。
	if !credMatcherForCapture().matches(local, credentialEndpointMethod, state) {
		t.Fatal("接缝未生效 —— 捕获匹配器应接受被替换的端点")
	}

	// 但**生产匹配器本身**仍然严格（接缝是实例级的，不是全局放宽）。
	if newProdCredMatcher().matches(local, credentialEndpointMethod, state) {
		t.Error("❌ 接缝污染了生产匹配器 —— 应各是各的实例")
	}
	t.Log("✅ 接缝只影响捕获匹配器，生产匹配器仍严格")
}
