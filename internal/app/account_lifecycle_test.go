package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// ═══════════════════════════════════════════════════════════════════
// 账号调度生命周期：用**真实 HTTP + 真实落盘**回答委托方四问
//
// 委托方原话（2026-10-07）：
//
//	"你真的能识别出哪个账号的积分是最快过期从而使用此账号吗"
//	"测试将目前使用账号标记为限额等异常状态看看项目能不能识别到
//	  从而自动切换账号反代使用"
//	"你真的能从上游识别到我账号是正常状态还是异常状态吗，
//	  如果你识别到后会将账号异常在项目中真实标记吗"
//	"当账号在官方平台解除异常恢复正常状态后你项目能识别到
//	  并且取消异常状态标记吗"
//
// ⚠️ 本文件的测试**不 mock 落盘**：用真实 auth.Persister 写真实文件，
//	必要时读回来验证"标记是否真的持久化了"（重启后是否还在）。
// ═══════════════════════════════════════════════════════════════════

// accountFixture 一个可控的账号集合 + 落盘器。
type accountFixture struct {
	t         *testing.T
	store     *auth.Store
	persister *auth.Persister
	dir       string
}

// newAccountFixture 建 N 个账号，各有指定到期日与额度，并落盘。
//
// packages 形如 {"2026-10-07", 802} —— 用于验证"谁最快过期"。
func newAccountFixture(t *testing.T, specs []acctSpec) *accountFixture {
	t.Helper()
	dir := t.TempDir()
	cleanupTempDirWithRetry(t, dir)

	persister := auth.NewPersister(filepath.Join(dir, "accounts.json"), storageCodec())
	store := auth.NewStore()
	for i, s := range specs {
		plat := s.platform
		if plat == "" {
			plat = auth.PlatformCN
		}
		store.Put(&auth.Account{
			UID:         s.uid,
			Nickname:    fmt.Sprintf("账号%d", i+1),
			AccessToken: "token-" + s.uid,
			Platform:    plat,
			Status:      pool.StatusNormal,
			Credit: auth.CreditSnapshot{
				Known:     true,
				Remaining: s.total,
				Packages:  s.packages,
			},
		})
	}
	if err := persister.Save(store); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	return &accountFixture{t: t, store: store, persister: persister, dir: dir}
}

type acctSpec struct {
	uid      string
	platform string
	total    int64
	packages []auth.PackageSnapshot
}

// deps 组装一个带真实落盘器的 Deps。
func (f *accountFixture) deps() Deps {
	return Deps{
		Accounts:  f.store,
		Persister: f.persister,
		Logger:    testLogger(f.t),
	}
}

// reload 从磁盘重新读回（验证"标记是否真的持久化"）。
func (f *accountFixture) reload() *auth.Store {
	f.t.Helper()
	st, err := f.persister.Load()
	if err != nil {
		f.t.Fatalf("重新加载失败: %v", err)
	}
	return st
}

// ═══════════════════════════════════════════════════════════════════
// 问题 1：能识别"积分最快过期"的账号并优先用它吗？
// ═══════════════════════════════════════════════════════════════════

// TestPicksEarliestExpiringAccountEndToEnd 守：真的会优先用**最早到期**的号。
//
// 构造三账号，让"最早到期"的那个**既不是额度最多的、也不是列表里第一个**，
// 这样如果实现退化成"按额度排"或"按顺序取"，测试就会红。
//
// ⚠️ 覆盖边界（Codex 第 52 轮指出：仅这一条不足以证明规则正确）：
//
//	本 fixture 仍容许"选额度最少"这种坏实现通过 —— 因为 uid-soon 恰好额度最少。
//	所以下面**必须**配套三条反例测试，分别打掉"按额度""按顺序""按临期总量"三种退化。
func TestPicksEarliestExpiringAccountEndToEnd(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		// 额度最多，但到期最晚 —— 不该被选
		{uid: "uid-late", total: 9999, packages: []auth.PackageSnapshot{
			{Name: "大包", Remain: 9999, ExpireAt: "2026-12-31"},
		}},
		// 额度中等，到期居中
		{uid: "uid-mid", total: 5000, packages: []auth.PackageSnapshot{
			{Name: "中包", Remain: 5000, ExpireAt: "2026-11-01"},
		}},
		// 额度最少，但【最早到期】—— 应该被选
		{uid: "uid-soon", total: 802, packages: []auth.PackageSnapshot{
			{Name: "临期包", Remain: 802, ExpireAt: "2026-10-07"},
		}},
	})

	got := pickForTest(t, fx, "cn")
	if got != "uid-soon" {
		t.Errorf("选中 %q，期望 uid-soon（到期 2026-10-07 最早）", got)
	}
}

// TestEarliestWinsEvenWhenItHasMostCredits 守：**打掉"按额度排序"的退化**。
//
// 与上一条互补：这条让"最早到期"的账号**同时额度最多**，
// 于是"按额度排"也能通过上一条、但**通不过**这一条的对称变体 ——
// 两条合起来才能排除"按额度"这个替代解释。
//
// 做法：保持到期顺序不变，只交换额度大小。
func TestEarliestWinsEvenWhenItHasMostCredits(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		// 最早到期，且额度**最多**
		{uid: "uid-soon-rich", total: 9999, packages: []auth.PackageSnapshot{
			{Name: "临期大包", Remain: 9999, ExpireAt: "2026-10-07"},
		}},
		{uid: "uid-late-poor", total: 10, packages: []auth.PackageSnapshot{
			{Name: "远期小包", Remain: 10, ExpireAt: "2026-12-31"},
		}},
	})
	if got := pickForTest(t, fx, "cn"); got != "uid-soon-rich" {
		t.Errorf("选中 %q，期望 uid-soon-rich（最早到期，额度也最多）", got)
	}
}

// TestEarliestWinsRegardlessOfInputOrder 守：**打掉"按输入顺序取"的退化**。
//
// 把同一组账号以不同顺序喂给 selector，结果必须一致。
func TestEarliestWinsRegardlessOfInputOrder(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-a-late", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-12-31"},
		}},
		{uid: "uid-b-soon", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
		{uid: "uid-c-mid", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-11-01"},
		}},
	})

	now := time.Now()
	var all []pool.Account
	for _, a := range fx.store.List() {
		all = append(all, a.ToPool(now))
	}

	// 正序与逆序都必须选同一个
	reversed := make([]pool.Account, len(all))
	for i := range all {
		reversed[i] = all[len(all)-1-i]
	}

	sel := pool.NewSelector()
	// 固定时钟，避免"2026-10-07 是否已过"的当日边界歧义
	fixed := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sel.SetClock(func() time.Time { return fixed })

	a1, err1 := sel.PickForPlatform(all, "cn")
	a2, err2 := sel.PickForPlatform(reversed, "cn")
	if err1 != nil || err2 != nil {
		t.Fatalf("都应选出账号: %v / %v", err1, err2)
	}
	if a1.ID != "uid-b-soon" || a2.ID != "uid-b-soon" {
		t.Errorf("正序选 %q、逆序选 %q，都应选 uid-b-soon —— "+
			"若随顺序变化，说明实现依赖输入顺序", a1.ID, a2.ID)
	}
}

// TestEarliestPackageNotExpiringTotal 守：用的是**账号内最早到期的那个包**，
// **不是**"未来 N 天内的临期额度总量"。
//
// 🔴 这是委托方明确否决过的一个方案（`pool.go` 包注释引了他的原话）：
//
//	"他是设置几天内的临期额度量排序，我是要求只看各个账号下哪个包过期就优先哪个账号"
//
// ⚠️ 设计要点（第一版我写错过，Codex 第 52 轮抓到）：
//
//	初版声称"uid-spread 最早包在 12 天后"，但同账号里放了 10-11 的包 ——
//	**自相矛盾**：既然同账号有更早的可用包，它的"最早到期"就是那个，
//	不可能同时在窗口外。那样的 fixture **没有干净地区分两种策略**。
//
//	正确构造（两种策略给出**相反**答案）：
//	  · uid-spread：**最早**包在第 4 天（10-11, 400），另有第 5 天（10-12, 500）
//	    ⇒ 最早到期 = 10-11；7 天窗口内总量 = 900（最大）
//	  · uid-single：**最早**包在第 2 天（10-09, 100）
//	    ⇒ 最早到期 = 10-09；窗口内总量 = 100（最小）
//
//	⇒ 按"最早到期"选 uid-single；按"窗口内总量"选 uid-spread。
//	  两个判据给出**相反**结果，测试才有区分力。
func TestEarliestPackageNotExpiringTotal(t *testing.T) {
	fx2 := newAccountFixture(t, []acctSpec{
		// 最早到期 = 10-11（第 4 天）；窗口内两个包 400+500 = 900 ⇒ 总量最大
		{uid: "uid-spread", total: 900, packages: []auth.PackageSnapshot{
			{Name: "包A", Remain: 400, ExpireAt: "2026-10-11"},
			{Name: "包B", Remain: 500, ExpireAt: "2026-10-12"},
		}},
		// 最早到期 = 10-09（第 2 天）；窗口内只有 100 ⇒ 总量最小
		{uid: "uid-single", total: 100, packages: []auth.PackageSnapshot{
			{Name: "唯一包", Remain: 100, ExpireAt: "2026-10-09"},
		}},
	})

	sel := pool.NewSelector()
	// 固定到 2026-10-07，让"7 天窗口"边界明确
	fixed := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	sel.SetClock(func() time.Time { return fixed })

	// 先自证 fixture 的区分力：两种策略必须给出**相反**答案。
	// （否则测试会"通过但没测到东西" —— 第一版就是这样。）
	expiringSpread, expiringSingle := int64(0), int64(0)
	{
		var accts []pool.Account
		for _, a := range fx2.store.List() {
			accts = append(accts, a.ToPool(fixed))
		}
		horizon := pool.DisplayHorizon
		for _, a := range accts {
			switch a.ID {
			case "uid-spread":
				expiringSpread = a.ExpiringWithin(horizon, fixed)
			case "uid-single":
				expiringSingle = a.ExpiringWithin(horizon, fixed)
			}
		}
	}
	if expiringSpread <= expiringSingle {
		t.Fatalf("fixture 无区分力：临期总量 spread=%d 未大于 single=%d —— "+
			"两种策略若给出同向答案，本测试就证明不了任何事",
			expiringSpread, expiringSingle)
	}

	var accts []pool.Account
	for _, a := range fx2.store.List() {
		accts = append(accts, a.ToPool(fixed))
	}
	got, err := sel.PickForPlatform(accts, "cn")
	if err != nil {
		t.Fatalf("应选出账号: %v", err)
	}
	if got.ID != "uid-single" {
		t.Errorf("选中 %q，期望 uid-single（最早到期 10-10）—— "+
			"若选到 uid-spread，说明退化成了「7 天内临期额度总量」排序，"+
			"那是委托方明确否决的方案", got.ID)
	}
}

// TestEarliestHandlesMultiplePackagesWithinAccount 守：多包账号取**包级最早**。
//
// 账号级只有一个"到期"概念是错的 —— 必须下钻到包。
func TestEarliestHandlesMultiplePackagesWithinAccount(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		// 账号总到期"看起来"很晚（有个 12-31 的大包），但内部有个 10-08 的小包
		{uid: "uid-multi", total: 5000, packages: []auth.PackageSnapshot{
			{Name: "大包", Remain: 4900, ExpireAt: "2026-12-31"},
			{Name: "小包", Remain: 100, ExpireAt: "2026-10-08"},
		}},
		// 只有一个包，到期 10-20
		{uid: "uid-single", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-20"},
		}},
	})
	if got := pickForTest(t, fx, "cn"); got != "uid-multi" {
		t.Errorf("选中 %q，期望 uid-multi —— 它内部有 10-08 的小包，"+
			"比 uid-single 的 10-20 更早；若选了 uid-single 说明没下钻到包级", got)
	}
}

// TestTieOnEarliestExpiryBreaksBySameDateRemain 守：并列到期时的 tie-break。
//
// 规则（pool.go 包注释）：同到期日 → **该日期的包**剩余量之和降序。
// ⚠️ 只统计"恰好等于该日期"的包，不能把其他日期的额度也算进来。
func TestTieOnEarliestExpiryBreaksBySameDateRemain(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		// 同为 10-11 到期，但当日额度 100
		{uid: "uid-tie-small", total: 5000, packages: []auth.PackageSnapshot{
			{Name: "当日小", Remain: 100, ExpireAt: "2026-10-11"},
			{Name: "其他日大", Remain: 4900, ExpireAt: "2026-12-31"},
		}},
		// 同为 10-11 到期，当日额度 300 ⇒ 应胜出
		{uid: "uid-tie-big", total: 300, packages: []auth.PackageSnapshot{
			{Name: "当日大", Remain: 300, ExpireAt: "2026-10-11"},
		}},
	})
	if got := pickForTest(t, fx, "cn"); got != "uid-tie-big" {
		t.Errorf("选中 %q，期望 uid-tie-big（同为 10-11 到期，当日额度 300 > 100）—— "+
			"若选了 uid-tie-small，说明 tie-break 错用了账号总额度（5000）"+
			"而不是「该到期日的额度之和」", got)
	}
}

// pickForTest 用**固定时钟**选出账号 ID。
//
// 🔴 固定时钟的理由（Codex 第 52 轮）：用 time.Now() 会让"2026-10-07 是否已过"
//
//	成为当日边界歧义，测试在 10-07 当天与之后行为不同 ⇒ 不稳定。
func pickForTest(t *testing.T, fx *accountFixture, plat string) string {
	t.Helper()
	sel := pool.NewSelector()
	fixed := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sel.SetClock(func() time.Time { return fixed })

	var accts []pool.Account
	for _, a := range fx.store.List() {
		accts = append(accts, a.ToPool(fixed))
	}
	got, err := sel.PickForPlatform(accts, plat)
	if err != nil {
		t.Fatalf("应选出账号: %v", err)
	}
	return got.ID
}

// TestEarliestExpiryIgnoresZeroRemainPackages 守：零额度包不参与"最早到期"。
//
// 🔴 为什么重要：一个已用尽的旧包若被算进去，会把账号错误地排到最前，
//
//	白白浪费一次调度（选了它，但它其实没额度可用）。
func TestEarliestExpiryIgnoresZeroRemainPackages(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		// 有一个"早就过期但额度为 0"的包，不该被当成本账号的最早到期日
		{uid: "uid-zero-old", total: 3000, packages: []auth.PackageSnapshot{
			{Name: "用尽的旧包", Remain: 0, ExpireAt: "2026-01-01"},
			{Name: "可用包", Remain: 3000, ExpireAt: "2026-12-31"},
		}},
		// 真正最早到期的号
		{uid: "uid-real-soon", total: 100, packages: []auth.PackageSnapshot{
			{Name: "临期包", Remain: 100, ExpireAt: "2026-11-01"},
		}},
	})

	sel := pool.NewSelector()
	now := time.Now()
	var accts []pool.Account
	for _, a := range fx.store.List() {
		accts = append(accts, a.ToPool(now))
	}
	got, err := sel.PickForPlatform(accts, "cn")
	if err != nil {
		t.Fatalf("应选出账号: %v", err)
	}
	if got.ID != "uid-real-soon" {
		t.Errorf("选中 %q，期望 uid-real-soon —— 零额度的旧包（2026-01-01）"+
			"不该被当成本账号的最早到期日", got.ID)
	}
}

// ═══════════════════════════════════════════════════════════════════
// 问题 2：把当前账号标记为异常，能自动切换吗？
// ═══════════════════════════════════════════════════════════════════

// TestRateLimitedAccountIsSwitchedAndPersisted 守：限流 → 换号 + **真实落盘**。
//
// 这是委托方问题的核心：模拟"当前账号被限流"，看是否真的换到另一个号。
// 额外验证：标记**写进了磁盘**（重启后仍生效，不会"复活"）。
func TestRateLimitedAccountIsSwitchedAndPersisted(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-first", total: 100, packages: []auth.PackageSnapshot{
			{Name: "临期", Remain: 100, ExpireAt: "2026-10-07"}, // 会最先被选
		}},
		{uid: "uid-backup", total: 900, packages: []auth.PackageSnapshot{
			{Name: "后备用", Remain: 900, ExpireAt: "2026-12-31"},
		}},
	})
	deps := fx.deps()

	chatter := newScriptedChatter()
	chatter.set("uid-first", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-backup", &acctBehavior{events: textEvents("来自备用号")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatalf("限流后应自动换号成功，实际失败: %v", err)
	}
	defer sess.Close()

	if sess.Account == nil || sess.Account.UID != "uid-backup" {
		t.Fatalf("应换到 uid-backup，实际 %v", sess.Account)
	}

	// ── 关键：标记真的落盘了吗？──
	//
	// 🔴 2026-10-09 改为**按模型**限流后，断言点从 StatusUntil 移到了
	//	ModelCooldowns —— 限流不再是"账号的问题"，而是"这个账号的这个
	//	模型"的问题。委托方实测：DeepSeek 被限后切 glm 仍可用。
	reloaded := fx.reload()
	got, ok := reloaded.Get("uid-first")
	if !ok {
		t.Fatal("重新加载后 uid-first 不见了")
	}
	// 落盘的模型冷却必须含**本次请求的模型**
	if len(got.ModelCooldowns) == 0 {
		t.Error("落盘后 ModelCooldowns 为空 —— 模型级冷却没有持久化，" +
			"重启后会立刻再撞同一个模型的 429")
	} else if until, has := got.ModelCooldowns["workbuddy/space-bunny"]; !has || until.IsZero() {
		t.Errorf("ModelCooldowns 里没有本次限流的模型 workbuddy/space-bunny（实际 %v）—— "+
			"必须按模型记，否则该账号其他可用模型会被一起冻住", got.ModelCooldowns)
	}
	// 账号本身**不该**被标成限流（它只是这一个模型受限）
	if got.Status.Normalize() == pool.StatusRateLimited {
		t.Error("账号级状态被标成 rate_limited —— 按模型限流后，账号本身应保持正常，" +
			"否则面板会显示误导性的「限流」，且其他模型也被挡掉")
	}
	if !got.StatusUntil.IsZero() {
		t.Errorf("账号级 StatusUntil = %v，期望零值 —— 模型级限流不该设账号级冷却",
			got.StatusUntil)
	}
	// 原因要能说清是哪个模型（面板展示用）
	if !strings.Contains(got.StatusReason, "space-bunny") {
		t.Errorf("StatusReason = %q，期望点名被限流的模型（面板要显示）", got.StatusReason)
	}
}

// TestLimiterRecoversAfterCooldown 守：冷却到期后**自动**回到可调度。
//
// 委托方问"识别到异常后会不会标记"，反面同样重要：
// 标记会不会**永远摘不掉**导致账号被永久废掉。
func TestLimiterRecoversAfterCooldown(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-cooling", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})

	// 模拟一次限流
	acct, _ := fx.store.Get("uid-cooling")
	acct.Status = pool.StatusRateLimited
	acct.StatusReason = "上游限流"
	acct.StatusUntil = time.Now().Add(60 * time.Second)

	// 冷却中：不可调度
	sel := pool.NewSelector()
	base := time.Now()
	sel.SetClock(func() time.Time { return base })
	if _, err := sel.PickForPlatform([]pool.Account{acct.ToPool(base)}, "cn"); err == nil {
		t.Error("冷却中不该被选中")
	}

	// 冷却已过：应恢复
	//
	// ⚠️ 必须**同时**推进 ToPool 的 now 与 selector 的时钟：
	//	ToPool(now) 只影响 Status 的计算，CooldownUntil 是原样拷贝的，
	//	由 selector 拿自己的时钟去比较。两个时钟不一致就会得到
	//	"状态已恢复但冷却未过"的矛盾视图（生产里两者都是 time.Now()，
	//	所以只是测试要注意；但这也说明 ToPool 的 now 参数容易被误用）。
	future := base.Add(2 * time.Minute)
	sel.SetClock(func() time.Time { return future })
	if _, err := sel.PickForPlatform([]pool.Account{acct.ToPool(future)}, "cn"); err != nil {
		t.Errorf("冷却已过应恢复可调度，实际: %v", err)
	}
	// 且有效状态回到正常
	if got := acct.EffectiveStatus(future); got != pool.StatusNormal {
		t.Errorf("冷却过后有效状态 = %q，期望 normal", got)
	}
}

// ═══════════════════════════════════════════════════════════════════
// 问题 3：真的能从上游识别"正常 vs 异常"并真实标记吗？
// ═══════════════════════════════════════════════════════════════════

// TestUpstreamStatusIsClassifiedAndRecorded 守：上游的各类错误被**分类并落盘**。
//
// 逐种上游异常跑一遍，断言：(a) 分类正确 (b) 真的写进磁盘 (c) 面板读得到。
//
// 🔴 2026-10-09 按模型限流后，**限流那一行的断言点变了**：
//
//	限流不再写账号级 Status/StatusUntil（那会把该账号其他仍可用的模型
//	一起冻住），而是写 ModelCooldowns —— 所以限流用例单独走
//	wantModelCooldown 分支，不再期待账号级状态变成 rate_limited。
func TestUpstreamStatusIsClassifiedAndRecorded(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantStatus   pool.Status
		wantCooldown bool
		// wantModelCooldown 表示限流：冷却记在**模型**上（账号级保持正常）
		wantModelCooldown bool
	}{
		{"限流429", rateLimitedErr{}, pool.StatusNormal, false, true},
		{"凭证401", authFailErr{}, pool.StatusAuthExpired, false, false},
		{"超时", context.DeadlineExceeded, pool.StatusTransient, true, false},
		{"未知错误", fmt.Errorf("某个没见过的东西"), pool.StatusTransient, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newAccountFixture(t, []acctSpec{
				{uid: "uid-x", total: 100, packages: []auth.PackageSnapshot{
					{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
				}},
			})
			deps := fx.deps()

			chatter := newScriptedChatter()
			chatter.set("uid-x", &acctBehavior{failWith: tc.err})

			_, _ = TryChat(context.Background(), deps, chatter,
				provider.ChatRequest{Model: "workbuddy/space-bunny"})

			reloaded := fx.reload()
			got, ok := reloaded.Get("uid-x")
			if !ok {
				t.Fatal("账号不见了")
			}
			if got.Status.Normalize() != tc.wantStatus {
				t.Errorf("落盘状态 = %q，期望 %q", got.Status.Normalize(), tc.wantStatus)
			}
			if got.StatusReason == "" {
				t.Error("StatusReason 为空 —— 面板无法解释'为什么异常'")
			}
			if tc.wantCooldown && got.StatusUntil.IsZero() {
				t.Error("应有冷却截止时间")
			}
			if !tc.wantCooldown && !got.StatusUntil.IsZero() {
				t.Error("这类异常不该有账号级冷却")
			}
			// 限流：冷却落在模型上
			if tc.wantModelCooldown {
				if len(got.ModelCooldowns) == 0 {
					t.Error("限流应记在 ModelCooldowns 上（按模型限流），实际为空")
				} else if until, has := got.ModelCooldowns["workbuddy/space-bunny"]; !has || until.IsZero() {
					t.Errorf("ModelCooldowns 缺少本次模型，实际 %v", got.ModelCooldowns)
				}
			} else if len(got.ModelCooldowns) != 0 {
				t.Errorf("非限流错误不该产生模型冷却，实际 %v", got.ModelCooldowns)
			}
		})
	}
}

// TestCoolingAccountStillSkippedAfterReload 守：**重启后**仍然跳过被限流的模型。
//
// 🔴 Codex 第 52 轮指出：只说"StatusUntil 非零"太弱 ——
//
//	过去的时间、错误时区、错误时长都能通过。
//	必须验证**重建 Store 与调度器后**该账号仍被跳过（这才是落盘的真正意义）。
//
// 🔴 2026-10-09 改为**按模型**限流后，本测试同时守住新能力的核心事实：
//
//	同一个账号，被限流的模型要跳过，**其他模型必须仍然可用**
//	（委托方实测：DeepSeek 被限后切 glm 能正常使用）。
func TestCoolingAccountStillSkippedAfterReload(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-first", total: 100, packages: []auth.PackageSnapshot{
			{Name: "临期", Remain: 100, ExpireAt: "2026-10-11"},
		}},
		{uid: "uid-backup", total: 900, packages: []auth.PackageSnapshot{
			{Name: "后备用", Remain: 900, ExpireAt: "2026-12-31"},
		}},
	})
	deps := fx.deps()

	const limitedModel = "workbuddy/space-bunny"
	const otherModel = "workbuddy/glm-5.3-flash"

	chatter := newScriptedChatter()
	chatter.set("uid-first", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-backup", &acctBehavior{events: textEvents("ok")})

	before := nowFunc()
	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: limitedModel})
	if err != nil {
		t.Fatalf("应换号成功: %v", err)
	}
	defer sess.Close()

	// ── 模型级冷却必须**合理**（未来 + 不超过限流冷却常量）──
	//
	// ⚠️ 上界从 rateLimitCooldown **推导**，不写死数字。
	//	原先写死 5 分钟；2026-10-09 把限流冷却从 60 秒改成 10 分钟后
	//	就假失败了。推导式让"改常量"不再需要同步改测试。
	reloaded := fx.reload()
	cooling, ok := reloaded.Get("uid-first")
	if !ok {
		t.Fatal("重新加载后账号不见了")
	}
	until, has := cooling.ModelCooldowns[limitedModel]
	if !has || until.IsZero() {
		t.Fatalf("落盘丢了模型冷却（ModelCooldowns=%v）—— 重启后无法自动恢复",
			cooling.ModelCooldowns)
	}
	if !until.After(before) {
		t.Errorf("冷却截止时间 %v 不在未来（现在 %v）—— 时区或时长写错了",
			until, before)
	}
	if d := until.Sub(before); d > rateLimitCooldown {
		t.Errorf("冷却时长 %v 超过限流冷却常量 %v —— 限流不该产生更长的冷却",
			d, rateLimitCooldown)
	}
	// 账号级状态必须保持正常（限流只针对那个模型）
	if cooling.Status.Normalize() == pool.StatusRateLimited {
		t.Error("账号级状态被标成 rate_limited —— 按模型限流后账号本身应正常")
	}

	// ── 关键：用**重新加载的** Store 重建调度，被限模型仍应跳过 ──
	sel := pool.NewSelector()
	sel.SetClock(nowFunc)
	now := nowFunc()
	var accts []pool.Account
	for _, a := range reloaded.List() {
		accts = append(accts, a.ToPool(now))
	}
	got, err := sel.PickForModel(accts, "cn", limitedModel)
	if err != nil {
		t.Fatalf("应有备用号可选: %v", err)
	}
	if got.ID != "uid-backup" {
		t.Errorf("重建后选中 %q，期望 uid-backup —— 冷却中的号重启后必须仍被跳过",
			got.ID)
	}

	// ── 🔴 新能力的核心：**其他模型仍然可用** ──
	//
	//	这是"账号+模型限流"区别于"账号级限流"的唯一实质差别，
	//	也是委托方实测确认过的上游行为（DeepSeek 被限 → 切 glm 可用）。
	gotOther, err := sel.PickForModel(accts, "cn", otherModel)
	if err != nil {
		t.Fatalf("其他模型应有账号可调度: %v", err)
	}
	if gotOther.ID != "uid-first" {
		t.Errorf("其他模型选中 %q，期望 uid-first ——\n"+
			"  被限流的只是 %s，该账号对 %s 仍应可用且优先级不变。\n"+
			"  若这里选到备用号，说明限流被错误地记成了**账号级**冷却。",
			gotOther.ID, limitedModel, otherModel)
	}

	// ── 断言**账号身份与顺序**，而不是只数次数 ──
	//
	// 🔴 Codex 第 52 轮指出："共调用两次" ≠ "首选、备用各一次" ——
	//	可能是同一个号被调了两次，或顺序反了。必须断言身份与顺序。
	order := chatter.callOrder()
	if len(order) != 2 {
		t.Fatalf("调用次数 = %d (%v)，期望 2 次", len(order), order)
	}
	if order[0] != "uid-first" {
		t.Errorf("第 1 次调用的是 %q，期望 uid-first（应当先试最早到期的号）", order[0])
	}
	if order[1] != "uid-backup" {
		t.Errorf("第 2 次调用的是 %q，期望 uid-backup（失败后换到备用号）", order[1])
	}
	// 首选号只被试过一次（不该被反复重试）
	cntFirst := 0
	for _, uid := range order {
		if uid == "uid-first" {
			cntFirst++
		}
	}
	if cntFirst != 1 {
		t.Errorf("uid-first 被调用 %d 次，期望 1 次（冷却后不该重复试同一个号）", cntFirst)
	}

	// ── 冷却到期后应**恢复调度**（Codex 要求补的生命周期另一半）──
	//
	// ⚠️ 推进量从 rateLimitCooldown **推导**（+1 分钟余量），不写死 2 分钟：
	//	冷却改成 10 分钟后，写死的 2 分钟**还没过期**，会让本断言假失败
	//	（"冷却到期后选中 uid-backup"——其实只是还没到期）。
	later := now.Add(rateLimitCooldown + time.Minute)
	selLater := pool.NewSelector()
	selLater.SetClock(func() time.Time { return later })
	var laterAccts []pool.Account
	for _, a := range reloaded.List() {
		laterAccts = append(laterAccts, a.ToPool(later))
	}
	back, err := selLater.PickForModel(laterAccts, "cn", limitedModel)
	if err != nil {
		t.Fatalf("冷却到期后应有账号可调度: %v", err)
	}
	// 冷却过期后，最早到期的 uid-first 应重新优先（它 10-11 vs 备用 12-31）
	if back.ID != "uid-first" {
		t.Errorf("冷却到期后选中 %q，期望 uid-first —— "+
			"冷却只应临时隔离，到期后必须恢复原优先级", back.ID)
	}
}

// TestReloadPreservesAllStatusFields 守：各类状态字段**完整**往返磁盘。
//
// Codex 指出的"只查 StatusUntil != 0 太弱"的推广：逐个状态验证
// Status / StatusReason / StatusUntil / ManualDisabled 都能落盘并读回。
func TestReloadPreservesAllStatusFields(t *testing.T) {
	cases := []struct {
		name     string
		status   pool.Status
		reason   string
		cooldown time.Duration
		manual   bool
	}{
		{"限流", pool.StatusRateLimited, "上游限流", 60 * time.Second, false},
		{"凭证失效", pool.StatusAuthExpired, "凭证失效: 401", 0, false},
		{"额度耗尽", pool.StatusNoCredit, "额度为零", 0, false},
		{"临时故障", pool.StatusTransient, "超时", 30 * time.Second, false},
		{"人工禁用", pool.StatusNormal, "", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newAccountFixture(t, []acctSpec{
				{uid: "uid-s", total: 100, packages: []auth.PackageSnapshot{
					{Name: "包", Remain: 100, ExpireAt: "2026-10-11"},
				}},
			})
			a, _ := fx.store.Get("uid-s")
			a.Status = tc.status
			a.StatusReason = tc.reason
			a.ManualDisabled = tc.manual
			if tc.cooldown > 0 {
				a.StatusUntil = nowFunc().Add(tc.cooldown)
			}
			if err := fx.persister.Save(fx.store); err != nil {
				t.Fatalf("落盘失败: %v", err)
			}

			got, ok := fx.reload().Get("uid-s")
			if !ok {
				t.Fatal("账号不见了")
			}
			if got.Status != tc.status {
				t.Errorf("Status = %q，期望 %q", got.Status, tc.status)
			}
			if got.StatusReason != tc.reason {
				t.Errorf("StatusReason = %q，期望 %q", got.StatusReason, tc.reason)
			}
			if got.ManualDisabled != tc.manual {
				t.Errorf("ManualDisabled = %v，期望 %v", got.ManualDisabled, tc.manual)
			}
			if tc.cooldown > 0 && got.StatusUntil.IsZero() {
				t.Error("StatusUntil 丢失")
			}
		})
	}
}

// TestNeverMarksBannedWithoutEvidence 守：**绝不**凭一次未知错误就标封号。
//
// 这是评审明确要求的消极断言：403/未知错误不得产生 banned。
func TestNeverMarksBannedWithoutEvidence(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("403 Forbidden"), authFailErr{}, rateLimitedErr{},
		context.DeadlineExceeded, fmt.Errorf("完全未知"),
	} {
		st, _, _ := classifyFailure(err)
		if st == pool.StatusBanned {
			t.Errorf("错误 %v 被判成 banned —— 本项目不应有任何路径产生 banned", err)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════
// 问题 4：官方平台解除异常后，项目能识别并取消标记吗？
//
// ⚠️⚠️ 本节的测试是 **Characterization（记录现状）**，不是正确行为护栏。
//
//	它们断言的是**当前产品缺陷**：不可调度状态缺少可达的恢复入口。
//	**这不是期望契约** —— 一旦实现了恢复机制，这些测试**应当变红并需要改写**
//	（先删/改现状断言，再加"成功清除"的规范断言）。
//
//	命名带 `Characterization` 就是为了**不把它混进"恢复功能必须通过"的语义**，
//	并让下一个人一眼看出"这条红了是好事"。
//
//	依据：Codex 第 52 轮裁定（原文见 codex-consult/52-codex-…md）：
//	  "提交，但明确作为 characterization test，不作为正确行为护栏…
//	   不能把它改成当前通过、语义却声称正确的测试。"
// ═══════════════════════════════════════════════════════════════════

// TestCharacterizationAuthExpiredRemainsExcludedAfterTimeAdvance 记录**当前真实行为**：
// 凭证失效不会自动解除。
//
// 🔴 这是"如实记录现状"的测试，**不是期望行为的测试**。
//
//	结论：auth_expired 只能靠**一次成功的对话**清除
//	（clearAccountFailure 只在 failover.go:150 被调用）。
//	而 auth_expired 又不可调度 ⇒ 永远不会被路由到 ⇒ **死锁**。
//	即：在官方平台重新登录并让项目重新导入凭据前，标记不会自己消失。
//
//	⚠️ 本测试若哪天变红，说明有人实现了"刷新额度即可清状态" —— 那是好事，
//	  但必须同时更新这条注释与状态机的设计说明。
func TestCharacterizationAuthExpiredRemainsExcludedAfterTimeAdvance(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-expired", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	acct, _ := fx.store.Get("uid-expired")
	acct.Status = pool.StatusAuthExpired
	acct.StatusReason = "凭证失效"

	// 无论过多久，都不会自愈
	for _, d := range []time.Duration{time.Minute, time.Hour, 30 * 24 * time.Hour} {
		future := time.Now().Add(d)
		if got := acct.EffectiveStatus(future); got != pool.StatusAuthExpired {
			t.Errorf("+%v 后状态 = %q，期望仍是 auth_expired（需人工重新授权）", d, got)
		}
	}

	// 且它不可调度 ⇒ 不会有对话成功 ⇒ clearAccountFailure 永不触发
	sel := pool.NewSelector()
	_, err := sel.PickForPlatform([]pool.Account{acct.ToPool(time.Now())}, "cn")
	if err == nil {
		t.Error("auth_expired 不该可调度（否则会把必然失败的请求发出去）")
	}
}

// TestCreditParsesRemainingFromSuccessfulBillingResponse 验证
// `provider.Credit` 能从**模拟的 billing 成功响应**里正确解析出余额。
//
// ⚠️ 这是**解析／接口集成测试**，**不是**恢复行为的 characterization 测试。
// 命名与范围按 Codex 第 52 轮裁定收窄（原文见
// `codex-consult/52-codex-账号调度四项测试评审.md`）。
//
// 🔴 **本测试未覆盖的东西（必须如实标注，不要当成已验证）**：
//
//	· **未覆盖** `refreshCredits` 的生产编排与账号写回 ——
//	  它内部自建 client（`cli_impl.go:349`）且**没有注入点**，
//	  所以无法在测试里让它指向假上游。生产路径覆盖**列为待办**。
//	· **未覆盖**异常状态的恢复行为。（初版曾把"测试代码自己写回额度、
//	  再断言状态未变"当作恢复证据 —— 那是错的：测试自己写的东西
//	  证明不了产品行为。已按 Codex 要求删除。）
//	· **不证明**真实上游"billing 成功"代表 chat 可用（跨端点鉴权一致性未验证）。
//
// ⇒ 对委托方第 4 问（官方解除后能否自动取消标记）的回答，
//
//	**执行证据层面是"未覆盖"**；只有**静态代码证据**（见下）。
//
// 静态代码证据（我方 grep + 逐层读取，非本测试所证）：
//
//	refreshCredits（cli_impl.go:347-387）本体只写
//	  LastObservedAt / LastError / Credit.Known / Credit.Remaining /
//	  Credit.At / Credit.Packages —— **不碰** Status / StatusReason / StatusUntil。
//	它唯一调用的生产函数是 p.Credit（:364），而
//	  provider.Credit（checkin.go:106-161）**只读**：构造并返回 CreditResult，
//	  不对任何账号字段赋值。
//	⇒ 整条刷新调用链没有"清状态"的副作用。
//	⚠️ 但"函数不写状态"≠"系统会自动恢复" —— 恰恰相反：
//	  正是因为没有写入，才**没有**恢复入口（这才是缺口所在）。
func TestCreditParsesRemainingFromSuccessfulBillingResponse(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSONForPath("/v2/billing/meter/get-user-resource", "billing-user-resource.json")

	client := newWorkbuddyClientForTest(t, fake.URL)
	cred := testWorkbuddyCredential()
	p := workbuddy.NewProviderFor(client, cred, workbuddy.PlatformCN)

	cr, err := p.Credit(context.Background(), "")
	if err != nil {
		t.Fatalf("额度解析应当成功: %v", err)
	}
	if cr.Remaining == nil {
		t.Fatal("未解析出余额 —— 解析逻辑有问题")
	}
	if *cr.Remaining <= 0 {
		t.Errorf("余额 = %d，期望为正（样本是有效额度）", *cr.Remaining)
	}
	if len(cr.Accounts) == 0 {
		t.Error("未解析出任何额度包")
	}
}

// TestManualDisabledIsNeverAutoCleared 守：人工禁用不被自动逻辑清掉。
//
// 与"异常状态自动恢复"是两回事 —— 人工设置必须始终优先。
func TestManualDisabledIsNeverAutoCleared(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-d", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})
	acct, _ := fx.store.Get("uid-d")
	acct.ManualDisabled = true

	// 即便调用"成功清理"，也不该解开人工禁用
	//
	// ⚠️ 2026-10-07：clearAccountFailure 现在只收 UID（写操作在锁内做），
	//	所以这里改传 UID；读回也要用快照（不再拿共享指针）。
	//
	// ⚠️ 2026-10-07 修 R1 后它还要一个**版本基线**：取当前 Rev 传进去，
	//	模拟"本次请求开始时的版本"（条件提交才会被接受）。
	//	本测试关注的是"人工禁用不被覆盖"，与版本时序无关，
	//	故取当前版本即可。
	rev, _ := fx.store.Rev("uid-d")
	clearAccountFailure(fx.deps(), "uid-d", rev, "deepseek-v4.1-flash")
	after, _ := fx.store.Snapshot("uid-d")
	if !after.ManualDisabled {
		t.Error("clearAccountFailure 解开了人工禁用 —— 人工设置必须不被自动逻辑覆盖")
	}

	// 多久都不可调度
	sel := pool.NewSelector()
	for _, d := range []time.Duration{0, time.Hour, 30 * 24 * time.Hour} {
		if _, err := sel.PickForPlatform([]pool.Account{acct.ToPool(time.Now().Add(d))}, "cn"); err == nil {
			t.Errorf("+%v 后人工禁用的账号被选中了", d)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════
// 面板/API 是否真的把状态暴露出来（"真实标记"的可见性）
// ═══════════════════════════════════════════════════════════════════

// TestAccountStatusVisibleThroughPanelAPI 守：异常标记在**面板 API** 上可见。
//
// 委托方问"会将账号异常在项目中真实标记吗" —— 标记必须能被看到，
// 否则等于没标记。
func TestAccountStatusVisibleThroughPanelAPI(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-bad", total: 50, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 50, ExpireAt: "2026-10-07"},
		}},
	})
	acct, _ := fx.store.Get("uid-bad")
	acct.Status = pool.StatusRateLimited
	acct.StatusReason = "上游限流"
	acct.StatusUntil = time.Now().Add(60 * time.Second)

	srv := httptest.NewServer(newMux(fx.deps()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/accounts")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	var body struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(body.Accounts) == 0 {
		t.Fatal("面板没返回账号")
	}
	a := body.Accounts[0]
	// 至少要有 status / status_reason 这类字段
	raw, _ := json.Marshal(a)
	s := string(raw)
	for _, want := range []string{"status", "reason"} {
		if !containsFold(s, want) {
			t.Errorf("面板响应缺少 %q 相关信息 —— 异常标记不可见即等于没标记\n%s", want, s)
		}
	}
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
