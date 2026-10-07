package app

import (
	"log"
	"os"
	"path/filepath"
	"syscall"

	"workbuddy.local/workbuddy-api/internal/datadir"
)

// gui.go：GUI 模式（双击 exe）的编排。
//
// ═══════════════════════════════════════════════════════════════════
// GUI 模式做什么（委托方要求，照 wild-work）
// ═══════════════════════════════════════════════════════════════════
//
//	双击 exe（无参数）时：
//	  ① 隐藏可能存在的控制台窗口（若以控制台子系统构建）
//	  ② 起服务（与 `serve` 完全同一套逻辑，不另写一份）
//	  ③ **手动启动弹一次提示**告知面板地址；`--silent`（开机自启）不弹
//	  ④ 挂托盘图标：左键 = 打开主界面，右键 = {主界面 / 日志 / 退出}
//	  ⑤ 日志写文件（GUI 子系统没有控制台可看）
//
// 🔴 关键设计：**不复制 serve 的逻辑**，而是给 runServe 传
//
//	一个"GUI 运行时"开关。理由：serve 里有端口解析、重启交接、
//	drain 超时、后台任务回收等一大堆已验证的逻辑，复制一份必然分叉。
//
// ⚠️ 提示框在**服务真正起来之后**才弹（不是启动前）：
//
//	否则用户点了"确定"却发现面板还打不开（服务还在初始化），
//	体验反而更差。所以提示由 serve 在 Listen 成功后触发（见 serve.go）。
type guiRuntime struct {
	// enabled 是否为 GUI 模式（决定是否挂托盘、弹提示）。
	enabled bool

	// silent 是否静默（开机自启）：不弹提示框。
	silent bool

	// logger 已转为写文件（GUI 子系统无控制台）。
	logger *log.Logger

	// logsDir 供托盘"查看日志"打开。
	logsDir string
}

// sigQuitFromTray 是托盘"退出"投递的**合成信号**。
//
// 🔴 为什么用合成信号而不是直接调 srv.Shutdown：
//
//	优雅关闭要处理的事不止 Shutdown —— 还有"重启中不能关"、
//	"drain 在途请求"、"回收后台任务"等。那些逻辑都在 serve 的
//	select 分支里，且已被测试覆盖。
//	⇒ 让托盘复用**同一条路径**（投一个信号进去），而不是另写一套关闭流程。
//	  另写必然会漏掉某些清理，且两边随后会分叉。
//
// ⚠️ 用 syscall.Signal 的非法值（-1）当哨兵：它不可能与真实信号冲突，
//
//	日志里会显示成 "signal -1"，一眼能看出是托盘触发的。
const sigQuitFromTray = syscall.Signal(-1)

// runGUI 是 GUI 模式的入口（双击 exe / `wbapi --silent`）。
func runGUI(silent bool) int {
	// 双击时若还有控制台窗口，藏掉（GUI 子系统下这里是空操作）
	hideConsoleWindow()

	// 日志落文件：GUI 子系统没有控制台，不落盘就完全无从排查。
	//
	// ⚠️ 只调 newGUIlogger（它自己打开文件并返回 cleanup）。
	//	我第一版先调 openLogFile 再调 newGUIlogger ⇒ **同一文件被打开两次**：
	//	  ① openLogFile 的句柄没人关（泄漏）
	//	  ② 测试里 TempDir 删不掉（"being used by another process"）
	//	重复实现已收敛成一处。
	logPath := filepath.Join(datadir.LogsDir(),
		"wbapi-"+nowFunc().Format("2006-01-02")+".log")
	logger, closeLog := newGUIlogger(logPath)
	defer closeLog()

	rt := &guiRuntime{
		enabled: true,
		silent:  silent,
		logger:  logger,
		logsDir: datadir.LogsDir(),
	}

	// 写一行并**回读文件大小**，确认日志真的落盘了。
	//
	// 🔴 不能只看 OpenFile 没报错：文件可能被创建却写不进去
	//	（磁盘满、权限、被独占）。实测踩过"文件 0 字节却以为日志正常"——
	//	那比完全没有日志更危险，因为它让人以为有据可查。
	logger.Printf("TAOAPI 启动（GUI 模式，silent=%v）", silent)
	if fi, err := os.Stat(logPath); err != nil || fi.Size() == 0 {
		reason := "写入后文件仍为空（可能被独占或磁盘不可写）"
		if err != nil {
			reason = err.Error()
		}
		if !silent {
			MessageBox("TAOAPI 提示",
				"无法写入日志文件：\n"+reason+
					"\n\n服务会照常启动，但出问题时将没有日志可查。\n"+
					"日志目录：\n"+datadir.LogsDir(),
				true)
		}
	}

	return runServeMode(nil, rt)
}
