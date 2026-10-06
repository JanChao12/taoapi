// readloop_guard_test.go：钉住"取响应体绝不能阻塞 CDP 读循环"这条红线。
//
// ═══════════════════════════════════════════════════════════════════
// 这是一个**真实事故**的护栏（2026-10-05）
// ═══════════════════════════════════════════════════════════════════
//
// **现象**：真实登录时，独立抓包工具明明看到
// `POST /console/login/enterprise?state=…` 返回 **200**，
// 但 `login.Login()` **永远捕获不到凭据**。
// 连续三次真实登录，每次都在 10 分钟总超时后失败，暂存文件从未写出。
//
// **根因**：`login.go` 原先在 `Network.loadingFinished` 的
// **事件回调内同步**调用 `fetchResponseBody`，而它内部走
// `sess.Call(...)` —— 需要 `readLoop` 读到应答才能返回。
// 但该回调**正是被 `readLoop` 那个 goroutine 同步调用的**
// （见 cdp.go: `fn(cdpEvent{...})`），于是：
//
//	readLoop ──invokes──► onEvent ──calls──► Call ──waits for──► readLoop
//	    ▲                                                            │
//	    └──────────────────── blocked ───────────────────────────────┘
//
// "等待者"与"读循环"是同一个 goroutine ⇒ 自死锁，
// 只能靠 `Call` 的 20 秒超时解开，之后凭据被当成"取 body 失败"静默丢弃。
//
// **修复**：把取 body 派发到独立 goroutine，让等待者与读循环分离。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么用**静态断言**而不是运行时测试
// ═══════════════════════════════════════════════════════════════════
//
// 原先 `integration_test.go` 里有一个"看起来覆盖同一条链路"的测试
// `TestCDPEventRoutingAndBodyFetch` —— 但它把生产逻辑**抄了一遍**
// （自己重写 candidates/finished 通道，且在回调**外**取 body），
// 因此**天然不含这个死锁**，全绿。
//
// **教训：测试抄生产逻辑 = 测了个假的。**
//
// 要真正抓住它需要"真实端点 + 真实浏览器"，而该端点的 host/path
// 是精确匹配的，本地服务冒充不了。所以这里改用静态断言 ——
// 它能在一秒内对**结构性不变量**给出确定结论，不依赖浏览器与网络。
//
// 运行时链路（浏览器 / CDP 事件是否流动）由 `integration_test.go` 覆盖，
// 两者互补。
package login

import (
	"os"
	"strings"
	"testing"
)

// TestCaptureNeverBlocksReadLoop 断言**取 body 的工作在独立 goroutine 中**。
//
// ═══════════════════════════════════════════════════════════════════
// ⚠️ 结构在 2026-10-05 变了，本测试随之更新（Codex 第 31 轮要求的状态机）
// ═══════════════════════════════════════════════════════════════════
//
// 原先：取 body 直接写在 `loadingFinished` 分支里。
// 现在：所有分支都调 `tryFinish`，取 body 统一在 `tryFinish` 里做
// （因为"条件齐备"可能在任意一个事件之后达成 —— 例如响应先到、
// 请求元数据后到，那时要等 requestWillBeSent 到达才取）。
//
// ⇒ 因此断言对象从"某个分支"改为"tryFinish 这个函数"。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 Codex 第 29 轮的批评（我接受）
// ═══════════════════════════════════════════════════════════════════
//
//	「"代码里存在 goroutine"的静态断言**守不住死锁行为**。
//	  补一个有限超时的行为测试。」
//
// ⇒ 行为级验证在 `capture_e2e_test.go` 的
//
//	`TestCaptureE2ESameSessionFetch`：它真的启动浏览器、走完整链路，
//	去掉 goroutine 后会**超时变红**（已做红绿对照）。
//
// 本静态断言仍然保留，但定位改为"**快速**给出结构性结论"
// （1 秒内，不需要浏览器），行为测试才是真正的守门人。
func TestCaptureNeverBlocksReadLoop(t *testing.T) {
	src := readFileOrFail(t, "login.go")

	// 定位 tryFinish 函数体（取 body 的唯一位置）。
	start := strings.Index(src, "tryFinish = func(reqID string)")
	if start < 0 {
		t.Fatal("未找到 tryFinish 定义 —— 结构可能已变，请更新本测试")
	}
	// 取到下一个顶层 "sessCDP.SetEventHandler" 为止。
	rest := src[start:]
	if j := strings.Index(rest, "sessCDP.SetEventHandler"); j > 0 {
		rest = rest[:j]
	}
	body := rest

	if !strings.Contains(body, "fetchResponseBody") {
		t.Fatal("tryFinish 里没有 fetchResponseBody —— 结构可能已变，请更新本测试")
	}

	// ── 断言 1：必须存在 goroutine 派发 ──
	goPos := strings.Index(body, "go func(")
	if goPos < 0 {
		t.Error("❌ 取 body 不在 goroutine 中 —— " +
			"会在 readLoop 上同步等待其自身的应答，导致自死锁。" +
			"（真实事故：真实登录连续多次 10 分钟超时、抓不到任何凭据。\n" +
			"  行为级验证见 TestCaptureE2ESameSessionFetch）")
	}

	// ── 断言 2：fetchResponseBody 不得出现在 goroutine 之外 ──
	if stripped := stripGoFuncs(body); strings.Contains(stripped, "fetchResponseBody") {
		t.Error("❌ fetchResponseBody 出现在 goroutine **之外** —— " +
			"这会在 readLoop 上同步等待应答，必然自死锁")
	}

	// ── 断言 3：必须先占位再派发（防重复取 body）──
	markPos := strings.Index(body, "markBodyFetchStarted")
	if markPos < 0 {
		t.Error("❌ 未找到 markBodyFetchStarted —— 结构可能已变")
	} else if goPos >= 0 && markPos > goPos {
		t.Error("❌ 防重复占位在 goroutine 之后 —— " +
			"应先占位再派发，避免同一 requestId 被重复处理")
	}
}

// readFileOrFail 读源码文件，失败即 Fatal。
func readFileOrFail(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return string(b)
}

// TestEventCallbackRunsOnReadLoop 钉住本护栏**赖以成立的前提**：
// 事件回调与读循环在同一个 goroutine 上。
//
// 若将来有人把事件回调改成"独立 goroutine 派发"，那么
// 「回调内同步 Call」就不再死锁 —— 届时上面那条断言
// 应当**重新评估**（可能可以放宽），而不是盲目保留。
//
// 这条测试的作用是：让那次改动**显式暴露**，而不是悄悄发生。
func TestEventCallbackRunsOnReadLoop(t *testing.T) {
	b, err := os.ReadFile("cdp.go")
	if err != nil {
		t.Fatalf("读取 cdp.go 失败: %v", err)
	}
	src := string(b)

	i := strings.Index(src, "func (s *cdpSession) readLoop()")
	if i < 0 {
		t.Fatal("未找到 readLoop")
	}
	loop := src[i:]
	if j := strings.Index(loop, "\nfunc "); j > 0 {
		loop = loop[:j]
	}

	if strings.Contains(loop, "go fn(") {
		t.Log("⚠️ 事件回调已改为异步派发（go fn）—— " +
			"此时「回调内同步 Call」不再死锁，" +
			"请重新评估 TestCaptureNeverBlocksReadLoop 的断言是否仍需保留")
		return
	}
	if !strings.Contains(loop, "fn(cdpEvent{") {
		t.Error("readLoop 里没有找到同步调用 fn(cdpEvent{...}) —— 结构已变，请更新本测试")
	}
	t.Log("✅ 回调在 readLoop 上同步执行 —— 护栏前提成立，" +
		"因此「回调内取 body 必须派发到独立 goroutine」这条约束必须保留")
}

// TestStripGoFuncsHelper 自测 stripGoFuncs —— 辅助函数错了会让主断言假绿。
func TestStripGoFuncsHelper(t *testing.T) {
	in := "x := 1\n" +
		"go func(id string) {\n" +
		"\tbody := fetchResponseBody(sess, id)\n" +
		"\t_ = body\n" +
		"}(p.ID)\n" +
		"y := 2"
	got := stripGoFuncs(in)
	if strings.Contains(got, "fetchResponseBody") {
		t.Errorf("stripGoFuncs 没剥干净，会导致主断言假绿:\n%s", got)
	}
	if !strings.Contains(got, "x := 1") || !strings.Contains(got, "y := 2") {
		t.Errorf("stripGoFuncs 误删了周围代码:\n%s", got)
	}

	// 反向：同步版本必须**保留** fetchResponseBody，否则主断言抓不到缺陷。
	sync := "body := fetchResponseBody(sess, id)\n_ = body"
	if !strings.Contains(stripGoFuncs(sync), "fetchResponseBody") {
		t.Error("stripGoFuncs 把同步调用也删了 —— 主断言会永远通过（假绿）")
	}
}

// ─────────────────────────────────────────────────────────────
// 辅助
// ─────────────────────────────────────────────────────────────

// loadingFinishedBranch 返回 login.go 里 loadingFinished 分支的源码片段。
func loadingFinishedBranch(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("login.go")
	if err != nil {
		t.Fatalf("读取 login.go 失败: %v", err)
	}
	src := string(b)

	i := strings.Index(src, `case "Network.loadingFinished":`)
	if i < 0 {
		t.Fatal("未找到 loadingFinished 分支 —— 代码结构变了")
	}
	rest := src[i:]
	if j := strings.Index(rest, `case "Network.loadingFailed":`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// stripGoFuncs 移除 `go func(...) { ... }(args)` 整段，返回剩余源码。
//
// 实现刻意简单（找 `go func(` → 配对花括号 → 跳过调用参数）；
// 失败时**倾向保留原文**，从而让"没剥干净"表现为断言变红，
// 而不是静默通过。辅助函数本身由 TestStripGoFuncsHelper 自测。
func stripGoFuncs(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "go func(")
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i])

		braceStart := strings.Index(s[i:], "{")
		if braceStart < 0 {
			out.WriteString(s[i:])
			return out.String()
		}
		braceStart += i

		depth := 0
		j := braceStart
		for ; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				break
			}
		}

		// 跳过 `(args)`
		k := j + 1
		if k < len(s) && s[k] == '(' {
			d := 0
			for ; k < len(s); k++ {
				if s[k] == '(' {
					d++
				} else if s[k] == ')' {
					d--
					if d == 0 {
						k++
						break
					}
				}
			}
		}
		s = s[k:]
	}
}
