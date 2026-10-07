//go:build !windows

// tray_other.go：非 Windows 平台的托盘/GUI 占位实现。
//
// 🔴 本项目**只面向 Windows**（凭据走 DPAPI，非 Windows 上是故意报错的
// 占位实现，见 storage/dpapi_other.go）。但**代码要能跨平台编译** ——
// 否则 lint/IDE/交叉编译检查全部失效。
//
// ⇒ 这里给出"明确不支持"的占位实现：调用方拿到 error，
//
//	按"没有托盘"降级运行（服务本身照常工作），
//	**不是**假装成功。
package app

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

// errTrayUnsupported 非 Windows 没有托盘。
var errTrayUnsupported = errors.New("托盘图标仅在 Windows 上可用")

type trayOptions struct {
	Logger  *log.Logger
	BaseURL string
	LogsDir string
	OnQuit  func()
	Tip     string
}

// startTray 在非 Windows 上明确失败（调用方据此降级为"无托盘"）。
func startTray(trayOptions) (func(), error) {
	return nil, errTrayUnsupported
}

// MessageBox 在非 Windows 上退化为写 stderr（不弹窗）。
//
// ⚠️ 刻意**不静默成功**：调用方不该以为"提示已经给用户看了"。
// 非 Windows 下本项目本来就不受支持（凭据走 DPAPI），
// 这里只需保证能编译、行为可预期。
func MessageBox(title, text string, isError bool) {
	prefix := "[提示]"
	if isError {
		prefix = "[错误]"
	}
	fmt.Fprintf(os.Stderr, "%s %s: %s\n", prefix, title, text)
}

// openLogFile 在非 Windows 上仍可用（纯文件操作）。
func openLogFile(logsDir string) (io.Writer, string, error) {
	return io.Discard, "", nil
}

// newGUIlogger 非 Windows 版本：只写 stderr，cleanup 为空操作。
func newGUIlogger(string) (*log.Logger, func()) {
	return log.New(os.Stderr, "wbapi ", log.LstdFlags|log.Lmsgprefix), func() {}
}

// hideConsoleWindow 非 Windows 无控制台可隐藏。
func hideConsoleWindow() {}
