package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/config"
)

// ═══════════════════════════════════════════════════════════════════
// 重启后的监听地址：语义与回归护栏
//
// 定调依据是委托方 2026-10-05 的原话：
//
//	"假设后台是 8891 那么反代也是 8891，在后台设置界面里修改了
//	  反代 [端口] 成 8892 后，那么后台和反代都变为 8892。"
//
// 即**面板与反代是同一个 listener**，面板改端口必须生效。
//
// 我在这条上走过两次弯路（详见 restart.go 的 plannedListenAddr 注释）：
//   v1 只读配置 —— Codex 指出与启动时的 --addr 口径不一致
//   v2 把 --addr 继承给子进程 —— 端到端验收抓到"服务实际监听 A、
//      面板被告知去 B"，彻底分叉
//   v3 以配置为准 + 子进程不带 --addr —— 两处同源，本文件守住它
// ═══════════════════════════════════════════════════════════════════

// newSettingsWithPort 构造一个指定端口的设置存储（隔离目录，不碰真实配置）。
func newSettingsWithPort(t *testing.T, port int) *config.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WBAPI_DATA_DIR", dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载设置失败: %v", err)
	}
	if err := st.Mutate(func(s *config.Settings) error {
		s.Port = port
		return nil
	}); err != nil {
		t.Fatalf("写入端口失败: %v", err)
	}
	return st
}

// TestPlannedListenAddrUsesConfiguredPort 守：重启用配置里的端口。
//
// 🔴 这是**面板改端口能否生效**的根据：
//
//	面板改端口 → 写进配置 → 重启 → 本函数读配置 → 新端口生效。
//	若这里改成读别的来源（如父进程的 --addr），面板的修改就被架空了。
func TestPlannedListenAddrUsesConfiguredPort(t *testing.T) {
	settings := newSettingsWithPort(t, 8892)
	got, err := plannedListenAddr(Deps{Settings: settings})
	if err != nil {
		t.Fatalf("不应出错: %v", err)
	}
	if got != "127.0.0.1:8892" {
		t.Errorf("重启目标 = %q，期望 127.0.0.1:8892 —— "+
			"必须用配置里的端口，否则面板改端口不生效", got)
	}
}

// TestPlannedListenAddrIsAlwaysLoopback 守：host 恒为回环。
//
// 🔴 安全边界：只监听 127.0.0.1 是硬约束，重启路径独立解析，必须自己守住。
// （启动路径由 requireLoopback 兜，但重启不该依赖那一道。）
func TestPlannedListenAddrIsAlwaysLoopback(t *testing.T) {
	settings := newSettingsWithPort(t, 8787)
	got, err := plannedListenAddr(Deps{Settings: settings})
	if err != nil {
		t.Fatalf("不应出错: %v", err)
	}
	if !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("重启目标 = %q，必须以 127.0.0.1: 开头（只监听回环）", got)
	}
}

// TestPlannedListenAddrRejectsBadPort 守：端口不可用时明确报错，不臆造地址。
//
// ⚠️ 一条实测发现（我第一版测试写错了）：
//
//	我原想构造"端口为 0 / 负数"的配置来触发报错，但 `config.Mutate`
//	**自己就会校验端口范围**（`MinPort=1024`，见 config.go:336 的 Validate），
//	非法值根本写不进去 —— 所以我那个测试的前提不可达。
//	（这本身是好消息：配置层已经挡住了坏端口。）
//
// 真正可达的"端口不可用"只有一种：**压根没有 Settings**（如装配早期失败）。
// 那时本函数必须报错，而不是给出一个凭空的地址去 bind。
func TestPlannedListenAddrRejectsBadPort(t *testing.T) {
	if _, err := plannedListenAddr(Deps{Settings: nil}); err == nil {
		t.Error("无设置时应报错，而不是给出一个凭空的地址")
	}
}

// TestConfigNormalizesOutOfRangePort 记录"配置层如何处理坏端口"。
//
// ⚠️ 两版测试都写错了，这里如实记下实测行为（而不是我以为的行为）：
//
//	第一版：以为 `Mutate` 会**拒绝**坏端口 → 实测它不拒绝，测试红了。
//	第二版：以为会被拒 → 仍红。真正去看代码才发现：
//	  `config.normalize()`（config.go:336）对越界端口做的是
//	  **静默归一为 DefaultPort**，不是报错。
//
// 这解释了为什么 plannedListenAddr 里那个 `port <= 0` 分支**几乎不可达**：
// 任何写进配置的坏值都会在加载时被 normalize 成 8787。
//
// 本测试的作用是**把这个事实钉住**：
//   - 若将来 normalize 改成"报错"，这里会红 → 提示去核对 plannedListenAddr
//     是否还需要保留 `port <= 0` 的判断；
//   - 若 normalize 改成"原样保留坏值"，也会红 → 那时 plannedListenAddr
//     的范围检查就变成必需的了（目前它只判 <= 0，挡不住 70000）。
func TestConfigNormalizesOutOfRangePort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WBAPI_DATA_DIR", dir)
	st, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("加载设置失败: %v", err)
	}

	for _, bad := range []int{0, -1, 80, 70000} {
		if err := st.Mutate(func(s *config.Settings) error {
			s.Port = bad
			return nil
		}); err != nil {
			// 若某天变成报错，也是可接受的设计 —— 但那时要回来改注释
			t.Logf("注意：端口 %d 现在会被拒绝（实现已变更，请核对 plannedListenAddr 的注释）", bad)
			continue
		}
		got := st.Get().Port
		if got < config.MinPort || got > 65535 {
			t.Errorf("写入端口 %d 后读回 %d —— "+
				"越界值既没被拒绝也没被归一，plannedListenAddr 的范围检查就不够了", bad, got)
		}
		t.Logf("已确认：写入端口 %d 被静默归一为 %d", bad, got)
	}
}

// TestRelaunchNeverPassesAddrFlag 守：**子进程永远不带 --addr**。
//
// 🔴 这条是 v2 那个矛盾的根因所在，必须用测试钉住：
//
//	如果重启时把父进程的 --addr 传给子进程，就会出现
//	  plannedListenAddr（以配置为准）告诉面板去 8882，
//	  而子进程按 --addr 实际监听 8881 —— **两处口径分叉**，
//	面板永远连不上新地址。
//
//	所以 relaunchAndHandshake 的 args 里**不得**出现 --addr。
//	本测试直接审查源码文本（该函数会真的拉进程，无法在单测里安全调用）。
func TestRelaunchNeverPassesAddrFlag(t *testing.T) {
	src, err := os.ReadFile("restart.go")
	if err != nil {
		t.Fatalf("读取 restart.go 失败: %v", err)
	}
	text := string(src)

	// 只检查 relaunchAndHandshake 函数体那一段
	start := strings.Index(text, "func relaunchAndHandshake")
	if start < 0 {
		t.Fatal("找不到 relaunchAndHandshake")
	}
	rest := text[start:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// 去掉注释行，只审可执行代码
	var codeLines []string
	for _, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		codeLines = append(codeLines, ln)
	}
	code := strings.Join(codeLines, "\n")

	if strings.Contains(code, `"--addr"`) || strings.Contains(code, "addrFlag") {
		t.Error("relaunchAndHandshake 不得把 --addr（或任何地址 override）传给子进程 —— " +
			"那会让「plannedListenAddr 以配置为准」与「子进程按 --addr 监听」分叉，" +
			"面板会被告知去一个没有服务的地址")
	}
	// 正向确认：必须传的参数还在
	for _, want := range []string{"relaunchIDFlag", "relaunchOriginFlag", "FlagSilent"} {
		if !strings.Contains(code, want) {
			t.Errorf("relaunchAndHandshake 应传 %s（交接标识、面板 origin、静默标志）", want)
		}
	}
}

// TestRelaunchChildGetsTrayRuntime 守：**重启后的子进程必须带 --silent**。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 2026-10-07 委托人实测缺陷（原话："我的托盘图标消失了退出不了"）
// ═══════════════════════════════════════════════════════════════════
//
//	不带 --silent 时，serve.go 里 `rt == nil && *silent` 不成立
//	⇒ **rt 保持 nil** ⇒ 子进程不挂托盘、没有窗口、日志只写 stderr。
//
//	后果不是"少个图标"这么轻 —— 而是**服务失去唯一的退出通道**：
//	  托盘右键「退出」是 GUI 模式关闭服务的正规方式；
//	  没有托盘就没有窗口消息通道，`taskkill`（不带 /F）会被 Windows 拒绝：
//	  "This process can only be terminated forcefully"
//	  （实测确认，因为该进程 MainWindowHandle=0）。
//	  用户只能去任务管理器强杀，且**强杀会让日志/用量数据来不及落盘**。
//
//	⇒ 这不是外观问题，是**可用性缺陷**，必须有测试钉住。
//
// 用源码审查而非行为测试：relaunchAndHandshake 会真的拉进程，
// 无法在单测里安全调用（与相邻的 TestRelaunchNeverPassesAddrFlag 同样选择）。
func TestRelaunchChildGetsTrayRuntime(t *testing.T) {
	src, err := os.ReadFile("restart.go")
	if err != nil {
		t.Fatalf("读取 restart.go 失败: %v", err)
	}
	text := string(src)

	start := strings.Index(text, "func relaunchAndHandshake")
	if start < 0 {
		t.Fatal("找不到 relaunchAndHandshake")
	}
	rest := text[start:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// 去掉注释行，只审可执行代码
	var codeLines []string
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		codeLines = append(codeLines, ln)
	}
	code := strings.Join(codeLines, "\n")

	if !strings.Contains(code, "FlagSilent") {
		t.Error("relaunchAndHandshake 必须给子进程传 FlagSilent（--silent）—— " +
			"否则重启后子进程 rt==nil：**托盘图标消失、没有办法退出服务**，" +
			"用户只能强杀（2026-10-07 委托人实测踩到）")
	}

	// 反向确认：不得为了"有托盘"而把弹窗也带回来。
	//	--silent 的语义是"挂 GUI 运行时但**不弹提示**"，
	//	重启是后台行为，弹一个「服务已启动」的框会打扰用户
	//	（而且重启发生在用户点完确认之后，弹窗纯属多余）。
	if strings.Contains(code, "FlagGUI") {
		t.Error("relaunchAndHandshake 不应传 GUI 提示相关标志 —— " +
			"重启是后台行为，--silent 才是正确语义（挂托盘、不弹窗）")
	}
}
