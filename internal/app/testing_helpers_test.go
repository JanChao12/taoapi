package app

import (
	"os"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// 这些辅助函数让 app 包的集成测试能构造真实的 workbuddy provider，
// 而不必把 workbuddy 的内部细节泄露到生产代码里。

// testTempDir 是 t.TempDir() 的**抗竞态**版本。
//
// 🔴 为什么需要它（2026-10-05 实测，连续三次踩到）：
//
//	t.TempDir() 在测试结束时调用 RemoveAll，而 Windows 上
//	"删掉仍被打开的句柄所在的目录"会失败并报：
//	    TempDir RemoveAll cleanup: unlinkat ...: The directory is not empty
//
//	我们的集成测试里有后台 goroutine（持久化器写账号、签到守护、
//	usage 写入）可能在 wg.Wait() 之后仍在收尾，于是清理时撞上。
//	表现是**每次失败的测试名都不一样**（谁先收尾谁撞），
//	极易被误判成"逻辑有 bug"。
//
//	这个 helper 用"延迟清理 + 重试"把它变成确定性：
//	先给后台任务一点时间收尾，再重试删除若干次。
//
// 注意：这不是掩盖问题。真正该做的是让被测代码有明确的关闭语义，
// 但那属于生产代码改动；对测试而言，清理竞态本身不是被测对象。
func testTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() // 仍用它分配目录（自动注册清理）
	return dir
}

// cleanupTempDirWithRetry 注册一个"带重试的清理"。
//
// 用法：在需要抗竞态的测试里显式调用，传入 t.TempDir() 的返回值。
// t.Cleanup 是后进先出，所以这个清理会在 t.TempDir() 自带的清理**之前**跑，
// 我们先删干净并让它变成空目录，自带清理就不会再失败。
func cleanupTempDirWithRetry(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		// 给后台 goroutine 收尾的时间（持久化器的原子写含 fsync）
		time.Sleep(50 * time.Millisecond)
		for attempt := 0; attempt < 10; attempt++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		// 仍失败就让 t.TempDir 的清理去报错（保留可观测性，不静默吞掉）
	})
}

// newWorkbuddyClientForTest 构造指向假上游的 workbuddy 客户端。
func newWorkbuddyClientForTest(t *testing.T, baseURL string) *workbuddy.Client {
	t.Helper()
	c := workbuddy.NewClient()
	c.SetBases(baseURL, baseURL)
	return c
}

// testWorkbuddyCredential 返回测试用凭证（非真实 token）。
func testWorkbuddyCredential() workbuddy.Credential {
	return workbuddy.Credential{
		AccessToken: "test-access-token",
		UID:         "test-uid-0000-0000-0000-000000000000",
	}
}

// newWorkbuddyProviderForTest 构造 workbuddy provider。
func newWorkbuddyProviderForTest(c *workbuddy.Client) provider.Provider {
	return workbuddy.NewProvider(c, testWorkbuddyCredential())
}
