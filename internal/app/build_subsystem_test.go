package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildScriptProducesGUISubsystem 守：构建脚本必须编出 GUI 子系统。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 2026-10-07 委托人实测反馈
// ═══════════════════════════════════════════════════════════════════
//
//	原话："我是用户要使用又不是调试测试员，搞个终端干嘛"
//
//	现象：双击 exe 弹出一个常驻的黑色终端窗口。
//	根因：`scripts/build.ps1` 的 go build **漏了 -H=windowsgui**
//	      ⇒ 产出 WINDOWS_CONSOLE 子系统（PE 头 Subsystem=3）
//	      ⇒ 双击必弹控制台，且它会跟着托盘程序一直留着。
//
//	实测对比（PE 头 OptionalHeader.Subsystem 字段）：
//	  漏标志 → 3 = WINDOWS_CONSOLE（双击弹终端）
//	  带标志 → 2 = WINDOWS_GUI（双击无黑窗）
//
//	为什么用"查脚本文本"而不是"查产物"：
//	  · 单测运行时 exe 不一定存在（CI/干净检出都没编过）；
//	  · 子系统是**编译期**属性，行为测试测不到（它决定的是
//	    "有没有控制台"，而不是程序逻辑）。
//	  ⇒ 与相邻的 TestRelaunchNeverPassesAddrFlag 同样选择审查源码/脚本文本。
//	  （产物侧的自检写在 build.ps1 里：构建后直接读 PE 头断言 Subsystem==2。）
//
// ⚠️ 与 gui_windows.go 的设计意图配套：
//
//	本项目的 exe **必须**是 GUI 子系统，同时用"无参数→GUI 模式 /
//	带子命令→命令行模式"的双模式来保住 CLI 用法（文档见
//	gui_windows.go 包注释 L15-25）。
func TestBuildScriptProducesGUISubsystem(t *testing.T) {
	// 测试的工作目录是包目录（internal/app），脚本在仓库根的 scripts/。
	path := filepath.Join("..", "..", "scripts", "build.ps1")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	text := string(src)

	// 去掉注释行，只审可执行代码 ——
	// 本文件的注释里会**大量提到** -H=windowsgui（解释为什么需要），
	// 直接 Contains 会被注释喂饱造成假阳性。
	var codeLines []string
	for _, ln := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		codeLines = append(codeLines, ln)
	}
	code := strings.Join(codeLines, "\n")

	// 🔴 必须**精确定位 go build 那一行**再判断，不能在整个代码里 Contains。
	//
	//	我第一版就是全文 Contains，结果**反向对照时照样全绿** ——
	//	因为本脚本的自检分支里有一句错误提示
	//	  throw "...必须带 -H=windowsgui。"
	//	它是**字符串字面量**（不是注释，剥离注释拦不住它），
	//	于是"漏了标志"的情况下 Contains 仍然为真 ⇒ 护栏形同虚设。
	//	⇒ 又一个"会撒谎的检查器"：只让测试变绿不算数，
	//	  必须证明它在未修复的代码上会红（本次反向对照才暴露出来）。
	var buildLine string
	for _, ln := range codeLines {
		if strings.HasPrefix(strings.TrimSpace(ln), "go build") {
			buildLine = ln
			break
		}
	}
	if buildLine == "" {
		t.Fatal("在 scripts/build.ps1 的可执行代码里找不到 `go build` 行 —— " +
			"构建命令被改写/移走了，本护栏无法验证子系统标志")
	}
	if !strings.Contains(buildLine, "-H=windowsgui") {
		t.Errorf("scripts/build.ps1 的 go build 行缺少 -H=windowsgui —— "+
			"编出来是 WINDOWS_CONSOLE 子系统，**双击会弹黑色终端窗口**"+
			"（2026-10-07 委托人实测踩到，原话\"我是用户要使用又不是调试测试员\"）。\n"+
			"  实际命令行: %s", strings.TrimSpace(buildLine))
	}

	// 反向确认：产物自检必须在（否则漏标志时没人拦得住）。
	//	build.ps1 会读 PE 头的 Subsystem 字段并断言为 2。
	if !strings.Contains(code, "$subsystem") {
		t.Error("scripts/build.ps1 缺少 PE 子系统自检（读 $subsystem 并断言为 2）—— " +
			"没有它，将来漏掉 -H=windowsgui 会一路构建成功并交到用户手里")
	}
}
