//go:build !windows

package autostart

import (
	"errors"
	"testing"
)

// 非 Windows 平台的行为测试。
//
// 本机是 Windows，这些用例平时不会跑（需要交叉编译的 CI 或 GOOS=linux 环境）。
// 仍然写出来的理由：这是"显式失败而不是静默成功"这条设计的【可执行契约】。
// 没有它，将来有人为了"让 linux 上也能编译通过/看起来能用"而把
// autostart_other.go 改成返回 nil，不会有任何测试拦住 ——
// 而那正是 dpapi_other.go 明确否决过的做法。

// TestSupportedFalseOnNonWindows 断言非 Windows 报告"不支持"。
func TestSupportedFalseOnNonWindows(t *testing.T) {
	if Supported() {
		t.Fatal("非 Windows 平台 Supported() 必须为 false")
	}
}

// TestOperationsFailExplicitly 断言所有写操作都明确失败。
//
// 关键：必须是 ErrUnsupported 本身（errors.Is 可判定），
// 而不是随便一个 error —— 调用方要据此区分"平台不支持"与"操作失败"。
func TestOperationsFailExplicitly(t *testing.T) {
	t.Run("Enable", func(t *testing.T) {
		err := Enable("/usr/local/bin/wbapi", []string{"serve"})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Enable 应返回 ErrUnsupported，实际: %v", err)
		}
	})

	t.Run("Disable", func(t *testing.T) {
		// 🔴 这里【故意】不接受 nil：返回 nil 会让调用方的
		// `if err := Disable(); err != nil` 分支以为清理成功了。
		err := Disable()
		if err == nil {
			t.Fatal("Disable 在非 Windows 上绝不能返回 nil（那是静默成功）")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Disable 应返回 ErrUnsupported，实际: %v", err)
		}
	})

	t.Run("Enabled", func(t *testing.T) {
		on, err := Enabled()
		if on {
			t.Fatal("Enabled 在非 Windows 上不应返回 true")
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Enabled 应返回 ErrUnsupported，实际: %v", err)
		}
	})

	t.Run("CommandLineFromRegistry", func(t *testing.T) {
		if _, err := CommandLineFromRegistry(); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("CommandLineFromRegistry 应返回 ErrUnsupported，实际: %v", err)
		}
	})
}
