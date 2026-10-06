//go:build windows

package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 本文件守住跨进程文件锁真的"跨进程"。
//
// 🔴 为什么必须用【真实子进程】测：
//
//	同一进程内两次 AcquireFileLock 会不会互相阻塞，取决于 Windows
//	对同一进程重复加锁的语义（同进程重复加锁通常**会成功**）——
//	所以进程内测试根本证明不了"另一个 wbapi 进程拿不到锁"。
//	要验证的是跨进程语义，就必须起真进程。
//
// 这条测试的存在理由（Codex 第 11、12 轮）：重启期间可能短暂出现
// 父子两个 wbapi 同时写同一个 JSONL。锁必须真的挡住这种并发。

const lockHelperEnv = "WBAPI_LOCK_HELPER"

// TestFileLockIsCrossProcess 验证：父进程持锁时，子进程拿不到。
func TestFileLockIsCrossProcess(t *testing.T) {
	// 子进程模式：尝试拿锁并把结果打到 stdout
	if os.Getenv(lockHelperEnv) == "1" {
		runLockHelper()
		return
	}

	dir := testDir(t)
	lockPath := filepath.Join(dir, "test.jsonl.lock")

	// 父进程先拿锁并一直持有
	parent, err := AcquireFileLock(lockPath, time.Second)
	if err != nil {
		t.Fatalf("父进程应能拿到锁: %v", err)
	}
	defer parent.Release()

	// 起子进程，让它尝试拿同一把锁（短超时，避免测试卡住）
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestFileLockIsCrossProcess")
	cmd.Env = append(os.Environ(),
		lockHelperEnv+"=1",
		"WBAPI_LOCK_PATH="+lockPath,
	)
	out, _ := cmd.CombinedOutput()

	got := string(out)
	if !containsAny(got, "LOCK_FAILED") {
		t.Fatalf("父进程持锁时子进程竟然拿到了锁！子进程输出: %s", got)
	}
	t.Logf("子进程正确报告拿不到锁: %s", firstLine(got))

	// 父进程释放后，子进程应能拿到
	parent.Release()

	cmd2 := exec.Command(exe, "-test.run=TestFileLockIsCrossProcess")
	cmd2.Env = append(os.Environ(),
		lockHelperEnv+"=1",
		"WBAPI_LOCK_PATH="+lockPath,
	)
	out2, _ := cmd2.CombinedOutput()
	if !containsAny(string(out2), "LOCK_OK") {
		t.Fatalf("父进程释放后子进程仍拿不到锁！输出: %s", out2)
	}
}

// runLockHelper 是子进程侧的逻辑：尝试拿锁并报告结果。
func runLockHelper() {
	path := os.Getenv("WBAPI_LOCK_PATH")
	if path == "" {
		os.Stdout.WriteString("LOCK_FAILED: no path\n")
		os.Exit(0)
	}
	lk, err := AcquireFileLock(path, 300*time.Millisecond)
	if err != nil {
		os.Stdout.WriteString("LOCK_FAILED: " + err.Error() + "\n")
		os.Exit(0)
	}
	lk.Release()
	os.Stdout.WriteString("LOCK_OK\n")
	os.Exit(0)
}

// containsAny 判断输出里是否含某标记。
func containsAny(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

// indexOf 是 strings.Index 的最小实现（避免为一个函数引入 strings）。
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// firstLine 取第一行，便于日志可读。
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// TestAcquireFileLockTimesOut 守：锁被占时按超时返回错误，不永久卡住。
func TestAcquireFileLockTimesOut(t *testing.T) {
	dir := testDir(t)
	lockPath := filepath.Join(dir, "x.lock")

	held, err := AcquireFileLock(lockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	start := time.Now()
	// 注意：同一进程重复加锁在 Windows 上通常**会成功**，
	// 所以这里不强求失败，只验证"不会永久卡住"。
	_ = timeoutProbe(lockPath, 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("获取锁耗时 %v，超时机制失效（会卡住请求）", elapsed)
	}
}

// timeoutProbe 尝试拿锁，返回是否成功（忽略错误）。
func timeoutProbe(path string, timeout time.Duration) bool {
	lk, err := AcquireFileLock(path, timeout)
	if err != nil {
		return false
	}
	lk.Release()
	return true
}
