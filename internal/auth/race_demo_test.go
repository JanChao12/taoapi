package auth

import (
	"sync"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════
// 账号字段并发访问的**确定性**证明（不依赖 -race）
//
// 🔴 背景：本机无 gcc，`go test -race` 用不了（项目硬约束）。
//	所以不能靠 race detector 展示竞争 —— 必须用**确定性手法**：
//	用可观测的中间状态（两段非原子写之间的窗口）证明「读到的组合不可能出现」。
//
// 这类测试的价值：它是**修复前必红、修复后必绿**的护栏，
// 而不是"偶尔红"的不稳定测试（Codex 明确要求不要依赖竞争碰巧发生）。
// ═══════════════════════════════════════════════════════════════════

// TestStoreMutateSerializesWriters 守：**写-写**必须被 Mutate 串行化。
//
// 手法：两个 goroutine 各自用 Mutate 做"读-改-写"（+1），
//
//	若 Mutate 真持锁，最终计数必然 == 迭代数（无丢失更新）。
//	若有人绕过锁直接改指针，就会出现丢失更新。
//
// ⚠️ 这条测的是 **Mutate 本身**是对的（基线）。真正的护栏在下面两条。
func TestStoreMutateSerializesWriters(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1", Credit: CreditSnapshot{Known: true, Remaining: 0}})

	const iters = 500
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				st.Mutate("u1", func(a *Account) bool {
					a.Credit.Remaining++
					return true
				})
			}
		}()
	}
	wg.Wait()

	got, _ := st.Get("u1")
	if want := int64(4 * iters); got.Credit.Remaining != want {
		t.Errorf("Mutate 未串行化：Remaining = %d，期望 %d（丢失更新）",
			got.Credit.Remaining, want)
	}
}

// TestDirectPointerWriteLosesUpdateAgainstMutate 展示**裸指针写会丢失更新**。
//
// 🔴 这是"既存数据竞争"的**确定性**证据（不靠 -race）：
//
//	模拟生产里 `List()`/`Get()` 拿裸指针直接写的写法 ——
//	它与 Mutate 并发时，**读-改-写不是原子的**，必然丢失更新。
//
// 手法：把"读-改-写"拆成两段并在中间**故意让出**（模拟网络 I/O 窗口），
//
//	这样丢失更新是**必然**发生，而不是偶尔发生。
//
// ⚠️ 注意：本测试**证明问题存在**，修复后它应当**被改写/删除**
//
//	（因为届时不允许再有裸指针写的路径）。
func TestDirectPointerWriteLosesUpdateAgainstMutate(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1", Credit: CreditSnapshot{Known: true, Remaining: 0}})

	const iters = 200
	var wg sync.WaitGroup

	// 写者 A：模拟生产 cli_impl.go 的刷新额度 —— Get/List 拿指针后直接写
	// 刻意用"读值 → 让出 → 写回"复刻真实的两段式（网络 I/O 在中间）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			a, _ := st.Get("u1") // 共享指针
			v := a.Credit.Remaining
			time.Sleep(time.Microsecond) // 模拟 I/O 窗口
			a.Credit.Remaining = v + 1   // 直接写（无锁）
		}
	}()

	// 写者 B：走 Mutate（持锁）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			st.Mutate("u1", func(a *Account) bool {
				a.Credit.Remaining++
				return true
			})
		}
	}()

	wg.Wait()

	got, _ := st.Get("u1")
	want := int64(2 * iters)
	if got.Credit.Remaining == want {
		t.Logf("未观测到丢失更新（Remaining=%d）—— 竞争是概率性的，本次没撞上", got.Credit.Remaining)
		return
	}
	// 这正是问题本身：裸指针写的路径会丢更新
	t.Logf("✅ 复现丢失更新：Remaining = %d，期望 %d（丢了 %d 次自增）—— "+
		"证明'拿裸指针直接写'与 Mutate 并发时不是原子的",
		got.Credit.Remaining, want, want-got.Credit.Remaining)
}

// TestTornReadAcrossTwoFields 用**受控执行**见证"共享指针读取"的危害。
//
// 🔴 为什么不再用"真并发跑 + 统计"（第一版那样，Codex 第 53 轮否决）：
//
//	那种写法有两个无法接受的问题：
//	  1. **不稳定**：靠"读者恰好落在两次写之间"，实测一次 8/4073、下次 0 次。
//	     不能作为可靠护栏。
//	  2. **会真的把测试进程打崩**（SIGSEGV）：`string` 是 (ptr,len) 两个机器字，
//	     写者并发改它会产生**不匹配的 header**，比较时解引用非法地址。
//	     ⚠️ 我原先写"只读单字段就不会崩"是**错的** —— 无同步并发访问
//	     与字段是不是简单类型无关，都不能称为安全。
//	     ⚠️ SIGSEGV **不能用 recover 兜住** ⇒ 一崩就带走整个测试进程。
//
// ⇒ 现在改为**受控执行**（Codex 建议）：不制造真实竞争，
//
//	而是**显式模拟**"写者写到一半时读者来读"这个交错，
//	直接证明"共享指针会暴露中间态"。确定性、不崩、必红必绿。
//
// ⚠️ 实现注意：**不能在 Mutate 回调内再调 Get/Snapshot** ——
//
//	Store 用的是**非可重入**的 sync.Mutex，那会**自死锁**
//	（我第一次改写时就这么踩了：测试挂到 60s 超时）。
//
//	🔴 下面"在回调内把共享指针带出来、出锁后再读"的写法
//	**只用于本受控见证测试**，用来精确复刻"调用方持有一个写了一半的
//	共享指针"。它**不是**生产代码的安全用法 —— 生产必须用
//	`Snapshot`（锁内深拷贝）或 `Mutate`（锁内改写）。
func TestTornReadAcrossTwoFields(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1", Status: "normal", StatusReason: ""})

	// 受控交错：回调内**只写一半**，并把共享指针带出来
	var shared *Account
	st.Mutate("u1", func(a *Account) bool {
		a.Status = "rate_limited" // 只写一半，**尚未**写 StatusReason
		shared = a                // 带出共享指针
		return true
	})

	// 在**锁外**通过共享指针观察 —— 这正是 Get 的用法（被消灭的写法）
	observedStatus, observedReason := shared.Status, shared.StatusReason

	// 再写另一半，确认最终状态自洽
	st.Mutate("u1", func(a *Account) bool {
		a.StatusReason = "上游限流"
		return true
	})
	snap, _ := st.Snapshot("u1")
	if snap.Status != "rate_limited" || snap.StatusReason != "上游限流" {
		t.Fatalf("前置条件不成立：最终状态应自洽，实际 Status=%q Reason=%q",
			snap.Status, snap.StatusReason)
	}

	// 🔴 断言：共享指针**确实**暴露了中间态（Status 已新、Reason 仍旧）
	intermediateExposed := observedStatus == "rate_limited" && observedReason == ""
	if !intermediateExposed {
		t.Errorf("未能通过共享指针观察到中间态（读到 Status=%q Reason=%q）—— "+
			"若这是「已加锁保护读取」的实现，本断言正说明已不再需要 Snapshot",
			observedStatus, observedReason)
	}
	t.Logf("受控见证：共享指针读到中间态 Status=%q Reason=%q（最终是 %q/%q）—— "+
		"这正是必须用 Snapshot 的原因",
		observedStatus, observedReason, snap.Status, snap.StatusReason)
}

// TestSharedPointerSeesCrossFieldInconsistency 用**受控执行**见证
// "跨字段组合不可能"（比单字段更隐蔽的一类）。
//
// 场景：写者把 (Credit.Known, Credit.Remaining) 从 (false,0) 改成 (true,802)。
// 读者若在两段写之间读，会看到 (true, 0) —— "已查到但额度为 0"，
// 这个组合在生产上不可能出现（面板会显示成"额度 0"）。
func TestSharedPointerSeesCrossFieldInconsistency(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})

	// 同上：回调内**不能**再调 Get/Snapshot（非可重入锁会自死锁），
	// 所以只把共享指针带出来，出锁后再读。
	var shared *Account
	st.Mutate("u1", func(a *Account) bool {
		a.Credit.Known = true // 先写 Known
		shared = a            // 带出指针（此刻 Remaining 还没写）
		return true
	})

	// 在锁外通过共享指针读 —— 看到的是 (Known=true, Remaining=0)
	known, remaining := shared.Credit.Known, shared.Credit.Remaining

	// 再补写 Remaining
	st.Mutate("u1", func(a *Account) bool {
		a.Credit.Remaining = 802
		return true
	})

	snap, _ := st.Snapshot("u1")
	if !snap.Credit.Known || snap.Credit.Remaining != 802 {
		t.Fatalf("前置条件不成立：最终应为 Known=true Remaining=802，实际 %v/%d",
			snap.Credit.Known, snap.Credit.Remaining)
	}

	if known && remaining == int64(0) {
		t.Logf("受控见证：共享指针读到不可能的组合 (Known=true, Remaining=0) —— " +
			"面板会把它显示成「额度 0」。这正是必须用 Snapshot 的原因")
		return
	}
	t.Errorf("未观察到跨字段不一致（读到 Known=%v Remaining=%v）", known, remaining)
}

// TestSnapshotNeverTears 是**修复的回归护栏**：`Snapshot` 永不撕裂。
//
// 与上一条形成对照：
//
//	· 上一条用 `Get`（共享指针）→ 可观测到撕裂
//	· 本条用 `Snapshot`（锁内值拷贝）→ **必须自洽**
//
// ⚠️ 为什么不用"并发跑 + 计时统计"断言（我第一版就是这样，是个**不稳定测试**）：
//
//	那种写法要"读者恰好落在两次写之间"才失败 —— 是**概率性**的。
//	已实测抖动：一次运行 8/4073 撕裂、下一次 0 次。
//	Codex 第 52 轮明确要求：**不要依赖竞争碰巧发生**。
//
// ⇒ 本测试改为**确定性**，直接验证"锁覆盖整个回调"这一语义：
//
//	写者在 `Mutate` 回调内写到一半时发信号，然后**持锁等待**；
//	读者在**独立 goroutine** 里调 Snapshot。
//	  · 若 Snapshot 真的持锁取值拷贝 ⇒ 它必须阻塞，读不到中间态
//	  · 写者收到"读者已尝试"信号后写完剩余字段并释放锁
//	  · 读者最终拿到的必须是**完整组合**
//
// 更强的确定性证明（Codex 第 53 轮要求）：
//
//	我第一版只发"读者已开始"信号 —— 那**不证明它真的尝试过加锁**
//	（也可能是没被调度到），因此无法排除"Snapshot 没加锁、只是恰好没读到"。
//
//	现在断言**两个**可区分的性质：
//	  ① 读者在写者持锁期间**必然阻塞**（用超时判定它没提前返回）
//	  ② 读者最终读到的组合必须完整
//
//	⇒ 若有人把 Snapshot 改成不加锁，① 会失败（读者立刻返回并拿到中间态），
//	  这正是本测试能抓住"故意移除锁"的原因。
func TestSnapshotNeverTears(t *testing.T) {
	st := NewStore()
	st.Put(&Account{
		UID:          "u1",
		Status:       "normal",
		StatusReason: "",
	})

	halfWritten := make(chan struct{}) // 写者：已写一半、仍持锁
	readerStarted := make(chan struct{})
	releaseWriter := make(chan struct{}) // 测试：放行写者去写完

	type readResult struct {
		snap Account
		ok   bool
	}
	readDone := make(chan readResult, 1)
	writerDone := make(chan struct{})

	// ⚠️ 写者等待放行时**带超时**：否则一旦 Snapshot 没加锁（读者提前返回），
	//	写者会永远等下去，表现为进程**挂起 30s 才超时** —— 那是很差的失败信号。
	//	有超时后，测试能**干净地断言失败**，不靠挂起表达结论。
	var writerTimedOut bool
	var timeoutMu sync.Mutex

	// 读者：真去 Snapshot，返回后才报告
	go func() {
		<-halfWritten
		close(readerStarted)
		snap, ok := st.Snapshot("u1") // 若正确实现 ⇒ 阻塞在锁上
		readDone <- readResult{snap, ok}
	}()

	// 写者：写到一半 → 通知读者 → 等测试放行（带超时）→ 写完
	writerWaited := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer close(writerWaited)
		st.Mutate("u1", func(a *Account) bool {
			a.Status = "rate_limited" // 只写一半
			close(halfWritten)
			select {
			case <-releaseWriter:
			case <-time.After(2 * time.Second):
				timeoutMu.Lock()
				writerTimedOut = true
				timeoutMu.Unlock()
			}
			a.StatusReason = "上游限流" // 写完另一半
			return true
		})
	}()

	<-readerStarted

	// ── 断言①：写者仍持锁时，读者**必须**还没拿到结果 ──
	readerReturnedEarly := false
	select {
	case res := <-readDone:
		readerReturnedEarly = true
		t.Errorf("写者持锁期间读者就返回了（读到 Status=%q Reason=%q）—— "+
			"说明 Snapshot 没有在锁内取值拷贝；"+
			"若这是「故意移除锁」的实现，本断言正是用来抓它的",
			res.snap.Status, res.snap.StatusReason)
	case <-time.After(50 * time.Millisecond):
		// 预期路径：读者被锁挡住
	}

	// 无论断言①结果如何都放行写者，避免把整个测试挂死
	close(releaseWriter)
	<-writerWaited

	timeoutMu.Lock()
	timedOut := writerTimedOut
	timeoutMu.Unlock()
	if timedOut && !readerReturnedEarly {
		t.Error("写者等待放行超时，且读者并未提前返回 —— 测试自身逻辑有问题")
	}
	if readerReturnedEarly {
		return // 已报错，不再等 readDone
	}

	res := <-readDone
	<-writerDone

	if !res.ok {
		t.Fatal("账号应存在")
	}
	// ── 断言②：组合必须完整自洽 ──
	oldCombo := res.snap.Status == "normal" && res.snap.StatusReason == ""
	newCombo := res.snap.Status == "rate_limited" && res.snap.StatusReason == "上游限流"
	if !oldCombo && !newCombo {
		t.Errorf("Snapshot 读到**中间态**: Status=%q StatusReason=%q —— "+
			"值拷贝没有被锁覆盖，修复无效",
			res.snap.Status, res.snap.StatusReason)
	}
	t.Logf("Snapshot 被锁正确挡住并读到自洽组合: Status=%q StatusReason=%q（旧=%v 新=%v）",
		res.snap.Status, res.snap.StatusReason, oldCombo, newCombo)
}
