package app

import (
	"os"

	"workbuddy.local/workbuddy-api/internal/autostart"
)

// 本文件把 internal/autostart 接到设置 API 上。
//
// 为什么要这一层薄封装：settings.go 只关心"设置开机自启成功/失败"，
// 不该关心"本程序的可执行文件在哪、要不要带 serve 参数"，
// 也不该关心"本平台支不支持"。把平台与命令细节收在这里，
// 将来换机制（例如改计划任务）只动这一处。
//
// 🔴 不要在本层再写一份平台判断：
// 起初本文件用过 //go:build 常量（那时 autostart 还没有 Supported()），
// 后来 autostart 包补上了 Supported()，两处判断就成了重复的真值来源，
// 迟早会不一致。现在统一以 autostart.Supported() 为准，
// build tag 常量已删除（2026-10-05）。

// autoStartSupported 报告本平台是否支持开机自启。
//
// 面板据此决定开关是否可点（不支持时显示为禁用，
// 而不是让用户点了才报错）。
func autoStartSupported() bool {
	return autostart.Supported()
}

// setAutoStart 打开/关闭开机自启。
//
// exePath 用 os.Executable() 取【当前进程】的真实路径而不是 os.Args[0]：
// 后者可能是相对路径或被改写过的形式，写进注册表后开机时不一定能解析。
func setAutoStart(enable bool) error {
	if !enable {
		return autostart.Disable()
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 🔴 必须带 --silent（用 autostart.DefaultArgs，不要在这里另写一份）。
	//
	// ═══════════════════════════════════════════════════════════════
	// 2026-10-08 委托人实测缺陷："我开机没有自启"
	// ═══════════════════════════════════════════════════════════════
	//
	//	本行原先写的是 `autostart.Enable(exe, []string{"serve"})` ——
	//	**漏了 --silent**。后果与重启子进程那个缺陷同源
	//	（serve.go L154 `if rt == nil && *silent`）：
	//
	//	  rt 保持 nil ⇒ 不挂托盘、**日志只写 stderr**
	//	  ⇒ GUI 子系统（-H=windowsgui）**没有控制台** ⇒ 日志静默丢失。
	//
	//	实测证据：自启进程（pid 928，监听 4567）跑起来了，
	//	  但它的日志文件最后一行停在**上一次启动**，本次开机零记录 ——
	//	  排查时看不到任何痕迹，于是委托人只能判断成"没自启"。
	//
	//	为什么测试没抓到：`TestAutostartArgsAreSilent` 断言的是
	//	  **autostart.DefaultArgs**（那个是对的），
	//	  而本函数**自己另写了一份参数**，于是成了测试的盲区。
	//	⇒ 现在改为直接复用 DefaultArgs，并加 `TestSetAutoStartUsesDefaultArgs` 钉住。
	//
	// 🔴 另外：测试二进制**绝不写注册表**。
	//
	//	`go test` 下 os.Executable() 是临时路径（...\Temp\go-build...\pkg.test.exe）。
	//	若测试间接调到本函数，会把用户真实的自启项改成**测试结束后即消失**的
	//	临时文件 —— 而 Run 键失效是静默的，用户下次开机毫无线索。
	//	（2026-10-08 在 Heal 路径上**实测踩到过**，详见 autostart.IsTestBinary。）
	if autostart.IsTestBinary(exe) {
		return nil
	}
	return autostart.Enable(exe, autostart.DefaultArgs)
}
