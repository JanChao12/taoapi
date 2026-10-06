package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 本文件针对 Codex 第 17 轮对 rename 重试提出的四条要求建立测试。
//
// Codex 原话（17-codex）：
//
//	只对 Windows 目标占用类错误重试是正确方向。最终应确保：
//	- 重试耗尽后临时文件会清理；
//	- 非占用错误立即返回；
//	- 写盘失败不会更新内存快照；
//	- 重试次数和等待时间不会让面板请求长时间无反馈。
//	这些有测试覆盖后即可接受。
//
// 【为什么现在才补】：第 17 轮我们只说明了修法（10 次 × 5ms），
// 没说测试覆盖情况 —— 而 Codex 的接受前提是"有测试覆盖后"。
// 提取第 13~17 轮结论时，这项被如实标记为"信息缺口"（拒绝推断为已完成）。
// 查证后确认：storage 包当时只有 lock 相关测试，file.go 没有任何测试。
// 本文件补上这个缺口。

// realRename 保存 os.Rename 的原始引用。
//
// 用途：注入替身之后，某些测试仍需要在"第 N 次之后改用真实 rename"
// （验证"挺过瞬时占用后真的能成功"）。`os.Rename` 本身未被改动
// （被改的是包内变量 renameFn），所以这里直接指向它。
var realRename = os.Rename

// withRenameFn 临时替换最底层的 os.Rename，测试结束自动还原。
//
// 🔴 注入点在最底层是**刻意的**：被调用的仍是同一个 AtomicWrite →
// renameWithRetry 生产控制流，重试循环、次数上界、错误分类全都会被真实执行。
//
// 【踩过的坑】最初把注入点放在 renameWithRetry 之上（替换整个重试函数），
// 结果"重试 10 次才放弃"这条要求**根本没被测到** ——
// 测试显示只调用 1 次，看起来像实现有 bug，实际是注入点选错了层次。
// 测试替身的注入层次必须低于"你要验证的那段逻辑"。
func withRenameFn(t *testing.T, fn func(from, to string) error) {
	t.Helper()
	orig := renameFn
	renameFn = fn
	t.Cleanup(func() { renameFn = orig })
}

// tempFilesLeft 返回目录下残留的临时文件（AtomicWrite 的中间产物）。
func tempFilesLeft(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	var left []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			left = append(left, e.Name())
		}
	}
	return left
}

// TestAtomicWriteCleansTempFileWhenRenameExhaustsRetries 对应要求①：
// 重试耗尽后，临时文件必须被清理掉。
//
// 为什么这条重要：临时文件里是**加密后的凭据**（auth 包用它写 accounts.json）。
// 重试耗尽却把 .tmp 留在盘上，等于凭据多了一份无人管理的副本，
// 且用户永远不会知道它存在。
func TestAtomicWriteCleansTempFileWhenRenameExhaustsRetries(t *testing.T) {
	dir := testDir(t)
	target := filepath.Join(dir, "accounts.json")

	// 让 rename 永远失败（模拟目标被别的进程死占不放）。
	calls := 0
	withRenameFn(t, func(from, to string) error {
		calls++
		return &os.LinkError{
			Op:  "rename",
			Old: from,
			New: to,
			Err: syscall.Errno(5), // ERROR_ACCESS_DENIED，归入"可重试"类
		}
	})

	err := AtomicWrite(target, []byte(`{"secret":"value"}`), 0o600)
	if err == nil {
		t.Fatal("rename 持续失败时 AtomicWrite 必须报错，不能假装成功")
	}

	// ① 临时文件必须被清理。
	if left := tempFilesLeft(t, dir); len(left) > 0 {
		t.Fatalf("重试耗尽后仍残留临时文件 %v —— 里面可能是加密凭据", left)
	}

	// 目标文件不应被创建（rename 从未成功）。
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("rename 全部失败，目标文件不该存在")
	}
}

// TestAtomicWriteRetriesExactlyTheConfiguredBudget 对应要求④：
// 重试次数有明确上界，不会无限循环。
//
// 同时验证"时间是可控的"：调用次数必须恰好等于 renameRetries，
// 而不是"一直重试到成功"。没有上界的话，面板保存请求可能挂死。
func TestAtomicWriteRetriesExactlyTheConfiguredBudget(t *testing.T) {
	dir := testDir(t)

	calls := 0
	withRenameFn(t, func(from, to string) error {
		calls++
		// 用真实的 Windows ERROR_ACCESS_DENIED(5)。
		// ⚠️ 不能用 os.ErrPermission —— 它在 Windows 上不被判为可重试，
		// 会只试 1 次，测不出"重试预算"这条要求。
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.Errno(5)}
	})

	start := time.Now()
	if err := AtomicWrite(filepath.Join(dir, "a.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("应当报错")
	}
	elapsed := time.Since(start)

	// 🔴 这里断言【字面量 30】，不是断言 == renameRetries。
	//
	// 原因（反向控制实验抓到的真实缺陷）：
	// 最初写成 `calls != renameRetries`，属【自我参照断言】——
	// 实测把 renameRetries 从 10 改成 1 后，实现和断言【同步变化】，
	// 测试照样通过，完全没起到守护作用。
	// 用被测对象的常量去校验被测对象的行为，等于没测。
	//
	// 30 这个数字是**契约**（2026-10-05 实验定标，见 file.go 的常量注释），
	// 改动它必须是有意识的决定，并且会让这条测试变红提醒你更新文档。
	//
	// ⚠️ 它确实按预期变红过一次：我把 10 → 30 时，这条测试拦住了我，
	//    迫使我回去确认"实验依据是否成立"。这就是它该有的作用。
	const wantRetries = 30
	if calls != wantRetries {
		t.Fatalf("重试次数应为 %d，实际 %d —— 上界是契约，不能随手改", wantRetries, calls)
	}
	// 顺带确认常量与契约一致（防止有人只改了常量而没改这里）。
	if renameRetries != wantRetries {
		t.Fatalf("renameRetries 常量已变为 %d，与契约 %d 不符；"+
			"改这里之前请先确认实验依据（TestRenameRetryBudgetAdequacy）仍然成立",
			renameRetries, wantRetries)
	}

	// ④ 时间上界：最坏 30 次 × 5ms = 150ms，留足调度余量。
	// 同样用字面量计算，避免"改了常量上限跟着放宽"。
	maxWant := wantRetries*5*time.Millisecond + 2*time.Second
	if elapsed > maxWant {
		t.Fatalf("重试耗时 %v 超出上界 %v（面板会长时间无反馈）", elapsed, maxWant)
	}
	t.Logf("重试 %d 次共耗时 %v（上界 %v）", calls, elapsed, maxWant)
}

// TestAtomicWriteReturnsImmediatelyOnNonRetryableError 对应要求②：
// 非"目标被占用"类的错误必须立即返回，不得重试。
//
// 反例（正是这条要防的）：磁盘满 / 路径不存在 这类真实故障
// 如果也被当成"瞬时占用"去重试 10 次，就会把快速失败拖成慢失败，
// 而用户看到的是"保存转圈很久最后失败"。
func TestAtomicWriteReturnsImmediatelyOnNonRetryableError(t *testing.T) {
	dir := testDir(t)

	calls := 0
	withRenameFn(t, func(from, to string) error {
		calls++
		// 一个明确不属于"占用"的错误：目录不存在。
		return &os.LinkError{
			Op:  "rename",
			Old: from,
			New: to,
			Err: os.ErrNotExist,
		}
	})

	err := AtomicWrite(filepath.Join(dir, "a.json"), []byte("x"), 0o600)
	if err == nil {
		t.Fatal("应当报错")
	}

	if calls != 1 {
		t.Fatalf("非占用类错误应只尝试 1 次，实际 %d 次 —— 真实故障被拖成了慢失败", calls)
	}

	// ② 顺带确认临时文件也被清理（失败路径统一清理）。
	if left := tempFilesLeft(t, dir); len(left) > 0 {
		t.Fatalf("立即返回时也须清理临时文件，实际残留 %v", left)
	}
}

// TestIsRetryableRenameErrClassification 直接钉住"哪些错误算占用"。
//
// 这是要求②的判定内核。分类错误有两个方向的有害后果：
//   - 该重试的没重试 → 偶发保存失败（用户以为改了设置其实没生效）
//   - 不该重试的重试了 → 真实故障变慢失败
//
// 🔴 本表里的 errno 值来自 2026-10-05 的实测，不是推测：
// 真实 Windows rename 占用错误报的是 Errno=5（ERROR_ACCESS_DENIED），
// 而 Go 的 syscall.EACCES 在 Windows 上是 0x20000001 —— 两者不相等。
// 所以这里【必须】用 5 来构造用例，用 syscall.EACCES 反而测不出真实路径。
func TestIsRetryableRenameErrClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil 不算可重试（不得 panic）", nil, false},
		{"Windows ERROR_ACCESS_DENIED (errno 5)", syscall.Errno(5), true},
		{"Windows ERROR_SHARING_VIOLATION (errno 32)", syscall.Errno(32), true},
		{"包在 LinkError 里的 errno 5", &os.LinkError{
			Op: "rename", Old: "a", New: "b", Err: syscall.Errno(5)}, true},
		{"文本 access is denied", errors.New("rename x: Access is denied."), true},
		{"文本 sharing violation", errors.New("The process cannot access the file: sharing violation"), true},
		{"文本 being used by another process",
			errors.New("The process cannot access the file because it is being used by another process."), true},
		{"文件不存在不属于占用", os.ErrNotExist, false},
		{"磁盘满不属于占用", errors.New("no space left on device"), false},
		{"路径太长不属于占用", errors.New("The filename or extension is too long"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableRenameErr(tc.err); got != tc.want {
				t.Fatalf("isRetryableRenameErr(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPermissionErrIsPlatformDependent 记录一个跨平台差异，避免将来误改。
//
// `os.ErrPermission` 在**类 Unix** 上就是 EACCES → 应当可重试；
// 但在 **Windows** 上它只是一个普通 error（文本 "permission denied"），
// 与 syscall.EACCES(0x20000001) 无关，也不匹配任何文本兜底 → 不可重试。
//
// 生产路径不受影响（真实的 Windows 占用错误报的是 errno 5，走 errno 分支），
// 但把这个差异写成断言可以防止有人"顺手让它跨平台一致"而破坏 Windows 行为。
func TestPermissionErrIsPlatformDependent(t *testing.T) {
	got := isRetryableRenameErr(os.ErrPermission)
	switch runtime.GOOS {
	case "windows":
		if got {
			t.Fatal("Windows 上 os.ErrPermission 不是占用类错误，不该判为可重试")
		}
	default:
		if !got {
			t.Fatal("类 Unix 上 os.ErrPermission 就是 EACCES，应当可重试")
		}
	}
	t.Logf("GOOS=%s 时 os.ErrPermission 可重试=%v", runtime.GOOS, got)
}

// TestWindowsErrnoConstantsAreNotWhatYouThink 是本文件最重要的一条：
// 它把"我们踩过的那个认知错误"固化成断言，防止有人把实现改回去。
//
// 背景：file.go 原先写 `errors.Is(err, syscall.EACCES)` 来判断
// Windows 的"访问被拒绝"。实测表明这在 Windows 上**永远为 false**：
//
//	syscall.EACCES     = 0x20000001 = 536870913
//	真实 ERROR_ACCESS_DENIED = 5
//
// 功能之所以没坏，是因为紧跟着的文本兜底恰好匹配了 "Access is denied."。
// 这是"碰巧对" —— 一旦 Go 改了错误文案，重试会静默失效，
// 表现为"偶发保存失败，用户以为改了设置其实没生效"，极难排查。
//
// 这条测试在 Windows 上跑时校验真实数值；在类 Unix 上则跳过
// （那里 EACCES 就是普通 errno，这个坑不存在）。
func TestWindowsErrnoConstantsAreNotWhatYouThink(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("该认知坑仅存在于 Windows")
	}

	const realAccessDenied = 5
	if uintptr(syscall.EACCES) == realAccessDenied {
		t.Fatal("前提已变：syscall.EACCES 现在等于 5 了，" +
			"file.go 的说明与实现都应重新审视")
	}

	// 真实的 Windows 占用错误必须被判为可重试（走 errno 分支）。
	if !isRetryableRenameErr(syscall.Errno(realAccessDenied)) {
		t.Fatal("Windows ERROR_ACCESS_DENIED(5) 必须被判为可重试")
	}

	// 记录实测事实，便于日后核对。
	t.Logf("syscall.EACCES = %d (0x%X)，真实 ERROR_ACCESS_DENIED = %d —— 二者不等",
		uintptr(syscall.EACCES), uintptr(syscall.EACCES), realAccessDenied)
}

// TestAtomicWriteSucceedsAfterTransientFailure 覆盖"真的挺过了瞬时占用"这条路。
//
// 前几条测的都是失败侧；这条测成功侧：前两次假装被占用，
// 第三次成功 —— 最终必须写入成功且内容正确。
// 没有这条，实现可能退化成"永远重试但从不成功"却无人发现。
func TestAtomicWriteSucceedsAfterTransientFailure(t *testing.T) {
	dir := testDir(t)
	target := filepath.Join(dir, "a.json")
	want := []byte(`{"ok":true}`)

	fails := 0
	withRenameFn(t, func(from, to string) error {
		if fails < 2 {
			fails++
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.Errno(5)}
		}
		return realRename(from, to) // 之后走真实 rename
	})

	if err := AtomicWrite(target, want, 0o600); err != nil {
		t.Fatalf("挺过瞬时占用后应当成功，实际: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("内容不符: 期望 %q，实际 %q", want, got)
	}

	// 成功后不应残留临时文件。
	if left := tempFilesLeft(t, dir); len(left) > 0 {
		t.Fatalf("成功后残留临时文件 %v", left)
	}
}

// TestAtomicWriteIsAtomicUnderConcurrency 是回归测试：
// 复现当初发现这个 bug 的场景（8 goroutine × 200 次并发替换同一文件）。
//
// 【这条与其它几条的区别】：其它几条注入假 rename 来测分支，
// 这条**不注入**、走完全真实路径，目的就是证明修复在真实并发下成立。
//
// 在修复前，这个场景稳定出现 Access is denied；
// 修复后必须零失败，且最终文件内容必须是某一次完整写入的值 ——
// 绝不能是半个 JSON（这正是"临时文件 + rename"要保证的原子性）。
func TestAtomicWriteIsAtomicUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("并发压测在 -short 下跳过")
	}

	dir := testDir(t)
	target := filepath.Join(dir, "concurrent.json")

	const (
		goroutines = 8
		perG       = 200
	)

	// 每次写入一个自描述、可校验完整性的 JSON。
	makePayload := func(g, i int) []byte {
		return []byte(`{"g":` + itoa(g) + `,"i":` + itoa(i) + `}`)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perG)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if err := AtomicWrite(target, makePayload(g, i), 0o600); err != nil {
					errCh <- err
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		t.Fatalf("并发写入出现 %d 次失败（修复前正是这个现象）: 首个错误 %v",
			len(errs), errs[0])
	}

	// 最终文件必须是一个完整的 JSON 对象（不是半个、不是空）。
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	s := string(got)
	if !strings.HasPrefix(s, `{"g":`) || !strings.HasSuffix(s, `}`) {
		t.Fatalf("文件内容不是完整 JSON（原子性被破坏）: %q", s)
	}

	// 且必须能被解析（结构完整）。
	if !looksLikeCompleteJSON(s) {
		t.Fatalf("最终内容结构不完整: %q", s)
	}

	// 不留临时文件。
	if left := tempFilesLeft(t, dir); len(left) > 0 {
		t.Fatalf("并发写入后残留临时文件 %v", left)
	}
}

// looksLikeCompleteJSON 做一个极简的结构完整性检查。
//
// 不用 encoding/json 是因为这里要验的是"没有被截断"，
// 而这个极简检查恰好能抓住截断（缺右括号）。
func looksLikeCompleteJSON(s string) bool {
	return strings.Count(s, "{") == strings.Count(s, "}") &&
		strings.Count(s, "[") == strings.Count(s, "]")
}

// itoa 避免为一个小工具引入 strconv 的 import（本文件其它地方不需要）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestAtomicWriteAppliesPerm 确认写入后权限被设置（0600 的额外防护）。
//
// 注意：Windows 上 chmod 语义有限，此断言只在非 Windows 上有意义，
// 但即使 Windows 也应当不报错（本函数只检查"不报错 + 文件可读"）。
func TestAtomicWriteAppliesPerm(t *testing.T) {
	dir := testDir(t)
	target := filepath.Join(dir, "perm.json")

	if err := AtomicWrite(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("文件应当存在: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	// Windows 上权限位不保证，只在类 Unix 上严格断言。
	if os.PathSeparator == '/' {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("权限应为 0600，实际 %o", got)
		}
	}
}

// TestAtomicWriteCreatesParentDir 确认会自动建目录（首次运行时的路径）。
func TestAtomicWriteCreatesParentDir(t *testing.T) {
	dir := testDir(t)
	nested := filepath.Join(dir, "a", "b", "c", "x.json")

	if err := AtomicWrite(nested, []byte("x"), 0o600); err != nil {
		t.Fatalf("嵌套目录写入失败: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("文件应当存在: %v", err)
	}
}
