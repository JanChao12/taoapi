//go:build windows

package app

import "runtime"

// lockOSThread 把当前 goroutine 锁定到当前 OS 线程，返回解锁函数。
//
// 🔴 为什么托盘必须锁线程（Windows 特有约束）：
//
//	`Shell_NotifyIcon` 要求**创建托盘图标的那个线程**自己持续泵消息
//	（GetMessage/DispatchMessage）。若消息循环所在的 goroutine 被 Go
//	调度到别的线程，消息就投递到"没有循环的线程"，托盘立刻失去响应
//	（表现为菜单弹不出来、左键无反应）。
//
//	`runtime.LockOSThread` 保证这个 goroutine 始终在同一个线程上 ——
//	这是 Go 里做 Win32 消息循环的标准做法。
//
// ⚠️ 解锁必须成对：线程锁定的 goroutine 退出时若没解锁，
//
//	该线程会被"浪费"（Go 会另开线程），长期运行会累积。
func lockOSThread() func() {
	runtime.LockOSThread()
	return runtime.UnlockOSThread
}
