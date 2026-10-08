// login.go：登录编排 —— 把浏览器、CDP、凭据提取串成一条可用流程。
//
// ═══════════════════════════════════════════════════════════════════
// 完整流程（全部来自实测，见 codex-consult/登录实验结论-凭据来源已定位.md）
// ═══════════════════════════════════════════════════════════════════
//
//  1. 生成不可预测的 state（crypto/rand）
//  2. 拉起受控浏览器（独立 profile），打开官方登录页：
//     https://www.codebuddy.cn/login/?platform=CLI&state=<我们的state>
//  3. **导航前**就连上 CDP 并启用 Network 域 —— 否则会漏掉早期请求
//  4. 用户在官方页面正常登录（扫码 / 手机号，我们完全不碰凭据输入）
//  5. 监听 Network 事件，登记候选 requestId：
//     必须精确匹配 https://www.codebuddy.cn/console/login/enterprise?state=<我们的state>
//  6. 等 **loadingFinished** 后再调 getResponseBody 取响应体
//  7. 从响应体解析 accessToken / refreshToken
//  8. 关掉浏览器、删干净临时 profile
//
// ═══════════════════════════════════════════════════════════════════
// 三条容易做错的时序（Codex 第 28 轮明确要求）
// ═══════════════════════════════════════════════════════════════════
//
//	① responseReceived 只是"登记"，**不能**立刻取 body。
//	   实测：过早调用 getResponseBody 会拿到空 body 或报错。
//	   必须等 loadingFinished（或 loadingFailed 时放弃该 requestId）。
//
//	② 必须在**导航之前**启用 Network 域。
//	   导航之后再启用，可能错过"导航本身触发的那批请求"。
//
//	③ 响应体可能是 base64Encoded（CDP 的这个字段为 true 时，
//	   body 是 base64 而非明文）。必须解码，否则 JSON 解析必然失败。
//
// ═══════════════════════════════════════════════════════════════════
// 语义边界（Codex 特别强调，别说过头）
// ═══════════════════════════════════════════════════════════════════
//
//	「凭据字段解析成功 = "捕获完成"，**不等于** "账号验证完成"。」
//
// 所以本文件只负责"拿到凭据"，**不**宣称登录可用。
// 凭据是否真的能用，由上层拿它去查一次额度（Credit）来证明 ——
// 那一步在上层做，失败就不落盘。
//
// ═══════════════════════════════════════════════════════════════════
// 安全
// ═══════════════════════════════════════════════════════════════════
//
//   - 凭据只在内存里流转，本文件**不写任何日志**、不落盘
//   - 所有错误信息不含 token（extractCredential 已保证）
//   - 进度回调只传"阶段名"，不传任何响应内容
//   - state 用完即弃；不持久化
package login

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LoginURLTemplate 是**国内版**官方登录页（实测 301 → /login/ → 200）。
//
// platform=CLI 与官方 CLI 客户端一致；state 由我们生成并由凭据端点带回。
//
// ⚠️ 2026-10-08 起请用 LoginURLFor(platform) —— 见下方说明。
const LoginURLTemplate = "https://www.codebuddy.cn/login/?platform=CLI&state=%s"

// LoginURLTemplateIntl 是**国际版**官方登录页（实测 200）。
//
// 🔴 为什么需要它（2026-10-08 委托人实测缺陷）：
//
//	原实现把登录页**写死**成国内版 www.codebuddy.cn，导致
//	**国际版账号无法通过「网页登录」添加** —— 用户在国际版登录页
//	根本登录不了自己的账号（域名不对，账号体系不同）。
//	面板虽然能按平台分组显示，但"添加"这条路只有国内版。
//
// 实测：https://www.workbuddy.ai/login/?platform=CLI&state=... → HTTP 200
const LoginURLTemplateIntl = "https://www.workbuddy.ai/login/?platform=CLI&state=%s"

// Platform 标识要登录哪个站点。
//
// 字符串值与 auth.PlatformCN / auth.PlatformIntl **一致**（"cn"/"intl"），
// 但本包刻意**不 import auth**（login 是底层工具包，不该依赖上层领域模型）。
// 有一个测试（TestPlatformValuesMatchAuth）钉住两者一致。
type Platform string

const (
	// PlatformCN 国内版（www.codebuddy.cn / copilot.tencent.com）。
	PlatformCN Platform = "cn"

	// PlatformIntl 国际版（www.workbuddy.ai）。
	PlatformIntl Platform = "intl"
)

// NormalizePlatform 把外部输入归一为已知平台。
//
// 未知/空值**保守地**回退到国内版（改造前的行为），
// 而不是报错：登录流程不该因为一个拼错的平台名就整条不可用。
// 但调用方（面板）应当只传已知值；服务端也做校验并回报明确错误。
func NormalizePlatform(s string) Platform {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "intl", "international", "ai", "workbuddyai":
		return PlatformIntl
	default:
		return PlatformCN
	}
}

// IsKnownPlatform 报告输入是否是**明确支持**的平台标识。
//
// 与 NormalizePlatform 的区别：后者永不失败（用于"宽松回退"），
// 前者用于"校验用户输入并给明确错误"。
func IsKnownPlatform(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cn", "intl", "international", "ai", "workbuddyai":
		return true
	}
	return false
}

// LoginURLFor 返回指定平台的登录页 URL（含 state）。
//
// 🔴 平台决定域名，而域名决定**账号体系** —— 选错平台会让用户
//
//	在一个不属于自己的站点上登录，表现为"登录页能打开但登录不了"。
func LoginURLFor(p Platform, state string) string {
	if p == PlatformIntl {
		return fmt.Sprintf(LoginURLTemplateIntl, state)
	}
	return fmt.Sprintf(LoginURLTemplate, state)
}

// 默认超时。用户登录（扫码/短信）需要时间，给足。
const (
	defaultLoginTimeout = 10 * time.Minute
	defaultCDPTimeout   = 20 * time.Second
)

// Phase 表示登录流程所处的阶段。用于面板展示进度。
//
// ⚠️ 只表达"阶段"，不含任何凭据或响应内容。
type Phase string

const (
	PhaseStarting  Phase = "starting"  // 正在启动浏览器
	PhaseWaiting   Phase = "waiting"   // 等待用户在浏览器里完成登录
	PhaseCapturing Phase = "capturing" // 已见到凭据响应，正在读取
	PhaseDone      Phase = "done"      // 已拿到凭据
	PhaseFailed    Phase = "failed"    // 失败
	PhaseCanceled  Phase = "canceled"  // 已取消
)

// Credentials 是完成一次网页登录后得到的凭据。
//
// 🔴 这个结构**绝不**序列化到日志、事件或 API 响应。
// 它只在内存中从 login 包传到上层，由上层加密落盘。
type Credentials struct {
	AccessToken  string
	RefreshToken string

	// Nickname 是账号昵称（用于面板显示），非秘密。
	Nickname string
}

// Result 是一次登录流程的结果。
//
// 注意：**只有 Credentials 是秘密**，其余字段可安全用于日志与 API 响应。
type Result struct {
	Credentials Credentials
	// Phase 是结束时的阶段。
	Phase Phase
	// Err 在失败时非空（**不含凭据**）。
	Err error
}

// ProgressFunc 是进度回调。只接收阶段，不接收任何响应内容。
type ProgressFunc func(Phase)

// Login 执行一次完整的网页登录，返回凭据。
//
// 参数：
//   - timeout：用户完成登录的总时限（<=0 用默认 10 分钟）
//   - onProgress：可选的阶段回调（可为 nil）
//   - cancel：可选的取消通道（可为 nil）
//
// 返回的 Result 在成功时 Credentials 非空，在失败/取消时 Err 非空。
//
// 🔴 调用方**必须**检查 Err，不要把 Credentials 当作"一定有效"。
//
//	真正的"可用性"要靠后续查一次额度来证明。
func Login(timeout time.Duration, onProgress ProgressFunc, cancel <-chan struct{}) Result {
	// 保留旧签名：默认国内版（改造前的行为）。
	// 需要国际版请用 LoginFor。
	return LoginFor(PlatformCN, timeout, onProgress, cancel)
}

// LoginFor 执行一次指定平台的网页登录。
//
// 与 Login 的唯一区别是多一个 platform 参数：
//   - PlatformCN  → 登录页 www.codebuddy.cn，凭据端点 host 也是它
//   - PlatformIntl → 登录页 www.workbuddy.ai，凭据端点 host 也是它
//
// 🔴 平台必须**同时**作用于"登录页"与"凭据捕获白名单"。
//
//	只改其中一个会出现最诡异的失败：用户在正确的站点登录成功，
//	但程序在监听另一个域名的响应 ⇒ **凭据永远抓不到**，
//	表现成"登录一直在转圈"，且没有任何错误线索。
//	（cdp.go 的 credMatcher 与此处共用同一个 platform 参数来防这个分叉。）
func LoginFor(
	platform Platform,
	timeout time.Duration,
	onProgress ProgressFunc,
	cancel <-chan struct{},
) Result {
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}

	report := func(p Phase) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	// ── 1. 生成 state ──
	state, err := newState()
	if err != nil {
		report(PhaseFailed)
		return Result{Phase: PhaseFailed, Err: fmt.Errorf("login: 生成 state 失败: %w", err)}
	}

	// ── 2. 启动受控浏览器 ──
	report(PhaseStarting)
	startURL := LoginURLFor(platform, state)
	sess, err := StartBrowser(LoginOptions{StartURL: startURL})
	if err != nil {
		report(PhaseFailed)
		return Result{Phase: PhaseFailed, Err: err}
	}
	// 无论如何都要收尾：关浏览器 + 删 profile。
	defer sess.Close()

	// ── 3. 连接 CDP 并开启监听（必须在导航稳定前就绪）──
	creds, err := captureCredentials(sess, platform, state, timeout, report, cancel)
	if err != nil {
		if errors.Is(err, errCanceled) {
			report(PhaseCanceled)
			return Result{Phase: PhaseCanceled, Err: err}
		}
		report(PhaseFailed)
		return Result{Phase: PhaseFailed, Err: err}
	}

	report(PhaseDone)
	return Result{Credentials: creds, Phase: PhaseDone}
}

// errCanceled 是内部取消信号。
var errCanceled = errors.New("login: 已取消")

// newState 生成一个不可预测的 state。
//
// 为什么必须 crypto/rand：state 是凭据响应的**唯一关联标识**，
// 可预测的 state 会让"串扰"（拿到别人的/上一次的响应）成为可能。
func newState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 用 UUID v4 的形态，与官方登录页生成的 state 形态一致。
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// captureCredentials 连上 CDP，监听凭据响应并提取。
//
// ⚠️ platform 决定凭据捕获的**域名白名单**（见 credmatch.go）。
// 传错会让"登录成功但抓不到凭据"，且没有任何错误线索。
func captureCredentials(
	sess *BrowserSession,
	platform Platform,
	state string,
	timeout time.Duration,
	report func(Phase),
	cancel <-chan struct{},
) (Credentials, error) {
	return captureCredentialsWithTrigger(sess, platform, state, timeout, report, cancel, nil)
}

// triggerFunc 在生产会话就绪后收到该会话本身（**仅测试用**）。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要它（Codex 第 30 轮裁定）
// ═══════════════════════════════════════════════════════════════════
//
// 我最初写的端到端测试：另开一个 CDP 会话 B 去 `Runtime.evaluate`
// 发请求，生产会话 A 只监听。**A 什么都收不到。**
//
// Codex 裁定原因：
//
//	「`Network.enable` 的状态至少是 session 级的；不能假设 A 启用后，
//	   B 通过 `Runtime.evaluate` 发出的请求一定会送到 A。
//	   **生产代码不应要求另一个 CDP session 替它发请求。**」
//
// ⇒ 正确的测试要让**生产会话自己**触发请求。这个回调就是那个接缝：
//
//	capture 把自己的会话交出来，测试在**同一个会话**里发 fetch。
//
// 🔴 生产恒传 nil —— 有测试断言"不传 trigger 时行为完全不变"。
type triggerFunc func(sess *cdpSession)

// captureCredentialsWithTrigger 与 captureCredentials 相同，
// 但在 Network 域就绪后把**生产会话本身**交给 onReady（可为 nil）。
func captureCredentialsWithTrigger(
	sess *BrowserSession,
	platform Platform,
	state string,
	timeout time.Duration,
	report func(Phase),
	cancel <-chan struct{},
	onReady triggerFunc,
) (Credentials, error) {
	var zero Credentials

	// 找到页面 target。浏览器刚启动时可能还在 about:blank / 登录页之间切换，
	// 所以重试几次直到拿到 type=page 的 target。
	http := newCDPHTTPClient(sess.CDPBaseURL(), 5*time.Second)
	var pageURL string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		targets, err := http.listTargets()
		if err == nil {
			for _, t := range targets {
				if t.Type == "page" && t.WebSocketDebuggerURL != "" {
					pageURL = t.WebSocketDebuggerURL
					break
				}
			}
		}
		if pageURL != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if pageURL == "" {
		return zero, errors.New("login: 未能连接到浏览器页面（CDP target 未就绪）")
	}

	sessCDP, err := newCDPSession(pageURL, 10*time.Second)
	if err != nil {
		return zero, fmt.Errorf("login: 建立 CDP 会话失败: %w", err)
	}
	defer sessCDP.Close()

	// ── 请求跟踪（Codex 第 31 轮指定的状态机）──
	//
	// 不再用"多个互不关联的 map"：那样无法区分
	//   ① 元数据缺失（还没收到 requestWillBeSent）
	//   ② 真实方法不是 POST（重定向变 GET / OPTIONS 预检）
	// 而两者都会被报成"方法不是 POST"，修法却完全不同。
	// 详见 reqstate.go。
	var (
		captured  = make(chan []byte, 1)
		reqs      = newReqTracker()
		trace     = newEventTrace()
		sightings = newSightingLog()
	)

	// 让诊断可被测试读取（只读旁路，不影响判定）。
	publishSighting(sightings)

	// matcher 是本次捕获的端点匹配器（生产为严格校验；测试可替换端点）。
	//
	// ⚠️ Codex 第 29 轮要求接缝为**实例级**，不用包级可变全局 ——
	// 见 credmatch.go 的说明。
	matcher := credMatcherForCapture(platform)

	// tryFinish 的前置声明：它在回调里被引用，但定义在回调之后
	// （为了把大段注释留在回调外面，不打断事件分支的可读性）。
	// ⚠️ 用 var 声明而不是闭包字面量，避免"回调引用未初始化变量"。
	var tryFinish func(reqID string)

	// 🔴🔴 **必须先注册事件回调，再调 Network.enable！**
	//
	// 这里踩过一个真实事故（2026-10-05），且它**比自死锁更隐蔽**：
	// 原先写成 `Call("Network.enable")` 之后再 `SetEventHandler`，
	// 结果**真实登录永远抓不到凭据**（连续多次超时）。
	//
	// 原因：`Network.enable` 自身就会让 CDP **回放**一批已发生的
	// Network 事件（requestWillBeSent / responseReceived 等）。
	// 此时回调还没注册 ⇒ `readLoop` 拿到事件时 `onEvent == nil`
	// ⇒ 按 cdp.go 的设计**直接丢弃**。
	//
	// 而 `SetEventHandler` 的文档**早就写了**"必须在 Call 之前设置"——
	// 生产代码违反了自己的约定，且没有任何测试发现。
	//
	// 实测证据（同一台机器、同一个 target）：
	//
	//	生产代码：先 enable 后 setHandler → 收不到任何事件（超时）
	//	诊断观察者：先 setHandler 后 enable → 正常收到 200 响应
	//
	// ⚠️ 别把这段"整理"回原来的顺序 —— 看起来更顺，但会静默失效。
	// tryFinish 在"条件齐备"时启动取 body。
	//
	// ═══════════════════════════════════════════════════════════════
	// 🔴 四条关键约束（每一条都对应一个踩过的坑或 Codex 的要求）
	// ═══════════════════════════════════════════════════════════════
	//
	//  ① **方法缺失不可当 POST**：`readyForBody` 要求 method 非空。
	//     若响应先到、元数据后到，本次不取；等 requestWillBeSent 到达时
	//     再次调用本函数（Codex：「先保留为待关联状态」）。
	//
	//  ② **必须在独立 goroutine 里取 body**：回调由 readLoop 同步调用，
	//     同步调 `Call` 会形成"读循环等自己"的死锁（真实事故 A）。
	//
	//  ③ **取 body 前置位防重复**：markBodyFetchStarted 在锁内完成。
	//
	//  ④ **端点匹配在锁外做**：不持锁调 CDP。
	tryFinish = func(reqID string) {
		snap, ok := reqs.snapshot(reqID)
		if !ok || !snap.readyForBody() {
			// 元数据未齐 —— 保留状态，等后续事件补齐（不报错、不放弃）。
			return
		}
		// 🔴 端点匹配（含 state 与方法校验）。方法来自**请求侧**记录。
		reason := matcher.rejectReason(snap.responseURL, snap.method, state)
		if reason != "" {
			sightings.observeRejected(reason)
			// Codex 第 31 轮：方法校验失败要记下**实际方法**，
			// 以便区分"元数据缺失（值为空）"与"真实方法不符（GET/OPTIONS）"。
			if reason == rejectMethod {
				sightings.observeRejectedMethod(snap.method)
			}
			return
		}
		// 防重复：先占位再取。
		if !reqs.markBodyFetchStarted(reqID) {
			return
		}
		sightings.observeAccepted()

		// ⚠️⚠️ **必须在新 goroutine 里取 body，绝不能在回调内同步取！**
		//
		// 这里踩过一个真实事故（2026-10-05）：原先写成同步调用，
		// 结果**永远捕获不到凭据**，每次都拖到 10 分钟总超时。
		//
		// 原因：本回调是被 `readLoop` 那个 goroutine **同步调用**的
		// （见 cdp.go 的 readLoop → fn(...)），而 `Call` 需要
		// `readLoop` 读到应答才能返回。若在此处同步调 Call：
		//
		//	readLoop ──invokes──► 本回调 ──calls──► Call ──waits──► readLoop
		//	    ▲                                                          │
		//	    └──────────────── blocked ─────────────────────────────────┘
		//
		// 即"等待者"与"读循环"是同一个 goroutine ⇒ 自死锁，
		// 只能靠 Call 的超时解开，而那时代码会当成"取 body 失败"丢弃。
		//
		// 🔴 验证方式（已做红绿对照）：把这层 goroutine 去掉，
		// `TestCaptureE2ESameSessionFetch` 会**变红超时**，且诊断显示
		// 「已匹配但取响应体失败（CDP 调用超时）」—— 正是本事故的症状。
		go func(id string) {
			body, err := fetchResponseBody(sessCDP, id)
			if err != nil {
				sightings.noteFetchFailed(classifyFetchErr(err))
				return
			}
			if len(body) == 0 {
				sightings.noteFetchFailed("响应体为空")
				return
			}
			select {
			case captured <- body:
			default: // 已有候选，忽略后续
			}
		}(reqID)
	}

	sessCDP.SetEventHandler(func(ev cdpEvent) {
		// 诊断：记录收到的事件类型。
		// 用于区分"事件根本没到"与"到了但被过滤丢弃"——
		// 这两种的修法完全不同（2026-10-05 排查时正因此绕了弯路）。
		sightings.noteEvent(ev.Method)

		switch ev.Method {
		case "Network.requestWillBeSent":
			// 请求侧：登记 URL 与**方法**（responseReceived 不带方法）。
			//
			// 🔴 只跟踪"路径像目标端点"的请求 —— 登录过程会产生上百个
			// 请求，全记就是无上限增长（本项目红线 60 MB）。
			var p struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL    string `json:"url"`
					Method string `json:"method"`
				} `json:"request"`
				// RedirectResponse 非空表示这一跳是重定向的中间态。
				RedirectResponse json.RawMessage `json:"redirectResponse"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				sightings.noteUnmarshalFail(ev.Method, err)
				return
			}
			if !pathLooksLikeCredential(p.Request.URL) {
				return
			}
			reqs.onRequestWillBeSent(p.RequestID, p.Request.Method, p.Request.URL)

			host, path := splitHostPath(p.Request.URL)
			trace.add(traceEntry{
				Event:  ev.Method,
				ReqID:  truncateForTrace(p.RequestID, 24),
				Host:   host,
				Path:   path,
				Method: methodOrMissing(p.Request.Method),
				Redir:  len(p.RedirectResponse) > 0,
			})

			// ⚠️ 重定向**复用 requestId**：这一跳已覆盖上一跳的 method/URL。
			// 不能拿上一跳的 POST 去批准最终的 GET（Codex 第 31 轮警告）。
			// 上面的 onRequestWillBeSent 已经覆盖写，这里只是留个醒目的注释。

			// 若响应已先到（元数据晚到），此刻补一次判定。
			tryFinish(p.RequestID)

		case "Network.responseReceived":
			// 🔴🔴 字段类型必须与 CDP 实际下发的一致 —— 这里是**第三个**
			// 导致"永远抓不到凭据"的真实缺陷（2026-10-05 实测）。
			//
			// 原先声明：`From string \`json:"fromDiskCache"\``
			// 而 CDP 实际下发的是 **bool**：
			//
			//	json: cannot unmarshal bool into Go struct field
			//	      .response.fromDiskCache of type string
			//
			// 后果：**每一条** responseReceived 都 unmarshal 失败 →
			// 回调在取 URL 之前就 return → 凭据端点**永远匹配不上**。
			//
			// ⚠️ 这个缺陷被前两个（自死锁、回调注册顺序）**掩盖**了：
			// 修完那两个之后仍然仍不到，才暴露出它。
			// 也说明了「诊断必须能区分"事件没到"与"到了但解析失败"」——
			// 这正是 sightings.noteUnmarshalFail 存在的理由。
			//
			// 🔴 教训：**不要为用不到的字段声明类型**。
			// 这个字段我们根本没用，留着它却把整条链路毒死了。
			// 所以现在只声明真正需要的字段。
			var p struct {
				RequestID string `json:"requestId"`
				Response  struct {
					URL    string `json:"url"`
					Status int    `json:"status"`
				} `json:"response"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				sightings.noteUnmarshalFail(ev.Method, err)
				return
			}
			// 诊断：记下实际见过的 path（区分"路径不同"与"识别写错"）。
			sightings.noteRespURL(p.Response.URL)

			// 只登记，**不取 body**（Codex 第 28 轮要求）。
			reqs.onResponseReceived(p.RequestID, p.Response.URL, p.Response.Status)

			host, path := splitHostPath(p.Response.URL)
			trace.add(traceEntry{
				Event:  ev.Method,
				ReqID:  truncateForTrace(p.RequestID, 24),
				Host:   host,
				Path:   path,
				Status: p.Response.Status,
			})

			tryFinish(p.RequestID)

		case "Network.loadingFinished":
			var p struct {
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				sightings.noteUnmarshalFail(ev.Method, err)
				return
			}
			// 标记完成；若前置条件齐备则（在独立 goroutine 里）取 body。
			reqs.onLoadingFinished(p.RequestID)
			tryFinish(p.RequestID)

		case "Network.loadingFailed":
			var p struct {
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				return
			}
			// 失败：终止并清理**该 requestId**（Codex 第 31 轮要求）。
			reqs.onLoadingFailed(p.RequestID)
		}
	})

	// ── 回调已就位，现在才启用 Network 域 ──
	//
	// ⚠️ 顺序承重：必须在 SetEventHandler **之后**。
	// enable 会触发一批事件回放；回调若未就位，那些事件会被丢弃
	// （见上方详细说明与实测证据）。
	if _, err := sessCDP.Call("Network.enable", map[string]any{}, defaultCDPTimeout); err != nil {
		return zero, fmt.Errorf("login: 启用 Network 域失败: %w", err)
	}

	// 网络域已就绪 —— 此时把会话交给测试触发（生产恒为 nil）。
	//
	// ⚠️ 必须在 enable **之后**才触发，否则请求发生在域生效前，
	// 监听方必然看不到（Codex 第 30 轮：不能用另一个会话代发）。
	if onReady != nil {
		onReady(sessCDP)
	}

	report(PhaseWaiting)

	// ── 等待凭据 / 超时 / 取消 ──
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case body := <-captured:
			report(PhaseCapturing)
			creds, err := extractCredential(body)
			if err != nil {
				// 拿到了响应但解析失败 —— 可能是上游改了契约。
				// 不重试（同一个响应重试也一样），直接报错。
				return zero, err
			}
			return Credentials{
				AccessToken:  creds.accessToken(),
				RefreshToken: creds.refreshToken(),
			}, nil

		case <-timer.C:
			// 把"为什么没抓到"带上 —— 否则这里只能报一句"超时"，
			// 而根因可能在南辕北辙的地方（见 sighting.go 的说明）。
			if s := sightings.summary(); s != "" {
				return zero, fmt.Errorf("login: 等待登录超时（%s）；%s", timeout, s)
			}
			return zero, fmt.Errorf("login: 等待登录超时（%s）", timeout)

		case <-cancel:
			return zero, errCanceled

		case <-sessCDP.closed:
			// CDP 会话断了：可能是用户关了浏览器窗口。
			return zero, errors.New("login: 浏览器连接已断开（窗口是否被关闭？）")
		}
	}
}

// fetchResponseBody 取一个已完成的请求的响应体。
//
// 处理 base64Encoded：CDP 对非文本内容会把 body 编码成 base64，
// 不处理的话 JSON 解析必然失败。
func fetchResponseBody(sess *cdpSession, requestID string) ([]byte, error) {
	raw, err := sess.Call("Network.getResponseBody", map[string]any{
		"requestId": requestID,
	}, defaultCDPTimeout)
	if err != nil {
		return nil, err
	}
	var p struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if !p.Base64Encoded {
		return []byte(p.Body), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(p.Body)
	if err != nil {
		return nil, fmt.Errorf("login: 响应体 base64 解码失败: %w", err)
	}
	return decoded, nil
}

// ─────────────────────────────────────────────────────────────
// 便利函数：判断凭据"看起来"是否有效（不做网络验证）
// ─────────────────────────────────────────────────────────────

// LooksLikeJWT 判断字符串是否为三段式 JWT 形态。
//
// ⚠️ 这只用于**快速排除明显无效的输入**，不构成任何安全判定。
// Codex 明确提醒：「JWT 形态不足以判定成功」——
// 真正的可用性必须靠调用上游接口验证（见上层的 Credit 校验）。
// 这里也**不做验签**（解码 ≠ 验签）。
func LooksLikeJWT(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return strings.HasPrefix(parts[0], "ey")
}
