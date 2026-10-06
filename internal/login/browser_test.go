// browser_test.go：浏览器生命周期的护栏测试（不真的启动浏览器）。
//
// 真的启动浏览器需要本机装了 Chrome/Edge，不适合放进常规测试。
// 这里钉住的是**不依赖浏览器**的关键契约：
//   - profile 删除的路径校验（防误删用户数据）
//   - 崩溃残留清理只认自己的前缀
//   - 浏览器路径探测返回的是**真实存在**的路径
package login

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoveOwnProfileRefusesForeignPaths 守住"只删自己创建的目录"。
//
// 这是防误删的核心护栏：如果路径校验被删掉，
// 一个 bug 就可能让程序删掉用户的任意目录。
func TestRemoveOwnProfileRefusesForeignPaths(t *testing.T) {
	// 造几个"不该被删"的目录（带哨兵文件）。
	cases := []string{
		filepath.Join(os.TempDir(), "user-important-data"),
		filepath.Join(os.TempDir(), "chrome-profile-real"),
		filepath.Join(os.TempDir(), "wbapi-login"), // 少随机后缀，但前缀对 → 允许
		filepath.Join(os.TempDir(), "not-ours-wbapi-x"),
	}
	for _, dir := range cases {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("准备目录失败: %v", err)
		}
		sentinel := filepath.Join(dir, "keep.txt")
		os.WriteFile(sentinel, []byte("x"), 0o600)
	}
	t.Cleanup(func() {
		for _, dir := range cases {
			os.RemoveAll(dir)
		}
	})

	// 这些必须被拒绝（前缀不符）。
	mustRefuse := []string{
		cases[0], // user-important-data
		cases[1], // chrome-profile-real
		cases[3], // not-ours-wbapi-x
	}
	for _, dir := range mustRefuse {
		if err := removeOwnProfile(dir); err == nil {
			t.Fatalf("应拒绝删除路径 %q", filepath.Base(dir))
		}
		if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
			t.Fatalf("目录 %q 被误删了！", filepath.Base(dir))
		}
	}
}

// TestRemoveOwnProfileRefusesEmpty 守住空路径不被当作"当前目录"删掉。
func TestRemoveOwnProfileRefusesEmpty(t *testing.T) {
	if err := removeOwnProfile(""); err == nil {
		t.Fatal("空路径必须被拒绝")
	}
}

// TestRemoveOwnProfileRefusesOutsideTemp 守住"必须位于系统临时目录下"。
func TestRemoveOwnProfileRefusesOutsideTemp(t *testing.T) {
	// 前缀对、但在临时目录之外的路径必须被拒绝。
	outside := filepath.Join(os.Getenv("SystemDrive")+`\`, "wbapi-login-evil")
	if err := removeOwnProfile(outside); err == nil {
		t.Fatalf("临时目录之外的路径应被拒绝: %s", outside)
	}
}

// TestRemoveOwnProfileDeletesOwnDir 确认自己创建的目录确实被删掉。
//
// 反向保证：上面都是"不该删的不删"，这条保证"该删的真删"。
// 少了它，一个"永远拒绝"的实现也能骗过所有测试。
func TestRemoveOwnProfileDeletesOwnDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "wbapi-login-test-*")
	if err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("x"), 0o600)

	if err := removeOwnProfile(dir); err != nil {
		t.Fatalf("应能删除自己创建的目录: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("目录应已被删除: %s", dir)
	}
}

// TestCleanupStaleProfilesOnlyTouchesOwnPrefix 守住残留清理的前缀边界。
func TestCleanupStaleProfilesOnlyTouchesOwnPrefix(t *testing.T) {
	// 造一个"我们的"残留 + 一个"别人的"目录。
	ours, err := os.MkdirTemp("", "wbapi-login-stale-*")
	if err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	theirs := filepath.Join(os.TempDir(), "some-other-app-data")
	os.MkdirAll(theirs, 0o700)
	os.WriteFile(filepath.Join(theirs, "keep.txt"), []byte("x"), 0o600)
	t.Cleanup(func() { os.RemoveAll(theirs) })

	cleaned, err := CleanupStaleProfiles()
	if err != nil {
		t.Logf("清理返回错误（可接受，取决于权限）: %v", err)
	}
	if cleaned < 1 {
		t.Fatalf("应至少清理 1 个自己的残留目录，实际 %d", cleaned)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Fatalf("自己的残留目录应被清理: %s", ours)
	}
	// 别人的目录必须原封不动。
	if _, err := os.Stat(filepath.Join(theirs, "keep.txt")); err != nil {
		t.Fatal("误删了他人的临时目录！")
	}
}

// TestFindBrowserReturnsExistingPath 确认探测返回的路径真实存在。
//
// 本机装了 Chrome 或 Edge 时才有意义；都没有时该返回 ErrNoBrowser
// （那也是正确行为，不算失败）。
func TestFindBrowserReturnsExistingPath(t *testing.T) {
	kind, path, err := FindBrowser()
	if err != nil {
		t.Skipf("本机无受支持浏览器（正确行为）: %v", err)
	}
	if kind != BrowserChrome && kind != BrowserEdge {
		t.Fatalf("意外的浏览器种类: %q", kind)
	}
	st, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("探测返回的路径不存在: %s (%v)", path, statErr)
	}
	if st.IsDir() {
		t.Fatalf("探测返回的是目录而非可执行文件: %s", path)
	}
	if !strings.HasSuffix(strings.ToLower(path), ".exe") {
		t.Fatalf("不像可执行文件: %s", path)
	}
}

// TestFreeLoopbackPortIsUsable 确认申请的端口在回环上可用。
func TestFreeLoopbackPortIsUsable(t *testing.T) {
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("端口号不合理: %d", port)
	}
}

// TestCurrentSessionInitiallyNil 确认没有登录流程时 CurrentSession 为 nil。
//
// 注意：这个测试假设没有并发登录在跑（常规 go test 下成立）。
func TestCurrentSessionInitiallyNil(t *testing.T) {
	// 先确保状态干净。
	manager.mu.Lock()
	prev := manager.current
	manager.current = nil
	manager.mu.Unlock()
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.current = prev
		manager.mu.Unlock()
	})

	if s := CurrentSession(); s != nil {
		t.Fatal("无登录流程时应返回 nil")
	}
}
