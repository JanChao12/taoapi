package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 条件提交（CAS）与版本机制的契约测试
//
// Codex 第 53 轮指出：我原先的 4 个测试**没有直接覆盖**这些关键契约：
//   · 版本相符 → 提交成功
//   · 版本变化 → 提交被拒（stale）
//   · 账号被删除 → 不提交
//   · **同 UID 删除后重导入（ABA 碰撞）** → 必须仍被拒
//   · 两个刷新竞争 → 先提交者胜出
//   · 无变化的写**不推进版本**（否则会无谓作废其它在途提交）
// ═══════════════════════════════════════════════════════════════════

// TestMutateIfRevCommitsWhenVersionMatches 守：版本相符 ⇒ 提交成功。
func TestMutateIfRevCommitsWhenVersionMatches(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})

	rev, ok := st.Rev("u1")
	if !ok {
		t.Fatal("账号应存在")
	}

	committed, stale := st.MutateIfRev("u1", rev, func(a *Account) bool {
		a.LastError = "新的结果"
		return true
	})
	if !committed {
		t.Error("版本相符时应提交成功")
	}
	if stale {
		t.Error("版本相符时不应报 stale")
	}
	snap, _ := st.Snapshot("u1")
	if snap.LastError != "新的结果" {
		t.Errorf("写入未生效: %q", snap.LastError)
	}
}

// TestMutateIfRevRejectsStaleVersion 守：版本已变 ⇒ 拒绝提交（结果被丢弃）。
//
// 这是"结果时序"的核心：过期的成功结果**不得**抹掉更新的失败标记。
func TestMutateIfRevRejectsStaleVersion(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})

	// 异步请求取快照 + 版本基线
	rev, _ := st.Rev("u1")

	// 期间别的路径把账号标成异常（版本推进）
	st.Mutate("u1", func(a *Account) bool {
		a.Status = "rate_limited"
		a.StatusReason = "上游限流"
		return true
	})

	// 过期的"刷新成功"结果回来 —— 必须被拒
	committed, stale := st.MutateIfRev("u1", rev, func(a *Account) bool {
		a.Status = "normal"
		a.StatusReason = ""
		return true
	})
	if committed {
		t.Error("版本已变，过期结果不应被提交")
	}
	if !stale {
		t.Error("应报告 stale=true")
	}

	// 🔴 关键：新的异常标记必须**完好无损**
	snap, _ := st.Snapshot("u1")
	if snap.Status != "rate_limited" || snap.StatusReason != "上游限流" {
		t.Errorf("过期的成功结果抹掉了新异常: Status=%q Reason=%q",
			snap.Status, snap.StatusReason)
	}
}

// TestMutateIfRevRejectsAfterRemoval 守：账号已删除 ⇒ 不提交。
func TestMutateIfRevRejectsAfterRemoval(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})
	rev, _ := st.Rev("u1")

	st.Remove("u1")

	committed, stale := st.MutateIfRev("u1", rev, func(a *Account) bool {
		a.LastError = "不该写进去"
		return true
	})
	if committed {
		t.Error("账号已删除，不应提交成功")
	}
	if stale {
		t.Error("账号不存在不是 stale，应返回 (false,false)")
	}
	if _, ok := st.Get("u1"); ok {
		t.Error("账号不该被复活")
	}
}

// TestMutateIfRevRejectsABAOnReimport 守：**同 UID 删除后重导入**不得被旧结果污染。
//
// 🔴 这是 Codex 第 53 轮指出的 ABA 碰撞：
//
//	若版本存在 Account 上、新对象从 0 开始数，那么
//	  "取基线(Rev=N) → 删除 → 重导入同 UID → 又改 N 次" 之后，
//	  旧请求的 Rev 会**再次匹配**，从而污染全新账号。
//
//	⇒ 版本必须来自**仓库级、永不复用**的序列。
//	  本测试就是把上面那条路径**逐字复现**出来。
func TestMutateIfRevRejectsABAOnReimport(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})

	// ① 旧请求取基线
	oldRev, _ := st.Rev("u1")

	// ② 删除
	st.Remove("u1")

	// ③ 重新导入同一个 UID
	st.Put(&Account{UID: "u1"})

	// ④ 🔴 **立刻**断言：重导入后版本就不再等于旧基线。
	//
	//	Codex 第 53 轮要求：必须在"重导入后、额外修改前"断言 ——
	//	否则"改 20 次之后不同"不能证明**重导入本身**避免了碰撞
	//	（那可能只是被那 20 次修改推开的）。
	newRev, ok := st.Rev("u1")
	if !ok {
		t.Fatal("重导入后账号应存在")
	}
	if newRev == oldRev {
		t.Fatalf("重导入后版本 %d 仍等于旧基线 %d —— 版本序列在重导入时被复用，"+
			"存在 ABA 碰撞（旧请求会污染全新账号）", newRev, oldRev)
	}

	// ⑤ 再做若干次修改，确认版本持续前进且**始终**不复用旧值
	for i := 0; i < 20; i++ {
		st.Mutate("u1", func(a *Account) bool {
			a.LastError = "新账号的变更"
			return true
		})
	}
	if mid, _ := st.Rev("u1"); mid == oldRev {
		t.Fatalf("多轮修改后版本回到旧基线 %d —— 序列回绕/复用", oldRev)
	}

	// ⑥ 旧结果回来 —— 必须被拒
	committed, stale := st.MutateIfRev("u1", oldRev, func(a *Account) bool {
		a.LastError = "旧请求污染了全新账号"
		return true
	})
	if committed {
		t.Error("旧请求的版本基线不该匹配重导入后的账号（ABA）")
	}
	if !stale {
		t.Error("应报告 stale=true")
	}
	snap, _ := st.Snapshot("u1")
	if snap.LastError == "旧请求污染了全新账号" {
		t.Error("新账号被旧请求污染了")
	}
}

// TestMutateIfRevConcurrentRefreshes 守：两个刷新竞争 ⇒ **先提交者胜出**，后者被拒。
//
// ⚠️ 语义澄清（Codex 要求）：全局版本保证的是"先提交者胜出"，
//
//	**不是**"后发请求胜出"，也**不是**"上游数据更新者胜出"。
//	这条测试把该语义固化下来，避免后来者误以为它是全新的。
func TestMutateIfRevConcurrentRefreshes(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})

	// 两个请求都取了**同一个**基线（真实并发下的常见情形）
	revA, _ := st.Rev("u1")
	revB := revA

	// A 先提交
	okA, staleA := st.MutateIfRev("u1", revA, func(a *Account) bool {
		a.Credit.Remaining = 111
		return true
	})
	// B 后提交 —— 基线已过期
	okB, staleB := st.MutateIfRev("u1", revB, func(a *Account) bool {
		a.Credit.Remaining = 222
		return true
	})

	if !okA || staleA {
		t.Error("先提交者应成功")
	}
	if okB {
		t.Error("后提交者基线已过期，应被拒")
	}
	if !staleB {
		t.Error("后提交者应报告 stale")
	}
	snap, _ := st.Snapshot("u1")
	if snap.Credit.Remaining != 111 {
		t.Errorf("最终值 = %d，期望 111（先提交者胜出）", snap.Credit.Remaining)
	}
}

// TestMutateNoChangeDoesNotBumpRev 守：**无变化的写不推进版本**。
//
// 🔴 Codex 第 53 轮指出我第一版的错：我写成无条件 `a.Rev++`。
//
//	后果：clearAccountFailure 在"本来就没有失败痕迹"时走 return false，
//	却仍推进版本 ⇒ **无谓作废一次正常的额度刷新结果**。
//
//	这条测试直接断言"不改就不动版本"。
func TestMutateNoChangeDoesNotBumpRev(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "u1"})

	before, _ := st.Rev("u1")

	changed := st.Mutate("u1", func(a *Account) bool {
		return false // 声明"什么都没改"
	})
	if changed {
		t.Error("回调返回 false 时 Mutate 应返回 false")
	}
	after, _ := st.Rev("u1")
	if after != before {
		t.Errorf("无变化的写推进了版本: %d → %d —— "+
			"这会让其它在途的条件提交被无谓丢弃", before, after)
	}

	// 对照：真的改了就必须推进
	st.Mutate("u1", func(a *Account) bool {
		a.LastError = "改了"
		return true
	})
	bumped, _ := st.Rev("u1")
	if bumped == before {
		t.Error("有变化的写必须推进版本")
	}
}

// TestRevNotPersistedToDisk 守：`Account.Rev` **不得落盘**。
//
// 🔴 Codex 第 53 轮指出：`json:"-"` 只证明"直接编码 Account 时会被忽略"，
//
//	**不能**证明实际编码的 `diskAcct` 也会忽略它。
//	⇒ 必须做一次真实的落盘往返，检查磁盘上**没有**版本字段。
func TestRevNotPersistedToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	p := NewPersister(path, xorCodec{key: 0x5A})

	st := NewStore()
	st.Put(&Account{
		UID:      "u1",
		Nickname: "测试号",
		Credit:   CreditSnapshot{Known: true, Remaining: 802},
	})
	// 推进版本到非零
	for i := 0; i < 5; i++ {
		st.Mutate("u1", func(a *Account) bool {
			a.LastError = "x"
			return true
		})
	}
	if rev, _ := st.Rev("u1"); rev == 0 {
		t.Fatal("版本应已推进（前置条件）")
	}

	if err := p.Save(st); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	// 磁盘原文里不得出现版本字段
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{`"rev"`, `"revseq"`} {
		if strings.Contains(lower, banned) {
			t.Errorf("磁盘上出现了版本字段 %s —— 版本不该持久化", banned)
		}
	}

	// 重新加载后能正常做条件提交（版本从 0 起算，但不影响正确性）
	loaded, err := p.Load()
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	rev, ok := loaded.Rev("u1")
	if !ok {
		t.Fatal("账号应被还原")
	}
	committed, stale := loaded.MutateIfRev("u1", rev, func(a *Account) bool {
		a.LastError = "重启后的写入"
		return true
	})
	if !committed || stale {
		t.Errorf("重载后条件提交应成功: committed=%v stale=%v", committed, stale)
	}
}

// （测试 codec 用本包既有的 xorCodec，见 auth_test.go —— 不另造。）
