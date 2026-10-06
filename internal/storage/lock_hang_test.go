//go:build windows

package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLockDoesNotHangOnSameProcessReentry 守：同进程内重复获取同一把锁
// 【不会永久卡住】。
//
// 🔴 为什么这条重要（2026-10-05 排查一次疑似死锁时写的）：
//
//	一次全量测试中出现过 `app.test` 卡住 856 秒、CPU 为 0 的现象。
//	虽然最终 8 次重跑都没复现（判断是外部进程干扰），但**必须排除**
//	一种真实可能：Windows 的文件锁在同一进程内**是可重入的**，
//	而"可重入"在不同实现下可能表现为"第二次也成功"或"永久阻塞"。
//	若我们的用法会导致永久阻塞，那是个必须修的死锁。
//
//	本测试确保：无论 Windows 的语义是哪种，**都不会永久卡住** ——
//	因为 AcquireFileLock 用的是"非阻塞尝试 + 睡眠重试 + 超时"，
//	而不是阻塞式的 LockFileEx 调用。
func TestLockDoesNotHangOnSameProcessReentry(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "reentry.lock")

	first, err := AcquireFileLock(path, time.Second)
	if err != nil {
		t.Fatalf("第一次获取应成功: %v", err)
	}
	defer first.Release()

	// 同进程内再获取一次：无论成功还是超时，都必须在有限时间内返回
	done := make(chan error, 1)
	go func() {
		lk, err := AcquireFileLock(path, 300*time.Millisecond)
		if lk != nil {
			lk.Release()
		}
		done <- err
	}()

	select {
	case err := <-done:
		// 两种结果都可接受（取决于 Windows 的同进程重入语义），
		// 关键是【返回了】
		if err != nil {
			t.Logf("同进程重入被拒（超时）: %v —— 这是可接受的语义", err)
		} else {
			t.Log("同进程重入成功 —— 也可接受，关键是没卡住")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("同进程重入获取锁卡死超过 5 秒 —— AcquireFileLock 存在死锁风险")
	}
}

// TestLockTimeoutBoundsWait 守：超时是硬上限，等待不会被无限拖长。
//
// 用同进程重入来制造"锁被持有"的场景（无论 Windows 判不判它成功），
// 只断言"调用能在有限时间内返回"这一条 —— 这正是我们依赖的契约。
func TestLockTimeoutBoundsWait(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "timeout.lock")

	held, err := AcquireFileLock(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	start := time.Now()
	_, _ = AcquireFileLock(path, 500*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("等待 %v，远超设定的 500ms —— 超时机制不可靠", elapsed)
	}
}
