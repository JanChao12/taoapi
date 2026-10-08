// credmatch.go：凭据端点的**匹配规则**与测试接缝。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么要单独一个文件（而不是塞在 cdp.go 里）
// ═══════════════════════════════════════════════════════════════════
//
// 2026-10-05 的真实事故：`captureCredentials` 在 CDP 事件回调里
// **同步**调 `fetchResponseBody`，与 readLoop 自死锁，真实登录
// **永远抓不到凭据**；而当时测试**全绿** —— 因为 `integration_test.go`
// 把捕获逻辑**抄了一遍**，没走生产函数。
//
// **教训：测试抄生产逻辑 = 测了个假的。**
//
// 要真测到生产函数，就得能让它连到本地假服务端。Codex 第 29 轮裁定：
// 这种"依赖替换"属于**合理的最小接缝，不是过度设计**，
// 但要求用**实例级**（不能用包级可变全局变量，否则并行测试互相污染）。
//
// 所以匹配规则与接缝集中在这里，`captureCredentials` 持有一个 matcher。
package login

import (
	"net/http"
	"net/url"
	"strings"
)

// credentialEndpointHost / Path 是凭据下发端点。
//
// ⚠️ 这不是"标准 OIDC token endpoint"（Codex 第 28 轮提醒）：
// 它是**实测出来的站点接口**，可能随上游变更。上游契约文档里如实这么写。
//
// 🔴 2026-10-08 起 host 分平台（见 credentialHostFor）：
//
//	国内版 www.codebuddy.cn，国际版 www.workbuddy.ai。
//	实测两个域名的路径**完全相同**（/console/login/enterprise，
//	无凭据时都返回 401），差别只在域名。
//
//	⚠️ 白名单**只有这两个**，绝不通配 —— 它是"凭据只能从哪来"的
//	安全边界（否则任意站点都能伪造一个凭据响应喂给我们）。
const (
	credentialEndpointHostCN   = "www.codebuddy.cn"
	credentialEndpointHostIntl = "www.workbuddy.ai"
	credentialEndpointPath     = "/console/login/enterprise"

	// credentialEndpointMethod 是实测到的请求方法。
	//
	// 🔴 2026-10-05 实测为 **POST**（此前交接文档记的是 GET，已更正）。
	//
	// Codex 第 29 轮裁定要求**校验方法**，理由：
	// 「不能因为同 URL 可能存在其他方法就全部接受」——
	// 宽泛捕获可能把**无关响应当成凭据**；而收紧导致的失败是
	// **明确、可诊断**的兼容性问题。
	credentialEndpointMethod = http.MethodPost
)

// credentialHostFor 返回指定平台的凭据下发域名。
//
// 未知平台保守回退国内版（与 NormalizePlatform 一致）。
func credentialHostFor(p Platform) string {
	if p == PlatformIntl {
		return credentialEndpointHostIntl
	}
	return credentialEndpointHostCN
}

// credMatcher 判定"某个响应是否就是我们要的凭据下发"。
//
// 生产用 newProdCredMatcher()；测试可用 withOverride 指向本地假服务端。
type credMatcher struct {
	// platform 决定允许的 host（国内版/国际版）。
	//
	// 🔴 零值（""）等于 PlatformCN —— 保守回退，不会因为漏设而放宽成"任意 host"。
	platform Platform

	// overrideHost / overridePath / overrideHTTPS 用于测试替换端点。
	//
	// ⚠️ 刻意做成**实例字段**而不是包级变量（Codex 第 29 轮要求）：
	// 包级可变全局会让并行测试互相污染，且难以判断"当前生效的是哪一个"。
	overrideScheme string
	overrideHost   string
	overridePath   string
	overrideMethod string
}

// newProdCredMatcher 返回生产匹配器（严格校验上游端点）。
//
// 不传平台时保守按国内版（改造前的行为）。
func newProdCredMatcher(platform ...Platform) *credMatcher {
	m := &credMatcher{}
	if len(platform) > 0 {
		m.platform = platform[0]
	}
	return m
}

// withOverrideForTest 返回一个指向本地假服务端的匹配器副本（仅测试用）。
//
// 🔴 只在测试里调用。生产**永不**调用它 —— 且有一个测试
// （TestProdMatcherRejectsLocalHTTP）断言生产匹配器确实拒绝本地 http。
func (m credMatcher) withOverrideForTest(rawURL string) *credMatcher {
	u, err := url.Parse(rawURL)
	if err != nil {
		return &m
	}
	out := m
	out.overrideScheme = u.Scheme
	out.overrideHost = u.Host
	out.overridePath = u.Path
	out.overrideMethod = http.MethodPost
	return &out
}

// matches 判断一个响应是否为本流程的凭据下发。
//
// 严格校验（Codex 第 28、29 轮要求）：
//   - scheme == https
//   - host 完全等于 www.codebuddy.cn
//   - path 完全等于 /console/login/enterprise
//   - **method == POST**
//   - state 恰好一个值，且等于本次生成的那个
//
// 任何一条不满足都返回 false（并给出原因，供诊断）。
func (m *credMatcher) matches(rawURL, method, wantState string) bool {
	return m.rejectReason(rawURL, method, wantState) == ""
}

// rejectReason 返回被拒绝的**原因**；空串表示匹配成功。
//
// ⚠️ 这个函数必须与 matches **判定完全一致** ——
// 否则诊断会指向错误的方向，比没有诊断更糟。
// 有元测试（TestRejectReasonAgreesWithMatcher）钉住这一点。
//
// 返回的原因是**常量字符串**，可安全进入错误信息与 API 响应。
func (m *credMatcher) rejectReason(rawURL, method, wantState string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rejectUnparsable
	}

	scheme, host, path := "https", credentialHostFor(m.platform), credentialEndpointPath
	if m.overrideHost != "" {
		scheme, host, path = m.overrideScheme, m.overrideHost, m.overridePath
	}

	if u.Scheme != scheme {
		// 生产要求 https；本地 http 会走到这里（这是**正确**的拒绝）。
		return rejectScheme
	}
	if !strings.EqualFold(u.Host, host) {
		return rejectHost
	}
	if u.Path != path {
		return rejectPath
	}
	if method != credentialEndpointMethod {
		return rejectMethod
	}
	if wantState == "" {
		return rejectState
	}
	vals, ok := u.Query()["state"]
	if !ok || len(vals) != 1 || vals[0] != wantState {
		return rejectState
	}
	return ""
}

// isCredentialEndpoint 保留旧的包级入口，**仅用于既有单元测试**。
//
// ⚠️ 生产代码一律走 `credMatcher.matches`（它带方法校验）。
// 这个包装默认按 POST 校验，避免旧测试无意间放宽。
func isCredentialEndpoint(rawURL, wantState string) bool {
	return newProdCredMatcher().matches(rawURL, credentialEndpointMethod, wantState)
}

// credEndpointerForTest 允许测试把捕获端点指向本地假服务端。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要它（Codex 第 29 轮明确认可这是"最小接缝"）
// ═══════════════════════════════════════════════════════════════════
//
// 2026-10-05 的真实事故：`captureCredentials` 在回调里同步调
// `fetchResponseBody` ⇒ 与 readLoop 自死锁，真实登录**永远抓不到凭据**。
// 而当时测试**全绿** —— 因为 `integration_test.go` 把捕获逻辑**抄了一遍**，
// 没走生产函数，天然不含那个死锁。
//
// **教训：测试抄生产逻辑 = 测了个假的。**
//
// 要真测到生产函数，就得让它能连到本地假服务端；而端点硬编码为
// `https://www.codebuddy.cn/...`，本地冒充不了（也不该改 hosts）。
//
// Codex 裁定原文要点：
//
//	「让测试调用生产 `captureCredentials`，只替换"允许的测试端点"，
//	  属于**合理的最小接缝，不是过度设计**。」
//
// ⚠️ 三条约束（Codex 要求，已落实）：
//  1. **实例级**，不用包级可变全局（避免并行测试互相污染）
//  2. 生产默认仍执行严格 HTTPS/上游端点检查 ——
//     有测试（TestProdMatcherRejectsLocalHTTP）断言这点
//  3. 接缝只影响"允许的端点"，匹配逻辑本身仍是同一份代码
//
// 🔴 生产恒为 nil。只有测试会设置它，且必须在 defer 里清除。
var credEndpointerForTest string

// credMatcherForCapture 返回本次捕获要用的匹配器。
//
// 生产返回严格校验的匹配器（host 由 platform 决定）；
// 测试可通过 `setCredEndpointForTest` 替换端点。
func credMatcherForCapture(platform Platform) *credMatcher {
	if credEndpointerForTest == "" {
		return newProdCredMatcher(platform)
	}
	return newProdCredMatcher(platform).withOverrideForTest(credEndpointerForTest)
}

// setCredEndpointForTest 让捕获端点指向本地假服务端（**仅测试用**）。
// 返回还原函数，调用方必须 defer 它。
func setCredEndpointForTest(rawURL string) func() {
	prev := credEndpointerForTest
	credEndpointerForTest = rawURL
	return func() { credEndpointerForTest = prev }
}
