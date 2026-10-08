package login

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════
// 「登录后保留窗口」的护栏（2026-10-08 委托人需求 2）
// ═══════════════════════════════════════════════════════════════════
//
//	委托人原话：「我每次登录后还没看到是否登录成功的反馈，
//	            你就直接把窗口关了」
//
//	⇒ 登录结束（成功/失败/取消）后**保留浏览器窗口**，
//	  只 Detach（停止监听 + 释放互斥位），不 kill 进程、不立刻删 profile。
//
// 🔴 这一改动的风险方向与"自愈"那次相反：
//
//	那次是我**多做了**（越权写注册表）；这次要防的是**少做了** ——
//	Detach 若不释放互斥位，下次登录会被 ErrLoginBusy 永久挡住，
//	表现为"第一次登录能用，之后再也点不动"，且没有任何错误提示。
//
// 所以下面重点守三件事：
//  1. Detach 不杀进程（窗口真的保留）
//  2. Detach 释放互斥位（下次还能登录）
//  3. Detach 可重复调用（幂等，不 panic、不重复删）

// fakeSession 造一个"有 cmd 但没有真浏览器"的会话，用于单测 Detach 语义。
//
// 用一个**真实存在但立即退出的**命令（cmd /c exit）当替身：
// 这样 cmd.Wait() 会正常返回，Detach 挂的清理 goroutine 能跑完，
// 我们就能断言 profile 最终被删掉。
func fakeSession(t *testing.T, profile string) *BrowserSession {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动替身进程失败: %v", err)
	}
	return &BrowserSession{
		kind:    BrowserChrome,
		exePath: "fake",
		profile: profile,
		port:    0,
		cmd:     cmd,
		closed:  make(chan struct{}),
	}
}

// TestDetachDoesNotKillProcess 守：Detach 必须**保留窗口**（不杀进程）。
//
// 这是需求 2 的核心 —— 若这里失败，说明改回了"用完即关"，
// 用户仍然看不到登录结果。
func TestDetachDoesNotKillProcess(t *testing.T) {
	// 用一个会活一会儿的进程，确认 Detach 后它仍在跑。
	cmd := exec.Command("cmd", "/c", "ping", "-n", "6", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动替身进程失败: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	s := &BrowserSession{
		kind:    BrowserChrome,
		profile: filepath.Join(os.TempDir(), "wbapi-login-detachtest-"+t.Name()),
		cmd:     cmd,
		closed:  make(chan struct{}),
	}

	s.Detach()

	// 进程必须还活着（Detach 不 kill）。
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		// Windows 上 Signal 常返回不支持；用 FindProcess 是否还在来判定。
		_ = err
	}
	// 用 Wait 的非阻塞特性判断：若进程已退出，Wait 会立刻返回。
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("🔴 Detach 之后进程就退出了 —— 用户看不到登录结果（需求 2 未满足）")
	case <-time.After(500 * time.Millisecond):
		// 仍在运行 ⇒ 符合预期。
	}
}

// TestDetachReleasesMutex 守：Detach 必须释放登录互斥位。
//
// 若不释放：第一次登录能用，之后**再也点不动**（永久 409），
// 且错误信息只会说"已有一个登录流程在进行中"，指向完全错误的方向。
func TestDetachReleasesMutex(t *testing.T) {
	// 清掉可能残留的状态。
	manager.mu.Lock()
	prev := manager.current
	manager.current = nil
	manager.mu.Unlock()
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.current = prev
		manager.mu.Unlock()
	})

	s := fakeSession(t, filepath.Join(os.TempDir(), "wbapi-login-mutextest-"+t.Name()))
	manager.mu.Lock()
	manager.current = s
	manager.mu.Unlock()

	s.Detach()

	manager.mu.Lock()
	got := manager.current
	manager.mu.Unlock()
	if got != nil {
		t.Fatal("🔴 Detach 未释放互斥位 —— 下次登录会被永久挡住（ErrLoginBusy）")
	}
}

// TestDetachIsIdempotent 守：Detach 可重复调用（closeOnce 生效）。
func TestDetachIsIdempotent(t *testing.T) {
	s := fakeSession(t, filepath.Join(os.TempDir(), "wbapi-login-idemtest-"+t.Name()))
	// 连调三次：不能 panic，也不能重复 close channel。
	for i := 0; i < 3; i++ {
		s.Detach()
	}
	select {
	case <-s.closed:
	default:
		t.Fatal("Detach 后 closed 通道应已关闭")
	}
}

// TestDetachDeletesProfileAfterProcessExit 守：窗口关闭后 profile 会被清理。
//
// 这是"保留窗口"与"不堆积垃圾"之间的折中：
//   - 窗口开着时 profile 必须完好（否则浏览器异常）
//   - 窗口关掉后 profile 应被删掉（否则 Temp 无限堆积）
func TestDetachDeletesProfileAfterProcessExit(t *testing.T) {
	profile, err := os.MkdirTemp("", "wbapi-login-cleanup-*")
	if err != nil {
		t.Fatalf("建临时 profile 失败: %v", err)
	}
	// 放个文件进去，确认整个目录被删（不是只删空目录）。
	if err := os.WriteFile(filepath.Join(profile, "x.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 替身进程立即退出 ⇒ Detach 的清理 goroutine 很快就会跑完。
	s := fakeSession(t, profile)
	s.Detach()

	// 给清理 goroutine 一点时间（它先 Wait 再删，带重试）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(profile); os.IsNotExist(statErr) {
			return // 已删，符合预期。
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("进程退出后 profile 未被清理: %s（会在 Temp 里堆积）", profile)
	_ = os.RemoveAll(profile)
}

// TestCloseStillDeletesImmediately 守：Close 的旧语义没被破坏。
//
// Detach 是新路径，但 Close 仍必须"立刻关且立刻删" ——
// 它是登录**失败**时收尾的另一条路（未来若需要"失败即关"就用它）。
// 有既有测试（TestCleanupStaleProfilesOnlyTouchesOwnPrefix 等）覆盖清理，
// 这里只补一条"Close 后 closed 通道关闭且互斥位释放"。
func TestCloseStillDeletesImmediately(t *testing.T) {
	profile, err := os.MkdirTemp("", "wbapi-login-close-*")
	if err != nil {
		t.Fatal(err)
	}
	s := fakeSession(t, profile)

	manager.mu.Lock()
	prev := manager.current
	manager.current = s
	manager.mu.Unlock()
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.current = prev
		manager.mu.Unlock()
	})

	_ = s.Close()

	select {
	case <-s.closed:
	default:
		t.Error("Close 后 closed 通道应已关闭")
	}
	manager.mu.Lock()
	got := manager.current
	manager.mu.Unlock()
	if got == s {
		t.Error("Close 未释放互斥位")
	}
}

// TestCleanupStaleProfilesSkipsNothingImportant 是既有前缀护栏的补充：
// 确认清理只认 wbapi-login- 前缀（防止将来改前缀时误删）。
func TestCleanupPrefixIsStable(t *testing.T) {
	src, err := os.ReadFile("browser.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// 前缀是"崩溃残留可识别"与"防误删"的共同依据，必须稳定。
	if !strings.Contains(text, `"wbapi-login-*"`) {
		t.Error("MkdirTemp 的前缀应是 wbapi-login-*（崩溃残留靠它识别）")
	}
	if !strings.Contains(text, `strings.HasPrefix(base, "wbapi-login-")`) {
		t.Error("removeOwnProfile 必须校验 wbapi-login- 前缀（防误删用户数据）")
	}
}

// 保证 sync 被使用（fakeSession 的并发安全性依赖它）。
var _ = sync.Mutex{}
