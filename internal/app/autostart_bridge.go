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
	// 开机时要自动起服务，所以固定带 serve 子命令。
	// 注意 autostart.DefaultArgs 也是 serve，但那是给测试用的默认值；
	// 这里显式传入是本层的决定，不依赖那个默认值。
	return autostart.Enable(exe, []string{"serve"})
}
