//go:build windows

// gui_windows.go：GUI 模式（双击 exe）的入口辅助 —— 弹窗提示、日志文件。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么需要"GUI 模式"
// ═══════════════════════════════════════════════════════════════════
//
//	委托方要求（照 wild-work）：
//	  · 双击 exe 直接能用（不需要 bat、不需要记命令）
//	  · 手动启动时**弹一次提示**告知面板地址
//	  · 开机自启**静默**（不弹窗打扰）
//	  · 有托盘图标（见 tray_windows.go）
//
//	⇒ exe 编译成 **GUI 子系统**（`-H=windowsgui`）后双击**没有黑窗**。
//	  代价是**没有控制台可看日志** ⇒ 必须把日志写进文件
//	  （`data/logs/`），并提供托盘"查看日志"入口。
//
// ⚠️ 子系统是**编译期**属性，一个 exe 只能选一种。为了不破坏命令行用法
//
//	  （`wbapi serve -v`、`wbapi status` 等仍要有输出），采取：
//
//		**同一个 exe 双模式** —— 带子命令时按命令行走（输出到 stdout，
//		GUI 子系统下 stdout 仍可被重定向/管道读取）；**无参数双击**时
//		进 GUI 模式（弹窗 + 托盘 + 日志落文件）。
package app

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	mbOK            = 0x00000000
	mbIconInfo      = 0x00000040
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000
)

// MessageBox 弹出系统模态对话框（阻塞直到用户点确定）。
//
// 只在 GUI 模式（双击启动）使用 —— 命令行模式绝不弹窗，
// 否则 `wbapi serve` 被脚本调用时会卡住等人点确定。
func MessageBox(title, text string, isError bool) {
	flags := uintptr(mbOK | mbSetForeground | mbTopMost)
	if isError {
		flags |= mbIconError
	} else {
		flags |= mbIconInfo
	}
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(title))),
		flags,
	)
}

// openLogFile 打开（追加）日志文件，返回可写的 writer。
//
// 用于 GUI 模式：没有控制台，日志必须落盘，否则出问题完全无从排查。
//
// 路径：`<数据根>/data/logs/wbapi-YYYY-MM-DD.log`（按日切分）
//
// ⚠️ 打开失败时返回 io.Discard 而不是错误：
//
//	日志写不了**不应该**让服务起不来（那会把"能跑"变成"完全不能用"）。
//	但会把错误一并返回，让调用方去提示用户 —— 静默降级等于骗人。
//
// ⚠️ 返回的 writer 若来自文件，**调用方负责关闭**（见 newGUIlogger 的
//
//	cleanup；本函数单独用时也要自己关，否则句柄泄漏）。
func openLogFile(logsDir string) (io.Writer, string, error) {
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return io.Discard, "", fmt.Errorf("创建日志目录失败: %w", err)
	}
	name := "wbapi-" + nowFunc().Format("2006-01-02") + ".log"
	path := filepath.Join(logsDir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return io.Discard, path, fmt.Errorf("打开日志文件失败: %w", err)
	}
	return f, path, nil
}

// syncWriter 在每次写入后立即 fsync。
//
// 🔴 为什么必须这样做（实测踩到）：
//
//	直接写 `*os.File` 时，数据留在**系统文件缓存**里，
//	外部程序（记事本 / PowerShell `Get-Content`）**读不到**，
//	直到进程退出或缓冲被刷出。
//
//	实测现象：服务跑了 30 秒，日志文件一直是 **0 字节**；
//	**杀掉进程后**同样的文件立刻变成 699 字节、内容完整。
//
//	两个后果都很糟：
//	  ① 排查时**看不到实时日志** —— 而 GUI 模式没有控制台，
//	     日志是**唯一**的排查手段（功能等于废掉）
//	  ② 我的"探测日志是否可写"逻辑会**误判**：
//	     写入成功但文件仍 0 字节 ⇒ 弹出"无法写入日志"的错误告警框
//	     （用户看到的是假故障，比没有告警更糟）
//
//	⚠️ 代价：每条日志一次 fsync，比纯缓冲慢。
//	  但本程序日志量极低（启动几行 + 请求级零星几行），
//	  换来"日志实时可见 + 探测不误判"，完全值得。
type syncWriter struct{ f *os.File }

func (s syncWriter) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	if err != nil {
		return n, err
	}
	// 立即落盘：失败不改变写入结果（数据已在缓存里，只是没能强制刷出）
	_ = s.f.Sync()
	return n, nil
}

// newGUIlogger 构造 GUI 模式的 logger：**主写日志文件，且每条立即落盘**。
//
// 🔴 返回的 cleanup 必须被调用（通常 defer）：它负责收尾 Sync + Close。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 三个我实测踩到的坑（都已修，勿改回）
// ═══════════════════════════════════════════════════════════════════
//
//  1. **绝不能把 os.Stderr 放进 io.MultiWriter 与文件并列**。
//
//     GUI 子系统（`-H=windowsgui`）下进程**没有 stderr 句柄**：
//     实测 `os.Stderr.Write(nil)` 返回
//     `write /dev/stderr: The handle is invalid`。
//     而 `io.MultiWriter` 的语义是**遇到第一个错误就停止** ——
//     若 stderr 在前，**文件永远拿不到内容**。
//
//     ⇒ 文件是**主目标**且**必须排在前面**；stderr 只在真可用时才附加
//     （用 stderrUsable 探测，不假定）。
//
//  2. **必须每条立即 fsync**（见 syncWriter）：否则实时看不到，
//     且"探测是否可写"会误判成失败并弹假告警。
//
//  3. **必须返回 cleanup 关文件**：否则句柄泄漏，且 Windows 上
//     "文件被占用"会让用户无法用编辑器打开日志查看 ——
//     而那恰恰是 GUI 模式唯一的排查手段。
func newGUIlogger(path string) (*log.Logger, func()) {
	if path == "" {
		return log.New(os.Stderr, "wbapi ", log.LstdFlags|log.Lmsgprefix), func() {}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// 文件打不开 ⇒ 退回 stderr（GUI 下可能同样不可用，但至少不崩）
		return log.New(os.Stderr, "wbapi ", log.LstdFlags|log.Lmsgprefix), func() {}
	}

	var w io.Writer = syncWriter{f: f}
	if stderrUsable() {
		// ⚠️ 文件在前：即使 stderr 写失败，文件也已经写入了
		w = io.MultiWriter(w, os.Stderr)
	}

	cleanup := func() {
		_ = f.Sync()
		_ = f.Close()
	}
	return log.New(w, "wbapi ", log.LstdFlags|log.Lmsgprefix), cleanup
}

// stderrUsable 探测 stderr 是否真的可写。
//
// 🔴 不能假定"GUI 子系统下 stderr 一定不可用"：
//
//	从命令行（`wbapi --silent`）启动时 stderr 是好的，日志双写更方便排查；
//	双击启动时 stderr 是无效句柄。所以**探测而不假定**。
//
// 探测方式：往 stderr 写 0 字节 —— 不产生任何输出，但能暴露
// "句柄无效"这一类错误（Windows 上对无效句柄的写会直接失败）。
func stderrUsable() bool {
	if os.Stderr == nil {
		return false
	}
	_, err := os.Stderr.Write(nil)
	return err == nil
}

// hideConsoleWindow 尝试隐藏本进程附带的控制台窗口。
//
// ⚠️ 只有在 exe 以**控制台子系统**构建、却又被双击时才有控制台可隐藏。
//
//	若 exe 以 GUI 子系统构建（本项目目标），本函数是空操作 —— 保留它是
//	为了"万一子系统没配上"时体验不至于太糟（黑窗会自己消失）。
//
// 用 GetConsoleWindow + ShowWindow(SW_HIDE)。
func hideConsoleWindow() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return // GUI 子系统下正常走到这里
	}
	const swHide = 0
	procShowWindow.Call(hwnd, swHide)
}

var (
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
)

// 让 syscall 包被引用（utf16Ptr 在同文件被 MessageBox 用到，
// 但显式保留 import 以防未来重构时漏掉）
var _ = syscall.UTF16PtrFromString
