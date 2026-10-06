// health.go：/healthz 的响应契约与跨 origin 探活（Codex 第 19 轮定稿）。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么 /healthz 不再返回纯文本 "ok"
// ═══════════════════════════════════════════════════════════════════
//
// 面板"改端口重启后自动重连"要用浏览器去探新地址是否**真的就绪**。
// 我原先的实现有两个真实缺陷（Codex 第 19 轮指出，我 4 组真实 Chrome
// 实验全部复现 —— 详见 docs/维护备忘.md §六之三）：
//
//	坑 1：用 fetch(url, {mode:'no-cors'}) 探测。
//	  实测 503 / 404 / 200 三者【全部 resolve】，且 type 恒为 'opaque'、
//	  status 恒为 0 —— **完全无法区分**。探针于是退化成"端口是否有人监听"，
//	  会导航到未就绪的服务，甚至无关程序。
//	坑 2：改为普通 CORS fetch 后只检查 response.ok。
//	  实测：无关服务若返回 `Access-Control-Allow-Origin: *` + 200，
//	  同样得到 ok:true 且 body 可读 → 仍会误判。
//
// 所以现在用**四条件**判定（缺一不导航）：
//
//	response.ok && service==='wbapi' && ready===true && restartId===本次标识
//
// ═══════════════════════════════════════════════════════════════════
// CORS 白名单：为什么必须包含【旧】origin
// ═══════════════════════════════════════════════════════════════════
//
// 坑 3（实测）：面板在旧 origin（如 127.0.0.1:8787）探测新端口（8788）时，
// 浏览器发出的 `Origin` 头**仍是旧 origin**。若新服务只允许"自己的" origin，
// 浏览器会直接拒绝读取（`TypeError: Failed to fetch`）—— **自动重连 100% 失效**。
// 实测对照：只允许自己 origin → 失败；允许旧 / 旧+新 / 任意回环 → 均成功。
//
// 坑 4（实测）：旧 origin **不能按监听地址推导**。
// 用户用 `http://localhost:8787` 打开面板时，`location.origin` 是
// `http://localhost:8787`，与 `http://127.0.0.1:8787` **是两个不同的 Origin 字符串**，
// 白名单写错一个就匹配不上。所以旧 origin 必须由前端把 `location.origin`
// 随重启请求带给服务端，并由服务端**独立严格校验**（不信任 body 里的字符串）。
//
// ⚠️ 旧 origin 为何可以保留到子进程退出（**理由要记对**）：
//
//	是"健康响应只含就绪状态与交接标识、不授予写权限"，
//	**不是**"旧端口已停止监听所以没有暴露面" —— 已加载的页面仍保留原 origin、
//	仍能发请求；旧端口以后也可能被别的程序占用。
//	（我原先用的就是后一个错误理由，Codex 第 19 轮纠正。）
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
)

// HealthPath 是健康检查路径。
const HealthPath = "/healthz"

// healthServiceName 是健康响应里的服务标识。
//
// 用途：**区分"我们的服务"与"恰好占着这个端口的无关程序"**。
// 这不是安全边界（不是凭据），而是"确认对面是谁"的握手标识。
//
// ⚠️ 2026-10-06 随产品改名（wbapi → TAOAPI）。
//
//	它与 ProductName 是**两个独立常量**（历史上就分开），
//	值保持一致以免 /healthz 与 /status 报出不同的名字。
const healthServiceName = ProductName

// healthResponse 是 /healthz 的响应体。
//
// 用 JSON 而不是纯文本 "ok"，因为面板需要判断**四个条件**才能导航：
// 仅凭 HTTP 200 无法区分"我们已就绪"与"某个无关服务恰好返回 200"。
type healthResponse struct {
	// Service 固定为 "wbapi"，用于识别对面是不是我们的服务。
	Service string `json:"service"`

	// Ready 表示服务已可接受业务请求。
	//
	// 目前 /healthz 能响应即为 true；保留该字段是为了让"监听已建立但
	// 尚未就绪"这类中间态在将来有表达位置（客户端按它判定，无需改协议）。
	Ready bool `json:"ready"`

	// RestartID 是**本次重启交接**的标识；非重启启动时为空串。
	//
	// 作用（Codex 第 19 轮提出，我认为比前三项更关键）：
	// 防止导航到"上一次重启遗留的、还活着的旧子进程" ——
	// 旧的子进程同样会返回 service/ready，只有 restartId 能区分
	// "它是不是**本次**交接的那个进程"。
	//
	// 它不是凭据（不授予任何权限），只是关联标识。
	RestartID string `json:"restartId,omitempty"`
}

// newRestartID 生成本次重启的交接标识。
//
// 固定长度十六进制，crypto/rand —— 与 panelToken 同源做法但用途不同：
// 它**不是**安全边界，只是"关联本次交接"的标识，因此可以出现在命令行。
func newRestartID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// validRestartID 校验标识形态：**必须恰好 32 位小写 hex**。
//
// ⚠️ 空串是**非法**的（返回 false）—— 不要被"没有标识"的调用场景误导：
// 调用方（serve.go）先判 `*restartID != ""` 才调用本函数，
// 所以这里不需要、也不应该把空串当合法。
//
// （本注释曾误写成"空串或 32 位小写 hex"，与实现和已有测试都矛盾；
//
//	由对抗式评审的实测发现并修正。缺失/格式错误都必须让握手失败，
//	所以返回 bool 而不是"宽松接受"。）
func validRestartID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}

// ── 允许的 origin 白名单 ──────────────────────────────────────────

// allowedOrigins 是 /healthz 的 CORS 白名单。
//
// **每次重启重建，不累积历次旧 origin**（Codex 明确要求：
// 否则白名单会随重启次数无界增长，把历史地址一直留在可读范围内）。
type allowedOrigins struct {
	set map[string]bool
}

// newAllowedOrigins 构造白名单。
//
// 入参是"候选 origin 字符串"，**每个都要经过严格校验**才纳入：
// 只有解析后确认是 http + 回环主机 + 合法端口的 origin 才被接受。
// 非法的静默丢弃（不报错）—— 白名单宁小勿大。
func newAllowedOrigins(candidates ...string) *allowedOrigins {
	a := &allowedOrigins{set: make(map[string]bool)}
	for _, c := range candidates {
		if norm, ok := normalizeLoopbackOrigin(c); ok {
			a.set[norm] = true
		}
	}
	return a
}

// Allows 判断给定 Origin 头是否被允许。
func (a *allowedOrigins) Allows(origin string) bool {
	if a == nil || origin == "" {
		return false
	}
	norm, ok := normalizeLoopbackOrigin(origin)
	if !ok {
		return false
	}
	return a.set[norm]
}

// OriginFor 返回应当回显的 `Access-Control-Allow-Origin` 值。
//
// 回显**规范化后的完整 origin**（不是通配 `*`，也不是请求原样）：
//   - 不用 `*`：Codex 第 12 轮已否决给 /api 开通配 CORS，这里是同一原则；
//   - 回显规范化值而非请求原样：避免把畸形字符串原样反射回去。
func (a *allowedOrigins) OriginFor(origin string) (string, bool) {
	if !a.Allows(origin) {
		return "", false
	}
	norm, _ := normalizeLoopbackOrigin(origin)
	return norm, true
}

// normalizeLoopbackOrigin 严格解析并规范化一个 origin 字符串。
//
// 返回 (规范化 origin, 是否合法)。合法当且仅当：
//   - 能被 URL 解析；
//   - scheme 是 http（本服务不提供 https）；
//   - **没有** userinfo / path / query / fragment；
//   - host 是回环地址（127.0.0.0/8 、::1 、localhost）；
//   - 端口是 1-65535 的显式端口。
//
// 🔴 不做前缀匹配（Codex 要求）：`http://127.0.0.1:8787.evil.com` 这类
// 前缀相同但主机不同的串必须被拒 —— 用 URL 解析后比对 Host 才能保证。
//
// 🔴 拒绝 `Origin: null`：它解析不出 scheme/host，天然被下面的检查拒绝。
func normalizeLoopbackOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" {
		return "", false
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", false
	}
	if !isLoopbackHostAny(host) {
		return "", false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", false
	}
	// 规范化输出：统一用解析后的 host（小写）+ **解析后的端口整数**。
	//
	// ⚠️ 端口必须用 `strconv.Itoa(p)` 而不是原始子串 `port`：
	//
	//	`u.Port()` 返回的是 URL 里的**原始文本**，`http://127.0.0.1:08788`
	//	会原样给出 `"08788"`（`Atoi` 能解析成 8788 并通过校验）。
	//	若直接把它拼进 ACAO，得到的是 `http://127.0.0.1:08788` ——
	//	而浏览器序列化 origin 时**从不带前导零**，这个值永远匹配不上
	//	任何真实 origin，白名单条目形同虚设（假阴性，静默失效）。
	//	（由对抗式评审实测发现，已有测试守着。）
	//
	// ⚠️ IPv6 必须**保留方括号**：Origin 语法里 `http://[::1]:8787` 才是合法的，
	// `http://::1:8787` 无法解析、永远匹配不上（实测踩到，已有测试守着）。
	h := strings.ToLower(host)
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return "http://" + h + ":" + strconv.Itoa(p), true
}

// isLoopbackHostAny 判断主机名是否为本机回环（接受 127.0.0.0/8 整段）。
//
// ⚠️ 与 csrf.go 的 isLoopbackHost 的区别（**不是重复实现，语义不同**）：
//
//	csrf.go 那个用于校验"写请求的 Origin/Referer 是否指向本服务"，
//	  它只认 **精确的 127.0.0.1** —— 因为服务只监听 127.0.0.1，
//	  把 127.0.0.2 也算作"本服务"是错的。
//	本函数用于 /healthz 的 CORS 白名单，需要覆盖 **127.0.0.0/8 整段** ——
//	  因为服务实际监听地址可能被配成段内其它地址。
//
// 只认 127.0.0.0/8 、::1 、localhost —— **不做 DNS 解析**：
// 解析会让"可解析到回环的域名"也通过，扩大可读范围且引入网络依赖。
func isLoopbackHostAny(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || h == "::1" {
		return true
	}
	// 127.0.0.0/8
	if strings.HasPrefix(h, "127.") {
		parts := strings.Split(h, ".")
		if len(parts) != 4 {
			return false
		}
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 255 {
				return false
			}
		}
		return true
	}
	return false
}

// healthHandler 处理 /healthz。
//
// origins 给出本次可读该响应的回环 origin 白名单（可为 nil = 不开 CORS）。
// restartID 是本次重启标识（非重启启动时为空串）。
func healthHandler(origins *allowedOrigins, restartID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
				"method_not_allowed", "只支持 GET/HEAD")
			return
		}

		// 目标服务自己的 origin 也要放行（正常访问 /healthz 的情况）。
		// 除此之外**只回显**白名单内的 origin；不在白名单就完全不加 CORS 头，
		// 让浏览器按同源策略拒绝 —— 这是"默认拒绝"而非"默认放行"。
		if origin := r.Header.Get("Origin"); origin != "" {
			if allowed, ok := origins.OriginFor(origin); ok {
				w.Header().Set("Access-Control-Allow-Origin", allowed)
			}
		}
		// Vary 必须设置：否则中间缓存可能把"带 CORS 头的响应"
		// 复用给另一个 origin 的请求（或不带 CORS 的复用给带 CORS 的）。
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Cache-Control", "no-store")

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		body := healthResponse{
			Service:   healthServiceName,
			Ready:     true,
			RestartID: restartID,
		}
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}
