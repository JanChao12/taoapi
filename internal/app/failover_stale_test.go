package app

import (
	"context"
	"errors"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider"
)

// ═══════════════════════════════════════════════════════════════════
// R1「其他旧结果提交」的护栏（2026-10-07 修，本轮补测试）
// ═══════════════════════════════════════════════════════════════════
//
// R1 有两个洞，都源于"UID-only + 无条件 Mutate"：
//
//	① **同 UID 重导入**：请求在途时账号被删除、又导入了同一个 UID
//	   （新对象、新版本），旧的失败记录/成功清除会写到**全新的账号**上。
//	② **旧成功抹掉更新的失败**：一次 chat 成功返回时，若期间账号已被
//	   别的来源标成异常（如并发额度刷新），无条件清除会把**更新的失败**
//	   抹掉 —— 用"基于旧前提的成功"覆盖新事实。
//
// 修法：两处都改走 `MutateIfRev(uid, baseRev, …)`，且 `baseRev` 必须是
// **发请求之前**取的基线（响应回来再读 Rev 等于没有保护）。
//
// 🔴 本文件用**确定性**构造，不靠"并发跑很多次碰运气"
//	（真并发抖动极大，本仓库已有 8/4073 vs 0 次的先例）。

// TestRecordFailureRejectedAfterReimportSameUID 守 R1①：
// 账号在请求在途期间被"删除后重导入同 UID"，旧的失败记录**不得**落到新账号上。
func TestRecordFailureRejectedAfterReimportSameUID(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-r1", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	st := fx.store

	// ① 请求开始：取版本基线（模拟 failover 里的 snap.Rev）
	baseRev, ok := st.Rev("uid-r1")
	if !ok {
		t.Fatal("前置条件不成立：账号应存在")
	}

	// ② 请求在途期间：账号被删除，又导入同一个 UID（新对象 ⇒ 新版本）
	st.Remove("uid-r1")
	st.Put(&auth.Account{
		UID: "uid-r1", Nickname: "重导入",
		Credit: auth.CreditSnapshot{Known: true, Remaining: 100},
	})

	// ③ 旧请求的失败记录回来了 —— 必须被**拒绝**
	recordAccountFailure(fx.deps(), "uid-r1", baseRev, errors.New("限流"))

	after, _ := st.Snapshot("uid-r1")
	if after.StatusReason != "" || after.LastError != "" || after.Status.Normalize() != pool.StatusNormal {
		t.Fatalf("🔴 R1① 复现：重导入的新账号被旧失败记录污染 —— "+
			"Status=%q Reason=%q LastError=%q（期望全部为空/normal）",
			after.Status, after.StatusReason, after.LastError)
	}
}

// TestClearFailureRejectedAfterReimportSameUID 守 R1①的另一半：
// 旧请求的"成功清除"同样不得作用到重导入的新账号。
func TestClearFailureRejectedAfterReimportSameUID(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-r1b", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	st := fx.store
	baseRev, _ := st.Rev("uid-r1b")

	// 在途期间：删除 + 重导入，且新账号**带着**一个异常状态
	st.Remove("uid-r1b")
	st.Put(&auth.Account{
		UID: "uid-r1b", Nickname: "重导入",
		Status:       pool.StatusRateLimited,
		StatusReason: "上游限流",
		LastError:    "上游限流",
		Credit:       auth.CreditSnapshot{Known: true, Remaining: 100},
	})

	// 旧请求的成功回来了 —— 不得清掉新账号的异常状态
	clearAccountFailure(fx.deps(), "uid-r1b", baseRev)

	after, _ := st.Snapshot("uid-r1b")
	if after.Status.Normalize() != pool.StatusRateLimited {
		t.Fatalf("🔴 R1① 复现：重导入账号的异常状态被旧成功抹掉 —— "+
			"Status=%q（期望 rate_limited）", after.Status)
	}
}

// TestClearFailureRejectedWhenNewerFailureExists 守 R1②：
// 账号在请求在途期间被标成异常 ⇒ 旧的成功**不得**清除这个更新的失败。
//
// 这是最隐蔽的一个洞：它不是"覆盖数据"，而是"用旧前提的成功抹掉新事实"。
func TestClearFailureRejectedWhenNewerFailureExists(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-r1c", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	st := fx.store

	// ① chat 请求开始，取基线（此时账号正常）
	baseRev, _ := st.Rev("uid-r1c")

	// ② 请求在途期间，另一个来源（如并发额度刷新）把它标成异常
	//    —— 用 Mutate，等价于生产里的 recordAccountFailure/额度刷新写点
	st.Mutate("uid-r1c", func(a *auth.Account) bool {
		a.Status = pool.StatusRateLimited
		a.StatusReason = "上游限流"
		a.LastError = "上游限流"
		return true
	})
	newRev, _ := st.Rev("uid-r1c")
	if newRev == baseRev {
		t.Fatal("前置条件不成立：期间状态变更应推进版本")
	}

	// ③ 那次 chat 的成功回来了 —— 不得清掉更新的失败
	clearAccountFailure(fx.deps(), "uid-r1c", baseRev)

	after, _ := st.Snapshot("uid-r1c")
	if after.Status.Normalize() != pool.StatusRateLimited || after.LastError == "" {
		t.Fatalf("🔴 R1② 复现：旧成功抹掉了更新的失败 —— "+
			"Status=%q LastError=%q（期望仍是 rate_limited 且带原因）",
			after.Status, after.LastError)
	}
}

// TestClearFailureCommitsWhenVersionMatches 是**正向**对照：
// 期间没有任何变更时，成功清除**必须**照常生效。
//
// 没有这条，一个"永远拒绝提交"的错误实现也能让上面三条测试全绿。
func TestClearFailureCommitsWhenVersionMatches(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-r1d", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	st := fx.store

	// 先造一个失败痕迹
	st.Mutate("uid-r1d", func(a *auth.Account) bool {
		a.Status = pool.StatusRateLimited
		a.StatusReason = "上游限流"
		a.LastError = "上游限流"
		return true
	})

	// 之后才开始请求 ⇒ 基线与当前一致
	baseRev, _ := st.Rev("uid-r1d")
	clearAccountFailure(fx.deps(), "uid-r1d", baseRev)

	after, _ := st.Snapshot("uid-r1d")
	if after.Status.Normalize() != pool.StatusNormal || after.LastError != "" {
		t.Fatalf("失败：版本相符时成功清除应生效 —— Status=%q LastError=%q",
			after.Status, after.LastError)
	}
	// 清掉人工禁用的顾虑：这里没设过，保持 false
	if after.ManualDisabled {
		t.Error("不该改动 ManualDisabled")
	}
}

// TestRecordFailureStillDecidesFailoverOnStale 守"修 R1 没有破坏换号语义"：
//
//	**换号决策依赖本次错误本身，与回写是否被接受无关。**
//	若把两者绑在一起，一次版本抖动就会让本该换号的请求直接失败
//	（用户看到报错，而其实换一个号就能成功）—— 那是比原缺陷更糟的退化。
func TestRecordFailureStillDecidesFailoverOnStale(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-r1e", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	st := fx.store
	baseRev, _ := st.Rev("uid-r1e")

	// 让版本失效
	st.Remove("uid-r1e")
	st.Put(&auth.Account{UID: "uid-r1e", Nickname: "重导入"})

	// 限流类错误 ⇒ 回写被拒，但**仍应返回"要换号"**
	got := recordAccountFailure(fx.deps(), "uid-r1e", baseRev,
		errors.New("429 too many requests: rate limit exceeded"))
	if !got {
		t.Fatal("🔴 版本不匹配时误判为「不用换号」—— " +
			"换号决策应与回写是否被接受解耦（本次是限流，应当换号）")
	}
}

// ═══════════════════════════════════════════════════════════════════
// R1 的**端到端**验证（走真实 TryChat 调用链）
// ═══════════════════════════════════════════════════════════════════
//
// 上面几条在 store 层构造窗口，能验证"条件提交本身生效"，
// 但**验不到调用方是否真的传了「发请求之前」的基线** ——
// 而那正是 Codex 特别点名的约束。
//
// 下面用 scriptedChatter 的 onCall 钩子在**真实请求在途窗口**里
// 制造"删除 + 重导入同 UID"，端到端证明旧结果不会污染新账号。

// TestTryChatFailureDoesNotPolluteReimportedAccount 端到端守 R1①：
// 一次 chat 失败返回时，若该 UID 已被删除并重导入，
// 失败记录**不得**写到新账号上。
func TestTryChatFailureDoesNotPolluteReimportedAccount(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-e2e")

	chatter := newScriptedChatter()
	chatter.set("uid-e2e", &acctBehavior{
		failWith: rateLimitedErr{},
		// 请求在途窗口：删除 + 重导入同 UID（新对象、新版本）
		onCall: func() {
			store.Remove("uid-e2e")
			store.Put(&auth.Account{
				UID:      "uid-e2e",
				Nickname: "重导入",
				Credit:   auth.CreditSnapshot{Known: true, Remaining: 100},
			})
		},
	})

	_, _ = TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/x"})

	after, ok := store.Snapshot("uid-e2e")
	if !ok {
		t.Fatal("重导入的账号应存在")
	}
	if after.StatusReason != "" || after.LastError != "" {
		t.Fatalf("🔴 R1① 端到端复现：重导入的新账号被旧失败记录污染 —— "+
			"Status=%q Reason=%q LastError=%q",
			after.Status, after.StatusReason, after.LastError)
	}
}

// TestTryChatSuccessDoesNotClearNewerFailure 端到端守 R1②：
// 一次 chat **成功**返回时，若期间账号已被标成异常，
// 不得把那个更新的失败抹掉。
func TestTryChatSuccessDoesNotClearNewerFailure(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-e2e2")

	chatter := newScriptedChatter()
	chatter.set("uid-e2e2", &acctBehavior{
		events: textEvents("PONG"),
		// 请求在途窗口：另一个来源把它标成异常（如并发额度刷新）
		onCall: func() {
			store.Mutate("uid-e2e2", func(a *auth.Account) bool {
				a.Status = pool.StatusRateLimited
				a.StatusReason = "上游限流"
				a.LastError = "上游限流"
				return true
			})
		},
	})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/x"})
	if err != nil {
		t.Fatalf("本次 chat 应成功返回（失败只是回写被拒）：%v", err)
	}
	if sess != nil {
		defer sess.Close()
	}

	after, _ := store.Snapshot("uid-e2e2")
	if after.Status.Normalize() != pool.StatusRateLimited || after.LastError == "" {
		t.Fatalf("🔴 R1② 端到端复现：旧成功抹掉了更新的失败 —— "+
			"Status=%q LastError=%q（期望仍是 rate_limited）",
			after.Status, after.LastError)
	}
}
