package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════
// 本包测试的临时目录清理（根治 Windows 上的逐测试清理竞态）
// ═══════════════════════════════════════════════════════════════════
//
// 🔴 背景（Codex 第 21 轮要求"消除而不是记录"）：
//
//	本包测试会反复并发写同一个文件、并起真实子进程测跨进程锁。
//	`t.TempDir()` 的清理是**逐测试立即执行**的，而刚写完/刚关闭的
//	文件句柄在 Windows 上可能尚未释放，于是 `RemoveAll` 失败：
//
//	    TempDir RemoveAll cleanup: unlinkat ...\001:
//	        The directory is not empty.
//
//	**要命的是表现**：失败的测试名每次都不同（谁先收尾谁撞），
//	单独重跑又总是通过 —— 极易被当成真实 regression 去"修"生产代码。
//
//	这个模式在 `internal/app` 已经踩过并根治（那边用 testConfigDir +
//	TestMain 整包重试清理，连跑 12 次全绿）。本包照同样的做法收敛：
//	**不逐测试清理**，改为整包结束后带重试删除。
//
// 与 internal/app 的实现保持一致的语义：
//   - 每个测试拿到一个独立子目录（互不干扰）
//   - 目录建在同一个整包共享的根下，收尾时**一次**带重试删除
//   - 删不掉也**不改变退出码**（残留临时目录不影响测试结论）

// testDir 返回一个供测试使用的独立目录。
//
// 用它替代 `t.TempDir()`：区别是**不注册 Go 的按测试清理**，
// 从而不会在句柄未释放时随机报 "directory is not empty"。
func testDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testArtifactRoot(), uniqueDirName(t))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建测试目录失败: %v", err)
	}
	return dir
}

var (
	storageRootOnce sync.Once
	storageRootPath string
)

// testArtifactRoot 返回整包共用的测试产物根目录。
func testArtifactRoot() string {
	storageRootOnce.Do(func() {
		d, err := os.MkdirTemp("", "wbapi-storagetest-")
		if err != nil {
			d = filepath.Join(os.TempDir(), "wbapi-storagetest-fallback")
			_ = os.MkdirAll(d, 0o700)
		}
		storageRootPath = d
	})
	return storageRootPath
}

// uniqueDirName 生成测试内唯一的目录名（带测试名便于残留时排查）。
func uniqueDirName(t *testing.T) string {
	var n atomic.Int64
	name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(t.Name())
	return fmt.Sprintf("%s-%d-%d", name, os.Getpid(), n.Add(1))
}

// TestMain 统一收口本包测试的临时产物清理（整包结束后带重试）。
func TestMain(m *testing.M) {
	code := m.Run()

	// 给残留的后台 goroutine / 子进程交回文件句柄
	time.Sleep(300 * time.Millisecond)

	root := storageRootPath
	if root == "" {
		os.Exit(code)
	}
	for attempt := 0; attempt < 20; attempt++ {
		if err := os.RemoveAll(root); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(code)
}
