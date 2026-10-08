// api_login.go：面板「网页登录」端点。
//
// 委托方要求（原话）：「我需要这种网页登录的快捷，不需要下载软件」
// —— 做成 wbapi 内置能力，单 exe 发朋友也能用。
//
//	POST /api/accounts/login/start   → 启动登录流程，立即返回
//	GET  /api/accounts/login/status  → 查询进度（阶段 + 脱敏结果）
//	POST /api/accounts/login/cancel  → 取消进行中的登录
//
// ═══════════════════════════════════════════════════════════════════
// 两条来自 Codex 第 28 轮的硬性纪律（本文件的实现依据）
// ═══════════════════════════════════════════════════════════════════
//
// ① 「凭据字段解析成功 = 捕获完成，**不等于** 账号验证完成。」
//
//	⇒ 拿到凭据后**必须**先查一次额度（Credit）证明它真能用，
//	  **验证通过才落盘**。否则会把无效凭据写进账号库，
//	  表现为"账号在列表里但永远用不了"。
//
// ② 「只有凭据验证成功才提交更新；取消或失败不得覆盖已有账号，
//
//	 也不得降级为明文保存。」
//	⇒ 失败/取消路径**绝不**触碰已有账号。
//
// ═══════════════════════════════════════════════════════════════════
// 安全
// ═══════════════════════════════════════════════════════════════════
//
//   - 本文件的任何响应**绝不**含 token（有测试守着）
//   - 状态查询只返回：阶段、脱敏 uid、昵称、额度、脱敏错误
//   - 凭据只在内存中从 login 包流到落盘那一行
//   - 同一时刻只允许一个登录流程（login 包内部互斥）
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/login"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// loginStartTimeout 是用户完成登录的总时限。
// 扫码/短信都需要时间，给足 10 分钟。
const loginStartTimeout = 10 * time.Minute

// loginRun 表示一次进行中的登录流程。
type loginRun struct {
	mu    sync.Mutex
	phase login.Phase

	// platform 本次登录的目标平台（国内版/国际版）。
	//
	// 🔴 必须在**启动时**定下并贯穿到底（登录页 + 凭据捕获白名单 +
	//	落盘时的 Platform 字段）。任何一处漏了都会分叉，症状是
	//	"登录页对了但抓不到凭据"或"账号落到错误的平台池里"。
	platform string

	// 结果（仅成功时有意义）
	done     bool
	uid      string // 脱敏后的
	nickname string
	credits  *int64
	errMsg   string // 脱敏后的

	cancelCh chan struct{}
	canceled bool
}

func (r *loginRun) setPhase(p login.Phase) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

func (r *loginRun) snapshot() (login.Phase, bool, string, string, *int64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase, r.done, r.uid, r.nickname, r.credits, r.errMsg
}

// loginManager 保存当前（唯一的）登录流程。
type loginManager struct {
	mu      sync.Mutex
	current *loginRun
}

var activeLogin loginManager

// handleLoginStart 启动一次网页登录。
//
// 立即返回（不阻塞 HTTP 请求），实际进度通过 status 端点查询。
func handleLoginStart(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Accounts == nil || deps.Persister == nil {
		writeError(w, http.StatusServiceUnavailable, openaiErrTypeServer,
			"no_accounts", "账号功能未启用")
		return
	}

	// ── 平台选择（2026-10-08 新增）──
	//
	// 🔴 为什么必须让用户显式选：
	//
	//	国内版与国际版是**两套账号体系**（不同域名、不同站点）。
	//	改造前登录页写死国内版 ⇒ 国际版用户根本无从添加。
	//	而"自动判断"做不到：用户还没登录，程序无从知道他要绑哪个平台。
	//
	// 请求体：{"platform":"cn"} 或 {"platform":"intl"}；缺省 = cn（兼容旧前端）。
	// ⚠️ 非法值**明确报错**而不是静默回退 —— 让用户绑到错误平台的代价
	//   远大于一次明确的 400（他会以为"登录成功了"但账号在另一个池里）。
	platform := auth.PlatformCN
	if r.Body != nil {
		var req struct {
			Platform string `json:"platform"`
		}
		if dec := json.NewDecoder(io.LimitReader(r.Body, 4096)); dec.Decode(&req) == nil {
			if strings.TrimSpace(req.Platform) != "" {
				if !login.IsKnownPlatform(req.Platform) {
					writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
						"bad_platform",
						"未知平台 "+req.Platform+"；只支持 cn（国内版）或 intl（国际版）")
					return
				}
				if login.NormalizePlatform(req.Platform) == login.PlatformIntl {
					platform = auth.PlatformIntl
				}
			}
		}
	}

	// 先查"是否已有流程"再查浏览器 —— 顺序有讲究：
	// 已有流程时用户该看到"正在进行中"，而不是被浏览器检查的结果误导。
	// （逆向对照时发现：把浏览器检查放前面，并发请求会拿到 503 而非 409。）
	activeLogin.mu.Lock()
	if activeLogin.current != nil {
		activeLogin.mu.Unlock()
		writeError(w, http.StatusConflict, openaiErrTypeInvalidRequest,
			"login_busy", "已有一个登录流程在进行中")
		return
	}
	activeLogin.mu.Unlock()

	// 是否有浏览器可用？没有就明确告知并引导走导入（Codex Q4：
	// 「不要修改策略或静默下载浏览器」）。
	if _, _, err := login.FindBrowser(); err != nil {
		writeError(w, http.StatusServiceUnavailable, openaiErrTypeServer,
			"no_browser",
			"未找到 Chrome 或 Edge。请安装其中一个浏览器，或改用「导入凭据」方式添加账号。")
		return
	}

	// 再次加锁占位（上面释放过锁，中间可能有并发请求也通过了检查）。
	activeLogin.mu.Lock()
	if activeLogin.current != nil {
		activeLogin.mu.Unlock()
		writeError(w, http.StatusConflict, openaiErrTypeInvalidRequest,
			"login_busy", "已有一个登录流程在进行中")
		return
	}
	run := &loginRun{phase: login.PhaseStarting, cancelCh: make(chan struct{}), platform: platform}
	activeLogin.current = run
	activeLogin.mu.Unlock()

	go runLogin(deps, run)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":       true,
		"phase":    string(login.PhaseStarting),
		"platform": string(platform),
		"hint":     "浏览器窗口已打开，请在其中完成登录。",
	})
}

// runLogin 在后台执行完整登录：拿凭据 → 验证可用性 → 落盘。
func runLogin(deps Deps, run *loginRun) {
	defer func() {
		// 流程结束后释放互斥位，允许下一次登录。
		activeLogin.mu.Lock()
		if activeLogin.current == run {
			activeLogin.current = nil
		}
		activeLogin.mu.Unlock()
	}()

	// 把 app 层的平台标识翻译成 login 包的。
	// （两边的字符串值刻意相同，但类型不同 —— 见 login.Platform 的说明。）
	lp := login.PlatformCN
	if run.platform == auth.PlatformIntl {
		lp = login.PlatformIntl
	}
	res := login.LoginFor(lp, loginStartTimeout, run.setPhase, run.cancelCh)
	if res.Err != nil {
		run.mu.Lock()
		run.done = true
		run.errMsg = loginErrorMessage(res.Err)
		run.mu.Unlock()
		return
	}

	creds := res.Credentials

	// ── 步骤①：查一次额度，证明凭据真能用 ──
	//
	// Codex：「凭据字段解析成功 = 捕获完成，不等于账号验证完成。」
	// 这里**先验证再落盘** —— 验证失败绝不写入账号库。
	uid, nickname, credits, err := verifyCredential(deps, run.platform, creds)
	if err != nil {
		run.mu.Lock()
		run.done = true
		run.errMsg = "凭据验证失败：" + loginErrorMessage(err)
		run.mu.Unlock()
		return
	}

	// ── 步骤②：验证通过后才落盘 ──
	//
	// 🔴 Platform 必须跟着写进去：它决定这个账号以后
	//	① 用哪套域名发请求、② 落到哪个调度池、③ 面板归到哪一组。
	//	漏写会默认成国内版，于是一个国际版账号的凭据会被发到国内域名 ——
	//	表现为"账号显示正常但一调用就失败"。
	acct := &auth.Account{
		UID:          uid,
		Nickname:     nickname,
		Platform:     run.platform,
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		Status:       pool.StatusNormal,
	}
	deps.Accounts.Put(acct)

	// 额度结果一并写入（避免刚登录完就被当作"未刷新"排除）。
	//
	// 用既有的 applyCreditResult 而不是自己手改字段 —— 它已经处理了
	// 额度包列表、Known 标志、时间戳等一整套语义，手改容易漏。
	if credits != nil {
		deps.Accounts.Mutate(uid, func(x *auth.Account) bool {
			applyCreditResult(x, provider.CreditResult{Remaining: credits}, time.Now())
			return true
		})
	}

	if err := deps.Persister.Save(deps.Accounts); err != nil {
		run.mu.Lock()
		run.done = true
		run.errMsg = "保存账号失败：" + loginErrorMessage(err)
		run.mu.Unlock()
		return
	}

	run.mu.Lock()
	run.done = true
	run.uid = auth.MaskUID(uid)
	run.nickname = nickname
	run.credits = credits
	run.phase = login.PhaseDone
	run.mu.Unlock()
}

// verifyCredential 用刚拿到的凭据确认"这确实是个能用的账号"。
//
// 返回 uid / 昵称 / 额度。**任何一步失败都返回错误**，调用方据此拒绝落盘。
//
// 为什么必须分两步（Codex 第 28 轮的要求）：
//
//		「凭据字段解析成功 = 捕获完成，不等于 账号验证完成。」
//
//	 1. Identity：用 token 换出 uid —— 证明 token 被上游认可，
//	    同时补全网页登录凭据里缺失的 uid（占位账号无法落盘）。
//	 2. Credit：查一次额度 —— 证明这个账号真的"能用"，
//	    而不只是"能识别身份"。顺带把额度写进账号，避免刚登录就被调度排除。
func verifyCredential(deps Deps, platform string, creds login.Credentials) (string, string, *int64, error) {
	if deps.WBClient == nil {
		return "", "", nil, errors.New("账号功能未启用")
	}

	// 用占位 uid 建一个临时凭据去查询。
	// ⚠️ 这个账号**不进**账号库，只存在于本次调用栈里。
	//
	// 🔴 Platform 必须设成用户选的那个：它决定 providerForAccount
	//	用哪套域名（国内 www.codebuddy.cn / 国际 www.workbuddy.ai）。
	//	设错的话验证一定失败（拿国际版 token 去国内域名查），
	//	而错误信息会被我们归类成"凭据无效"，指向完全错误的方向。
	tmp := &auth.Account{
		UID:          "pending-login",
		Platform:     platform,
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		Status:       pool.StatusNormal,
	}

	p := providerForAccount(deps, tmp)
	if p == nil {
		return "", "", nil, errors.New("无法为该渠道创建客户端")
	}

	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()

	// ── 步骤 1：换出身份（uid）──
	// 这一步同时证明 token 有效：无效 token 拿不到 uid。
	//
	// ⚠️ 这里用**窄接口类型断言**而不是把 Identity 加进 provider.Provider：
	// 后者会强迫每个渠道实现一个它可能根本没有的能力（provider 层不许
	// 为了一个渠道的需要而加宽公共契约）。
	ident, ok := p.(interface {
		Identity(context.Context) (workbuddy.Identity, error)
	})
	if !ok {
		return "", "", nil, errors.New("该渠道不支持用凭据反查账号身份，无法完成网页登录")
	}
	id, err := ident.Identity(ctx)
	if err != nil {
		return "", "", nil, fmt.Errorf("凭据无效或已过期: %w", err)
	}

	// ── 步骤 2：查一次额度，证明账号"能用"──
	// 用真实 uid 重建凭据（部分上游头依赖 uid 派生）。
	real := &auth.Account{
		UID:          id.UID,
		Nickname:     id.Nickname,
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		Status:       pool.StatusNormal,
	}
	p2 := providerForAccount(deps, real)
	if p2 == nil {
		return "", "", nil, errors.New("无法为该渠道创建客户端")
	}
	res, err := p2.Credit(ctx, "")
	if err != nil {
		return "", "", nil, fmt.Errorf("额度查询失败: %w", err)
	}

	var credits *int64
	if res.Remaining != nil {
		v := *res.Remaining
		credits = &v
	} else {
		// 能查到身份但额度为空 —— 账号可用但当前无额度包。
		// 这**不算失败**（新账号可能还没领额度），落盘时额度留空。
		credits = nil
	}

	return id.UID, id.Nickname, credits, nil
}

// handleLoginStatus 查询登录进度。
func handleLoginStatus(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 GET")
		return
	}

	activeLogin.mu.Lock()
	run := activeLogin.current
	activeLogin.mu.Unlock()

	if run == nil {
		// 没有进行中的流程：返回 idle。
		// ⚠️ 这里**不保留**上一次的结果 —— 避免凭据相关的残留状态。
		writeJSON(w, http.StatusOK, map[string]any{
			"busy":  false,
			"phase": "idle",
		})
		return
	}

	phase, done, uid, nickname, credits, errMsg := run.snapshot()
	out := map[string]any{
		"busy":  !done,
		"phase": string(phase),
	}
	if uid != "" {
		out["uid"] = uid // 已脱敏
	}
	if nickname != "" {
		out["nickname"] = nickname
	}
	if credits != nil {
		out["credits"] = *credits
	}
	if errMsg != "" {
		// 🔴 再脱敏一次（纵深防御）。
		//
		// errMsg 存入 run 时已经过 loginErrorMessage，但这里是**响应出口** ——
		// 任何将来新增的写入 errMsg 的路径都必须经过这一层。
		// 测试 TestLoginStatusNeverLeaksTokenFields 会往 errMsg 里注入
		// 一个 JWT，正是为了钉住这条出口防线。
		out["error"] = sanitizeErrorMessage(errMsg)
	}
	if done && errMsg == "" {
		out["ok"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLoginCancel 取消进行中的登录。
func handleLoginCancel(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}

	activeLogin.mu.Lock()
	run := activeLogin.current
	activeLogin.mu.Unlock()

	if run == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phase": "idle"})
		return
	}

	run.mu.Lock()
	if !run.canceled {
		run.canceled = true
		close(run.cancelCh)
	}
	run.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phase": "canceled"})
}

// loginErrorMessage 把内部错误转成给用户看的脱敏信息。
//
// 🔴 绝不能把原始错误直接抛给前端 —— 里面可能含 URL、token 片段、
// 或上游返回的原始报文。
//
// ⚠️ 这里**同时**用 errors.Is 和字符串匹配：
// errors.Is 只在错误**未被包装**时命中，而 login 包内部多处用了
// fmt.Errorf("%w") 包装，加上跨包传递，单靠 errors.Is 会漏（测试抓到过）。
// 字符串匹配是兜底，两层都要有。
func loginErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	// 第一层：errors.Is（适用于未被包装的哨兵错误）
	switch {
	case errors.Is(err, login.ErrNoBrowser):
		return "未找到 Chrome 或 Edge，请改用「导入凭据」方式添加账号。"
	case errors.Is(err, login.ErrLoginBusy):
		return "已有一个登录流程在进行中。"
	}

	// 第二层：字符串匹配（覆盖被包装的情况）
	msg := err.Error()
	switch {
	case strings.Contains(msg, "未找到受支持的浏览器"):
		return "未找到 Chrome 或 Edge，请改用「导入凭据」方式添加账号。"
	case strings.Contains(msg, "已有登录流程"):
		return "已有一个登录流程在进行中。"
	case strings.Contains(msg, "等待登录超时"):
		return "等待登录超时，请重试。"
	case strings.Contains(msg, "已取消"):
		return "登录已取消。"
	case strings.Contains(msg, "浏览器连接已断开"):
		return "浏览器窗口被关闭，登录中断。请重试。"
	case strings.Contains(msg, "浏览器进程提前退出"):
		return "浏览器启动后立即退出，请重试或改用「导入凭据」。"
	case strings.Contains(msg, "非回环"):
		return "浏览器调试端口未正确绑定，已中止。请重试。"
	}

	return sanitizeErrorMessage(msg)
}

// sanitizeErrorMessage 兜底脱敏：截断 + 抹掉疑似凭据。
//
// 🔴 这里是**最后一道防线**：任何进入 API 响应的错误文本都必须过这里。
// 之前用 strings.Fields 逐字段判断的做法有真实漏洞（测试抓到）：
// JWT 里没有空格，但错误文本常写成 `token=eyJ...` 或 `Bearer eyJ...`，
// 整个字段既不等于 JWT、长度又可能不足 120，于是被原样保留。
// 现在改为**直接扫描文本、按形态替换**，不依赖分词。
func sanitizeErrorMessage(msg string) string {
	// 去掉换行/控制字符（防日志注入与面板渲染问题）
	msg = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, msg)

	msg = redactTokenLike(msg)

	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// redactTokenLike 把文本里所有"像凭据"的子串替换成 …。
//
// 覆盖三种形态：
//  1. JWT：三段 base64url，以 eyJ 开头（Keycloak token 都是这个形态）
//  2. 超长不透明串：连续 40+ 个 base64url/hex 字符
//     （不透明 refresh token 不一定长得像 JWT）
//  3. 常见前缀写法：Bearer xxx / token=xxx / access_token=xxx
//
// 为什么不用正则：标准库 regexp 够用且更清晰，这里就用了 —— 但要注意
// 模式必须足够宽，否则会漏（第一次实现就是因为太窄而漏掉了 JWT）。
func redactTokenLike(s string) string {
	// 先处理带前缀的写法，再处理裸串。
	repl := []struct {
		re   *regexp.Regexp
		repl string
	}{
		// Bearer <token>
		{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]{20,}`), "Bearer …"},
		// token=xxx / access_token=xxx / refresh_token=xxx / accessToken":"xxx"
		{regexp.MustCompile(`(?i)\b(access_?token|refresh_?token|id_?token|machine_?token|token)\b\s*[=:]\s*"?'?[A-Za-z0-9._\-]{20,}"?'?`), "token=…"},
		// 裸 JWT（三段，以 eyJ 开头）
		{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}`), "…"},
		// 裸超长串（兜底）
		{regexp.MustCompile(`[A-Za-z0-9_\-]{60,}`), "…"},
	}
	for _, r := range repl {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// ensureLoginJSONUnused 防止 json 包被误删（status 端点用了 map，但保留引用以便未来扩展）。
var _ = json.Marshal
