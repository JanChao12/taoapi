package auth

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════
// R2「保存顺序」的护栏（2026-10-07 修，本轮补测试）
// ═══════════════════════════════════════════════════════════════════
//
// 要保的性质（一句话）：
//
//	**写盘顺序 == 取快照顺序。**
//
// 为什么它是独立于"快照自洽性"的另一件事：
// `Save` 原先在锁内取快照、出锁后才序列化+写盘。
// 每份快照各自自洽，但**完成写盘的先后可以颠倒** ⇒
// 较旧的那份最后落盘，已删除的账号/旧凭据**复活**（重启后被读回）。
//
// 🔴 本文件用**确定性插桩**而不是"真并发跑 N 次"来验证：
//
//	真并发靠"恰好交错"，实测抖动极大（本仓库 `race_demo_test.go`
//	记录过 8/4073 与 0 次的差异）。不稳的护栏等于没有护栏。
//	这里用 `Persister.saveHook` 让交错**必然发生**。

// testCodec 是恒等编解码器：本测试只关心落盘的**账号集合**，
// 不关心凭据加密（加密路径另有测试守着）。
// 用恒等而非 XOR，是为了让断言与"密文内容"完全解耦。
type testCodec struct{}

func (testCodec) Name() string                     { return "test-identity" }
func (testCodec) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (testCodec) Decrypt(c []byte) ([]byte, error) { return c, nil }

// loadUIDs 读出磁盘上当前有哪些 UID。
func loadUIDs(t *testing.T, p *Persister) []string {
	t.Helper()
	st, err := p.Load()
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	return st.UIDs()
}

// TestSaveOrderMatchesSnapshotOrder 是 R2 的**回归护栏**。
//
// 受控交错（用 saveHook 精确制造）：
//
//	goroutine-旧: Save() 取到含 X 的快照 ──→ 【在此阻塞】
//	主 goroutine: 删除 X，然后 Save()（快照不含 X）
//	              ──→ 期望它完成写盘
//	goroutine-旧: ──→ 解除阻塞，继续写盘
//
// 修复后：`saveMu` 覆盖了"取快照"这一步 ⇒ 第二次 Save 必须**等**第一次
// 整条链做完才可能取快照 ⇒ 不可能出现"旧快照后写盘"。
// 因此最终的磁盘内容里 **X 必须已删除**。
func TestSaveOrderMatchesSnapshotOrder(t *testing.T) {
	path := tmpPath(t)
	p := NewPersister(path, testCodec{})

	st := NewStore()
	st.Put(&Account{UID: "keep", Nickname: "保留"})
	st.Put(&Account{UID: "victim", Nickname: "待删除"})

	// ── 第一次 Save（"旧"）：取完快照后阻塞在插桩点 ──
	//
	// 🔴 绝不能用 `sync.Once` 来实现"只阻塞第一次"：
	//
	//	`Once.Do` 在第一个调用执行期间会**阻塞其它调用者**，
	//	于是第二次 Save 会被 `Once` 挡住 —— 那等于**测试自己提供了
	//	串行化**，把要验的缺陷掩盖掉。我第一版正是这么写的，
	//	结果反向对照（去掉 saveMu）时测试**照样通过**，护栏形同虚设。
	//
	//	正确做法：用**原子标志**只让第一次进钩子时阻塞，其余直接放行 ——
	//	这样"第二次能否越过第一次"完全由被测的 `saveMu` 决定。
	release := make(chan struct{})
	entered := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	p.saveHook = func() {
		if first.CompareAndSwap(true, false) {
			close(entered)
			<-release // 等主 goroutine 完成"删除 + 第二次 Save"
		}
	}

	oldDone := make(chan error, 1)
	go func() { oldDone <- p.Save(st) }()

	// 等它确实进到"快照已取、尚未写盘"的位置
	<-entered

	// ── 删除 victim，再 Save 一次（"新"）──
	st.Remove("victim")
	if got := st.UIDs(); len(got) != 1 || got[0] != "keep" {
		t.Fatalf("前置条件不成立：删除后应只剩 keep，实际 %v", got)
	}

	// 第二次 Save 放到 goroutine 里（修复后它会被 saveMu 挡住）
	newDone := make(chan error, 1)
	go func() { newDone <- p.Save(st) }()

	// 放行第一次 Save。此后两次 Save 都会跑完 ——
	// ⚠️ 不用"此刻 newDone 是否已完成"做断言：
	//    那个判断依赖 goroutine 调度时机，我在第一版里用它，
	//    结果**在未修复的代码上也照样通过**（select 的 default
	//    在第二次 Save 被调度之前就命中了）。那是"测试自己不可靠"，
	//    不是"缺陷不存在"。真正该断言的是**磁盘最终内容**。
	close(release)

	if err := <-oldDone; err != nil {
		t.Fatalf("第一次 Save 失败: %v", err)
	}
	if err := <-newDone; err != nil {
		t.Fatalf("第二次 Save 失败: %v", err)
	}

	// ── 🔴 断言：磁盘上 victim 必须【不在】 ──
	//
	// 这是本测试的**唯一真判据**，且它是确定性的：
	//
	//	修复前：第一次（较旧、含 victim）的快照最后落盘 ⇒ [keep victim]
	//	        已删除账号复活，且**下次启动会被真实读回**。
	//	修复后：写盘顺序 == 取快照顺序 ⇒ [keep]
	//
	// 已做**反向对照**：临时移除 saveMu 后，本断言确实变红
	// （实测磁盘 = [keep victim]），证明护栏真的守得住。
	got := loadUIDs(t, p)
	if len(got) != 1 || got[0] != "keep" {
		t.Fatalf("🔴 R2 复现：磁盘内容 = %v，期望仅 [keep]。"+
			"较旧的快照最后落盘，已删除账号复活", got)
	}
}

// TestSaveIsSerializedAcrossGoroutines 是**无插桩**的并发护栏：
// 大量并发 Save 后，磁盘内容必须等于"最后一次取快照时的状态"，
// 且不得出现中间态（不可能为空、不可能含已删除账号）。
//
// ⚠️ 这条**不能**替代上面那条：并发跑只提高"撞上"的概率，
// 不保证必现。它守的是"串行化没有把正常路径写坏"。
func TestSaveIsSerializedAcrossGoroutines(t *testing.T) {
	path := tmpPath(t)
	p := NewPersister(path, testCodec{})

	st := NewStore()
	for _, uid := range []string{"a", "b", "c"} {
		st.Put(&Account{UID: uid, Nickname: uid})
	}

	const rounds = 40
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Save(st); err != nil {
				t.Errorf("Save 失败: %v", err)
			}
		}()
	}
	wg.Wait()

	// 全部账号都在（没有任何一次 Save 把它写丢）
	got := loadUIDs(t, p)
	if len(got) != 3 {
		t.Fatalf("并发保存后账号数 = %d（%v），期望 3 —— 疑似写丢", len(got), got)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i] != want {
			t.Fatalf("UID 列表 = %v，期望 [a b c]", got)
		}
	}

	// 文件必须是**完整可解析**的（原子替换 + 串行化不产生半截文件）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("🔴 文件被写成 0 字节")
	}
}

// TestSaveDoesNotDeadlockWhenCalledFromGoroutines 守"锁序没有反转"。
//
// 🔴 为什么值得单独守（见 Persister.saveMu 的锁序说明）：
//
//	`Save` 的持锁顺序是 saveMu → Store.mu。
//	若将来有人把 `Save` 写进 `Mutate` 回调里（即：持 Store.mu 时调 Save），
//	就会形成**反向持锁** ⇒ 死锁。
//	而 `Store.mu` 是**非可重入**的，那种写法本身还会立刻自死锁。
//
//	本测试用超时把"死锁"变成**失败**而不是"挂住 CI"。
func TestSaveDoesNotDeadlockWhenCalledFromGoroutines(t *testing.T) {
	path := tmpPath(t)
	p := NewPersister(path, testCodec{})
	st := NewStore()
	st.Put(&Account{UID: "u1", Nickname: "n"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 读写混跑：证实"Save 与 Mutate 交替"不会自己锁死
				st.Mutate("u1", func(a *Account) bool {
					a.LastError = "x"
					return true
				})
				_ = p.Save(st)
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("🔴 超时：疑似死锁（检查是否有路径在 Store.mu 内调用 Save）")
	}
}
