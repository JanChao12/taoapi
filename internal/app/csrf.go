// csrf.go：管理接口的写操作防护。
//
// 🔴 为什么需要（Codex 第 11、12 轮连续提出）：
//
//	`/api/*` 与 `/panel/*` **不校验 API key** —— 理由是它们只监听 127.0.0.1，
//	且面板必须在"还没设 key"时就能打开（否则鸡生蛋）。
//
//	但这组合出一个真实风险：你在浏览器里访问了恶意网页，它可以执行
//
//	    fetch('http://127.0.0.1:8787/api/settings', {
//	      method:'PATCH', headers:{'Content-Type':'application/json'},
//	      body: JSON.stringify({apiKey:'attacker'})
//	    })
//
//	浏览器会真的把请求发出去。攻击者读不到响应（同源策略），
//	但对"改 key / 触发重启 / 禁用账号"这类有副作用的操作，读不到响应不重要。
//
// ═══════════════════════════════════════════════════════════════════
// 防护策略（Codex 两轮评审后定稿）
// ═══════════════════════════════════════════════════════════════════
//
//  1. **自定义请求头 `X-WBAPI-Panel: <token>`** —— 主防线。
//     浏览器跨站请求默认无法携带自定义头（会触发 CORS 预检，
//     而我们不应答预检 → 请求被浏览器自己拦下）。
//     token 在进程启动时随机生成，写进面板页面。
//  2. **Origin / Referer 校验** —— 辅助防线。
//     给了来源就必须指向本服务（回环 + 端口一致）。
//  3. **要求 JSON Content-Type** —— 不依赖头的第三道防线：
//     跨站"简单请求"只能用 form-urlencoded / multipart / text-plain。
//
// 🔴 我最初的方案（已否决，勿改回）："Origin 缺失就放行"。
//
//	理由是"浏览器跨站必定带 Origin，所以缺失=脚本=可信"。
//	Codex 指出这站不住：跨站表单、隐私策略、特殊 WebView、`Origin: null`
//	都可能不按预期出现 —— 把"通常带"当成"一定带"是典型防线漏洞。
//	改为**要求显式 token**：脚本带一个头即可，而攻击者无法让浏览器自动带上。
//	**安全默认值必须是拒绝，不是放行。**
//
// 明确接受的残余风险：本机其他进程知道 token 的话能改 key。
// 这是**有意接受**的 —— 能本机跑进程的攻击者本来就能读
// `~/.wbapi/config.json`、改 hosts、注入 DLL，防他意义有限。
// 我们要防的是"浏览器里的网页"，不是"本机已有代码执行权的人"。
package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
)

// panelTokenHeader 是面板写操作必须携带的自定义头。
//
// 用自定义头（而非标准头）是关键：浏览器允许跨站"简单请求"携带的
// 只有 Accept / Content-Type / Content-Language 等少数几个，
// **自定义头一定会触发预检**，而我们不应答预检。
const panelTokenHeader = "X-WBAPI-Panel"

// newPanelToken 生成进程级的面板写操作令牌。
//
// 每次启动换一个：避免"上次会话的 token"被长期复用于攻击。
// 用 crypto/rand —— 它是安全边界的一部分，可预测的值等于没防护。
func newPanelToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// isStateChanging 判断方法是否有副作用（需要防护）。
func isStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// checkWriteAuth 校验写请求。返回空串表示通过。
//
// 顺序：先验 token（主防线），再验来源（辅助），最后验 Content-Type。
func checkWriteAuth(r *http.Request, deps Deps) string {
	// ── 主防线：自定义头 token ──
	if want := deps.panelToken; want != "" {
		got := r.Header.Get(panelTokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			return "缺少或错误的 " + panelTokenHeader + " 请求头（面板写操作需要它）"
		}
	}
	// want 为空 = 未启用防护（进程内嵌入式用法/部分测试）。
	// 这不降低实际安全性：那种场景下调用方本来就在进程内。

	// ── 辅助防线：来源必须指向本服务 ──
	if reason := checkSameOrigin(r, deps.ListenPort); reason != "" {
		return reason
	}

	// ── 辅助防线：JSON Content-Type ──
	// 例外：没有 body 的请求（如 POST /restart）允许缺失
	if r.ContentLength != 0 {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if !strings.HasPrefix(ct, "application/json") {
			return "写操作要求 Content-Type: application/json"
		}
	}
	return ""
}

// checkSameOrigin 校验请求来源（仅在给了来源时校验）。
//
// 注意：**缺失来源不再自动放行** —— 那件事已由 token 主防线负责。
// 本函数只管"如果给了 Origin/Referer，它必须是对的"。
func checkSameOrigin(r *http.Request, selfPort int) string {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		if origin == "null" {
			// 沙箱 iframe / file:// 场景。正常面板不可能从 null origin 发起。
			return "请求来源不被允许（Origin: null）"
		}
		if !originMatches(origin, selfPort) {
			return "请求来源不被允许（Origin 不匹配）"
		}
		return ""
	}

	referer := strings.TrimSpace(r.Header.Get("Referer"))
	if referer != "" {
		u, err := url.Parse(referer)
		if err != nil {
			return "请求来源不被允许（Referer 无法解析）"
		}
		if !originMatches(u.Scheme+"://"+u.Host, selfPort) {
			return "请求来源不被允许（Referer 不匹配）"
		}
	}

	// 两者都缺失：token 已验过，放行（脚本场景）。
	return ""
}

// originMatches 判断 origin 是否指向本服务的回环地址。
//
// 接受 127.0.0.1 / localhost / [::1]（Codex 提醒：用户可能用 localhost
// 打开面板，只允许 127.0.0.1 会导致"面板能读、保存却失败"）。
func originMatches(origin string, selfPort int) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	port := u.Port()

	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if selfPort > 0 && port != itoa(selfPort) {
		return false
	}
	return isLoopbackHost(host)
}

// isLoopbackHost 判断 host 是否为回环地址。
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// guardManagementAPI 包装管理接口（/api/*）的处理器，做写操作防护。
//
// 只对有副作用的方法生效；GET/HEAD/OPTIONS 直接放行
// （读操作无副作用，不应影响本机脚本与监控）。
func guardManagementAPI(deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isStateChanging(r.Method) {
			next(w, r)
			return
		}
		if reason := checkWriteAuth(r, deps); reason != "" {
			status := http.StatusForbidden
			if strings.Contains(reason, "Content-Type") {
				status = http.StatusUnsupportedMediaType
			}
			writeError(w, status, openai.ErrTypeInvalidRequest,
				"cross_origin_denied", reason)
			return
		}
		next(w, r)
	}
}
