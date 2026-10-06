package config

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

// TestMain 统一收口本包测试的临时目录清理。
//
// 🔴 与 internal/app 里的同一问题（2026-10-05 实测，两个包都踩到）：
//
//	`t.TempDir()` 在每个测试结束时调用 RemoveAll。Windows 上
//	"删除仍被打开的句柄所在的目录"会失败并报：
//
//	    TempDir RemoveAll cleanup: unlinkat ...\001:
//	        The directory is not empty.
//
//	本包的 `TestConcurrentMutateAndGet` 会起 16 个 goroutine 反复
//	原子写配置文件（临时文件 + rename），收尾时若还有句柄没交回内核，
//	清理就失败 —— 实测约 2/10。
//
//	**最坑的是表现**：它挂在"并发"这个测试上，看起来像并发逻辑有 bug，
//	而实际上被测逻辑没问题、断言也没失败，报错来自测试框架的清理阶段。
//
// 做法：测试产物放到本包自己管理的目录，整包结束后带重试删除。
// 这不是掩盖问题 —— 清理竞态不是被测对象；真正要保证的是
// Mutate 的原子性与并发安全，那由测试自身的断言负责。
func TestMain(m *testing.M) {
	code := m.Run()

	// 给残留 goroutine 时间把文件句柄交回内核
	time.Sleep(300 * time.Millisecond)

	root := artifactRootPath
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

// testConfigDir 返回一个供测试写配置文件的目录。
//
// 与 t.TempDir() 的区别：不注册 Go 的按测试清理 ——
// 目录由 artifactRoot 在整包结束时带重试删除。
func testConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testArtifactRoot(), uniqueName(t))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建测试目录失败: %v", err)
	}
	return dir
}

// artifactRootPath 是本包测试产物的根目录（惰性创建一次）。
var (
	artifactRootOnce sync.Once
	artifactRootPath string
	nameSeq          atomic.Int64
)

func testArtifactRoot() string {
	artifactRootOnce.Do(func() {
		d, err := os.MkdirTemp("", "wbapi-cfgtest-")
		if err != nil {
			d = filepath.Join(os.TempDir(), "wbapi-cfgtest-fallback")
			_ = os.MkdirAll(d, 0o700)
		}
		artifactRootPath = d
	})
	return artifactRootPath
}

// uniqueName 生成测试内唯一的目录名（含测试名便于残留排查）。
func uniqueName(t *testing.T) string {
	name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(t.Name())
	return fmt.Sprintf("%s-%d-%d", name, os.Getpid(), nameSeq.Add(1))
}
