package app

import (
	"os"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/autostart"
)

// ═══════════════════════════════════════════════════════════════════
// 开机自启"静默"的跨包一致性护栏（2026-10-07）
// ═══════════════════════════════════════════════════════════════════
//
// 委托方要求：**手动启动弹提示，开机自启不弹**。
//
// 实现方式：自启注册的命令行带 `--silent`，GUI 模式据此跳过 MessageBox。
//
// 🔴 这里有一个**跨包耦合**必须钉住：
//
//	`autostart.DefaultArgs` 里的 flag 字符串
//	与 `app.FlagSilent` 必须**完全相同**。
//	两者天各一方（autostart 刻意不 import app，避免反向依赖），
//	一旦谁改了字面量而另一边没跟，症状是：
//
//	  开机后服务**起来了但弹窗**（打扰用户），
//	  或者更糟 —— 若 flag 名写错，serve 会因
//	  "flag provided but not defined" **直接退出**，
//	  表现为"开机后服务根本没起来"，且没有任何提示。
//
// ⇒ 用本测试把两个常量钉在一起。

// TestAutostartArgsAreSilent 守：自启命令行必须带 --silent。
func TestAutostartArgsAreSilent(t *testing.T) {
	args := autostart.DefaultArgs
	if len(args) == 0 {
		t.Fatal("DefaultArgs 不应为空")
	}
	if args[0] != CmdServe {
		t.Errorf("自启第一个参数 = %q，期望 %q（明确的子命令）", args[0], CmdServe)
	}

	found := false
	for _, a := range args {
		if a == FlagSilent {
			found = true
		}
	}
	if !found {
		t.Fatalf("🔴 自启命令行缺少 %s（%v）—— "+
			"开机后会弹提示框打扰用户", FlagSilent, args)
	}
}

// TestAutostartSilentFlagMatchesApp 守：两个包里的 flag 字面量一致。
//
// 这是上面那条的"根因护栏"：即使两边都"看起来有 --silent"，
// 拼写不同也会让 serve 报 unknown flag 而退出。
func TestAutostartSilentFlagMatchesApp(t *testing.T) {
	if autostart.FlagSilent != FlagSilent {
		t.Fatalf("🔴 flag 字面量不一致：autostart.FlagSilent = %q，"+
			"app.FlagSilent = %q —— "+
			"开机自启会因 unknown flag 直接退出（服务根本起不来）",
			autostart.FlagSilent, FlagSilent)
	}
	// 顺手确认它确实是"带横线"的 flag 形态（不是子命令名）
	if !strings.HasPrefix(FlagSilent, "--") {
		t.Errorf("FlagSilent = %q，应是 -- 开头的 flag", FlagSilent)
	}
}

// TestSetAutoStartUsesDefaultArgs 守：**生产路径**必须复用 DefaultArgs。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么必须单独立这条（2026-10-08 委托人实测缺陷）
// ═══════════════════════════════════════════════════════════════════
//
//	委托人原话："我开机没有自启。"
//
//	真因：`autostart_bridge.go` 的 setAutoStart **自己另写了一份参数**
//	  `autostart.Enable(exe, []string{"serve"})` —— 漏了 `--silent`。
//	  ⇒ serve.go 的 `if rt == nil && *silent` 不成立
//	  ⇒ 自启进程不挂托盘、日志只写 stderr
//	  ⇒ GUI 子系统没有控制台 ⇒ **日志静默丢失**（实测：本次开机零日志记录）
//
//	⚠️ 上面两条测试为什么没抓到：
//	  `TestAutostartArgsAreSilent` / `TestAutostartSilentFlagMatchesApp`
//	  断言的对象都是 **`autostart.DefaultArgs`**（那个常量一直是对的），
//	  而**真正写进注册表的是本文件的 setAutoStart** —— 测试根本没碰它。
//	  ⇒ 典型的"测了常量、没测调用方"盲区；这正是护栏要补的位置。
//
// 用源码审查而非行为测试：setAutoStart 会**真的写本机注册表**
// （与 autostart_windows_test.go 的真实读写测试一样需要门控），
// 不适合在普通单测里调用。选择与 TestRelaunchNeverPassesAddrFlag 相同的做法。
func TestSetAutoStartUsesDefaultArgs(t *testing.T) {
	src, err := os.ReadFile("autostart_bridge.go")
	if err != nil {
		t.Fatalf("读取 autostart_bridge.go 失败: %v", err)
	}
	text := string(src)

	// 去掉注释行，只审可执行代码
	var codeLines []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		codeLines = append(codeLines, ln)
	}
	code := strings.Join(codeLines, "\n")

	// 1) 必须把 DefaultArgs 交给 Enable —— 这是"单一真值来源"。
	if !strings.Contains(code, "autostart.DefaultArgs") {
		t.Error("setAutoStart 必须复用 autostart.DefaultArgs，不能在调用处另写一份参数 —— " +
			"另写就会漏掉 --silent，导致自启进程不挂托盘且日志静默丢失" +
			"（2026-10-08 委托人实测踩到，原话\"我开机没有自启\"）")
	}

	// 2) 反向确认：不得再出现字面量参数列表（`[]string{"serve"}` 那种）。
	//	注释里会提到这个历史写法，所以只查可执行代码。
	if strings.Contains(code, `[]string{"serve"}`) {
		t.Error("setAutoStart 里出现了硬编码的 []string{\"serve\"} —— " +
			"这正是漏掉 --silent 的历史写法，请改为 autostart.DefaultArgs")
	}
}

// TestServeAcceptsSilentFlag 守：runServe 真的认识 --silent。
//
// ⚠️ 不能直接调 runServe（它会起服务并阻塞），所以这里断言
//
//	**flag 定义存在**。这个缺陷的症状极隐蔽：`serve --silent`
//	会因 "flag provided but not defined" 退出，用户只看到
//	"开机后服务没起来"，且**没有任何提示**。
func TestServeAcceptsSilentFlag(t *testing.T) {
	sf := newServeFlagSet()
	if sf.fs.Lookup("silent") == nil {
		t.Fatal("🔴 serve 未定义 --silent flag —— " +
			"开机自启（serve --silent）会因 unknown flag 直接退出")
	}
	// 顺带确认其余内部 flag 也还在（防止重构时误删）
	for _, name := range []string{"addr", "v", "restart-id", "panel-origin"} {
		if sf.fs.Lookup(name) == nil {
			t.Errorf("serve 的 --%s flag 丢失", name)
		}
	}
}
