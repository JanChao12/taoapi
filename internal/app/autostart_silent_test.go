package app

import (
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
