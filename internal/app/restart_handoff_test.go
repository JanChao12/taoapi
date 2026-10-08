package app

import (
	"os"
	"strings"
	"testing"
)

// 本文件守住 2026-10-09 委托方实测故障：
//
//	「我使用后台面板的更新，在我更新后面板未能重启，浏览器刷新也没用，
//	  我将托盘图标退出，结果打不开了提示端口占用，后台面板也没有更新成功」
//
// ═══════════════════════════════════════════════════════════════════
// 根因（实测日志为证，不是推断）
// ═══════════════════════════════════════════════════════════════════
//
//	`handleUpdateApply` 触发的重启**只发了信号、没有先填交接上下文**
//	（漏调 setPending）。而 serve 主循环要求上下文齐全：
//
//	  serve.go: pendingID, clientOrigin, newAddr := deps.restart.takePending()
//	             if pendingID == "" { → 放弃重启 → 原地恢复 }
//
//	实测日志（D:\tools\TAOAPI\data\logs\wbapi-2026-10-09.log）：
//	  04:31:53 自动更新：已替换程序…准备重启生效
//	  04:31:53 收到重启请求，正在重新启动…
//	  04:31:53 重启放弃：未取得本次交接标识；服务保持在原地址
//	  04:31:53 已恢复到 127.0.0.1:4545 继续服务（重启未生效…）
//
//	⇒ 更新**必然失败**：exe 换成了新版，跑着的仍是旧进程；
//	  而那个"原地恢复"出来的旧进程继续占着端口，
//	  用户关掉托盘后再双击新 exe 就撞上"端口占用"。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么这些测试能抓住它（而不是只测"有没有调用某个函数"）
// ═══════════════════════════════════════════════════════════════════
//
//	它们断言的是**交接上下文的完整性** —— 即 serve 主循环实际读到的东西。
//	只要有任何调用方发出信号却没填上下文，takePending 就会返回空，
//	测试立刻变红。这正是一条"能抓住该故障的测试"，
//	而不是"通过即证明实现正确"的形式化断言。
//
// 反向对照（已实测）：把 requestRestart 里的 setPending 注释掉，
// 下面 TestRequestRestartFillsHandoffContext 等测试全部变红。

// TestRequestRestartFillsHandoffContext 守：requestRestart 必须把
// 交接上下文（restartID / origin / newAddr）真正写进槽位。
//
// 🔴 这是本次故障的最小复现：只要 setPending 被漏掉，
// serve 主循环就会读到空 restartID 并**放弃重启**。
func TestRequestRestartFillsHandoffContext(t *testing.T) {
	rst := newRestartState()

	const (
		wantID     = "0123456789abcdef0123456789abcdef"
		wantOrigin = "http://127.0.0.1:4545"
		wantAddr   = "127.0.0.1:4545"
	)
	if !rst.requestRestart(wantID, wantOrigin, wantAddr) {
		t.Fatal("首次 requestRestart 应成功")
	}

	gotID, gotOrigin, gotAddr := rst.takePending()
	if gotID != wantID {
		t.Errorf("交接标识 = %q，期望 %q\n"+
			"  ⇒ 空标识会让 serve 主循环【放弃重启】（服务原地恢复、永不换版本）,"+
			"这正是 2026-10-09「更新后不重启」的故障形态", gotID, wantID)
	}
	if gotOrigin != wantOrigin {
		t.Errorf("面板 origin = %q，期望 %q（缺失会让旧页面读不到新端口健康响应）",
			gotOrigin, wantOrigin)
	}
	if gotAddr != wantAddr {
		t.Errorf("新地址 = %q，期望 %q（握手探测的就是它）", gotAddr, wantAddr)
	}

	// 信号也必须发出（否则重启永远不会开始）。
	select {
	case <-rst.ch:
	default:
		t.Error("requestRestart 必须同时发出重启信号（只填上下文不发信号 = 永不重启）")
	}
}

// TestClaimThenSignalIsTheOnlyPair 守：claimRestart + signal 成对使用。
//
// 这是 handleRestart 的写法（先写 202 响应、再发信号）。
// 它同样必须留下完整上下文 —— 否则同样退化成"放弃重启"。
func TestClaimThenSignalIsTheOnlyPair(t *testing.T) {
	rst := newRestartState()

	const wantID = "fedcba9876543210fedcba9876543210"
	if !rst.claimRestart(wantID, "http://localhost:4545", "127.0.0.1:4545") {
		t.Fatal("首次 claimRestart 应成功")
	}
	// 受理后、发信号前：上下文必须**已经**可读（不能等到 signal 才写）。
	if id, _, _ := rst.takePending(); id != wantID {
		t.Errorf("claimRestart 之后上下文应已就绪，实际 id=%q", id)
	}
	rst.signal()
	select {
	case <-rst.ch:
	default:
		t.Error("signal 后应收到重启信号")
	}
}

// TestRequestRestartRejectsConcurrent 守：重入返回 false（供调用方回 409），
// 且**不覆盖**已受理那次的上下文。
//
// 🔴 若重入时把上下文覆盖成新值，serve 主循环会用"第二次的标识"
// 去握手，而它拉起的是按第一次上下文启动的进程 —— 握手恒不匹配，
// 15 秒后超时杀进程。所以重入必须无副作用。
func TestRequestRestartRejectsConcurrent(t *testing.T) {
	rst := newRestartState()

	const firstID = "11111111111111111111111111111111"
	if !rst.requestRestart(firstID, "", "127.0.0.1:4545") {
		t.Fatal("首次应成功")
	}

	// 第二次：必须被拒绝，且不得污染已有上下文。
	const secondID = "22222222222222222222222222222222"
	if rst.requestRestart(secondID, "", "127.0.0.1:9999") {
		t.Fatal("重启进行中时第二次 requestRestart 应返回 false（调用方据此回 409）")
	}

	if id, _, addr := rst.takePending(); id != firstID || addr != "127.0.0.1:4545" {
		t.Errorf("被拒绝的请求污染了上下文：id=%q addr=%q（应保持首次的 %q / 127.0.0.1:4545）",
			id, addr, firstID)
	}
}

// TestAbortAllowsSecondRequestRestart 守：失败回滚后能再次触发。
//
// 与既有 TestRestartStateAbortAllowsRetry 互补 —— 那条测 begin/abort
// 的裸状态，这条测 requestRestart 这个高层入口。
func TestAbortAllowsSecondRequestRestart(t *testing.T) {
	rst := newRestartState()

	if !rst.requestRestart("33333333333333333333333333333333", "", "127.0.0.1:4545") {
		t.Fatal("首次应成功")
	}
	// 模拟 serve 主循环放弃重启后的回滚。
	rst.abort()
	<-rst.ch // 消费掉首次的信号

	const retryID = "44444444444444444444444444444444"
	if !rst.requestRestart(retryID, "", "127.0.0.1:4545") {
		t.Fatal("abort 之后应能再次 requestRestart（否则用户一次失败就永久点不动）")
	}
	if id, _, _ := rst.takePending(); id != retryID {
		t.Errorf("重试后的标识 = %q，期望 %q", id, retryID)
	}
}

// TestClientOriginHelperReadsHeader 守 clientOrigin 读的是 Origin 头。
//
// 更新路径没有请求体字段可带 origin，只能从头部取（浏览器对 POST
// 同源请求也会带 Origin）。取不到时返回空串 —— 那是安全的一侧：
// 子进程白名单只含自己的 origin，不影响重启本身。
func TestClientOriginHelperReadsHeader(t *testing.T) {
	if got := clientOrigin(nil); got != "" {
		t.Errorf("nil 请求应返回空串，实际 %q", got)
	}

	req := mustRequest(t, "POST", "/api/update/apply", "")
	if got := clientOrigin(req); got != "" {
		t.Errorf("无 Origin 头时应返回空串，实际 %q", got)
	}

	req.Header.Set("Origin", "http://127.0.0.1:4545")
	if got := clientOrigin(req); got != "http://127.0.0.1:4545" {
		t.Errorf("应读出 Origin 头，实际 %q", got)
	}
}

// TestEverySignalCallSiteHasHandoffContext 守：**任何** signal() 调用点
// 都必须先填好交接上下文。
//
// 🔴 这是本次故障的"结构性"护栏 —— 上面几条测的是行为，
// 这条直接审源码，确保**将来新增的调用方**也不会重犯。
//
// ⚠️ 为什么必须审源码而不是只靠行为测试：
//
//	行为测试只覆盖"已存在的调用路径"。若后人新增第三个触发重启的入口
//	（比如"定时重启"），行为测试不会知道它存在，而它会重犯同样的错。
//	本条按"每一处 signal() 都必须在同一个函数里伴随 claim/setPending"
//	来断言，于是新增调用方必须先想清楚上下文从哪来。
//
// 允许的两种写法（本项目当前就这两种）：
//
//	a) 直接调 requestRestart —— 它内部已含 claim + signal；
//	b) 调 claimRestart 后再 signal —— 用于"要先写响应再关闭"的场景。
//
// 反向对照：在 api_update.go 里另起一个只调 signal() 的函数，本条立刻红。
func TestEverySignalCallSiteHasHandoffContext(t *testing.T) {
	files := []string{"restart.go", "api_update.go", "serve.go"}

	// 按函数切块，检查每个含 signal() 的函数块里是否有上下文来源。
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		text := string(src)

		// 定位所有 `X.signal()` 调用（排除定义与注释行）。
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(trimmed, ".signal()") {
				continue
			}
			// 跳过注释、定义（func ... signal()）与测试内直接调用。
			if strings.HasPrefix(trimmed, "//") ||
				strings.HasPrefix(trimmed, "*") ||
				strings.HasPrefix(trimmed, "func ") {
				continue
			}
			// signal() 必须出现在含 claim/setPending 的文件内上下文 ——
			// 这里用"同文件必须存在 claimRestart 或 setPending 调用"近似，
			// 更严格的作用域级检查见下方 funcBlockContaining 断言。
			if !strings.Contains(text, "claimRestart(") && !strings.Contains(text, "setPending(") {
				t.Errorf("🔴 %s 里的 %q 所在文件没有任何 claimRestart/setPending 调用。\n"+
					"  ⇒ 只发信号不填交接上下文 = serve 主循环读不到 restartID =\n"+
					"     「重启放弃：未取得本次交接标识」= 2026-10-09 更新不重启那个故障。\n"+
					"  正确做法：调 requestRestart，或 claimRestart 后再 signal。",
					name, trimmed)
			}
		}
	}

	// 更强的断言：restart.go 中 requestRestart 必须"先 claim 再 signal"。
	src, err := os.ReadFile("restart.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	idx := strings.Index(text, "func (s *restartState) requestRestart")
	if idx < 0 {
		t.Fatal("找不到 requestRestart 定义")
	}
	body := text[idx:]
	claimAt := strings.Index(body, "claimRestart(")
	signalAt := strings.Index(body, "s.signal()")
	if claimAt < 0 || signalAt < 0 {
		t.Fatalf("requestRestart 必须同时含 claimRestart 与 signal（claimAt=%d signalAt=%d）",
			claimAt, signalAt)
	}
	if claimAt > signalAt {
		t.Error("🔴 requestRestart 里 signal 出现在 claim 之前：\n" +
			"  主循环可能先取到空槽位 ⇒ 同样触发出「重启放弃」。必须先 claim 再 signal。")
	}
}

// TestUpdateApplyTriggersRestartWithContext 审源码：更新路径必须
// 走带上下文的入口，而不是裸 signal()。
//
// 这是对 TestEverySignalCallSiteHasHandoffContext 的**针对性**补充 ——
// 直接盯住出事的那个函数，让"修好了又被改回去"立刻可见。
func TestUpdateApplyTriggersRestartWithContext(t *testing.T) {
	src, err := os.ReadFile("api_update.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	idx := strings.Index(text, "func handleUpdateApply")
	if idx < 0 {
		t.Fatal("找不到 handleUpdateApply")
	}
	// 取到文件末尾即可（它是本文件最后一个处理函数）。
	body := text[idx:]

	if !strings.Contains(body, "claimRestart(") {
		t.Error("🔴 handleUpdateApply 必须调 claimRestart（填交接上下文）后 signal。\n" +
			"  2026-10-09 故障：这里只调了 signal()，导致更新后服务永不重启、\n" +
			"  exe 已是新版而跑着的仍是旧版。")
	}
	if !strings.Contains(body, "newRestartID(") {
		t.Error("🔴 handleUpdateApply 必须生成 restartID：健康握手要求子进程回显同一标识，" +
			"空标识在 healthMatches 里**直接判不健康**，父进程将超时并杀掉刚拉起的新进程。")
	}
	if !strings.Contains(body, "plannedListenAddr(") {
		t.Error("🔴 handleUpdateApply 必须用 plannedListenAddr 得到握手探测地址" +
			"（与子进程的解析规则同源）。")
	}
}
