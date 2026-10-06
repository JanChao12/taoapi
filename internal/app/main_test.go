package app

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

// testConfigDir 返回一个供测试写配置文件的目录。
//
// 与 t.TempDir() 的区别：**不注册 Go 的按测试清理**。
// 目录统一由 testArtifactRoot 在整包结束时带重试删除。
//
// 🔴 为什么不用 t.TempDir()（2026-10-05 实测，约 1/10 概率踩到）：
//
//	本包大量测试会起 httptest 服务并让它们读写配置文件。
//	t.TempDir() 的清理是**逐测试立即执行**的，而服务端刚关闭时
//	连接/句柄可能尚未完全释放，于是 RemoveAll 失败并报：
//
//	    TempDir RemoveAll cleanup: unlinkat ...\001:
//	        The directory is not empty.
//
//	**要命的是表现**：每次失败的测试名都不一样（谁先收尾谁撞），
//	而单独跑那个测试总是通过 —— 很容易被当成真实 regression 去"修"代码。
//
//	改为"整包结束后统一重试清理"，把随机性收敛成一次可容忍的收尾。
func testConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(testArtifactRoot(), uniqueName(t))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建测试配置目录失败: %v", err)
	}
	return dir
}

// testArtifactRoot 返回整包共用的测试产物根目录。
var (
	artifactRootOnce sync.Once
	artifactRootPath string
)

func testArtifactRoot() string {
	artifactRootOnce.Do(func() {
		d, err := os.MkdirTemp("", "wbapi-apptest-")
		if err != nil {
			d = filepath.Join(os.TempDir(), "wbapi-apptest-fallback")
			_ = os.MkdirAll(d, 0o700)
		}
		artifactRootPath = d
	})
	return artifactRootPath
}

// uniqueName 生成一个测试内唯一的目录名。
//
// 用测试名 + 计数器：名字里带测试名便于残留时排查是谁留下的。
func uniqueName(t *testing.T) string {
	var n atomic.Int64
	return fmt.Sprintf("%s-%d-%d", sanitizeName(t.Name()), os.Getpid(), n.Add(1))
}

// sanitizeName 把测试名里的路径分隔符换成下划线（子测试名含 "/"）。
func sanitizeName(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(s)
}

// TestMain 统一收口本包测试的临时产物清理。
//
// 见 testConfigDir 的说明：不逐测试清理，改为整包结束后带重试删除。
func TestMain(m *testing.M) {
	code := m.Run()

	// 给残留的后台 goroutine 时间交回文件句柄
	time.Sleep(300 * time.Millisecond)

	root := artifactRootPath
	if root == "" {
		os.Exit(code)
	}
	// 带重试删除（容忍句柄延迟释放）
	for attempt := 0; attempt < 20; attempt++ {
		if err := os.RemoveAll(root); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 删不掉也不改变退出码：残留临时目录不影响测试结论
	os.Exit(code)
}
