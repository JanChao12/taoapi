package pool

import (
	"testing"
	"time"
)

// fixedNow 返回一个固定的"现在"，所有测试用它保证可复现。
func fixedNow() time.Time {
	return time.Date(2026, 10, 4, 12, 0, 0, 0, zone)
}

func newTestSelector() *Selector {
	s := NewSelector()
	s.SetClock(fixedNow)
	return s
}

// acct 构造一个"已查过额度"的可调度账号，减少测试噪音。
func acct(id string, credits int64, pkgs ...Package) Account {
	return Account{ID: id, Credits: credits, CreditsKnown: true, Packages: pkgs}
}

func pkg(remain int64, expire string) Package {
	return Package{Name: "包", Remain: remain, ExpireAt: expire}
}

// ─────────────────────────────────────────────────────────────
// 核心：只看最早到期包，与"临期总量"无关
// ─────────────────────────────────────────────────────────────

// TestPickEarliestPackageWins 是【本轮修正的核心测试】。
//
// 委托人的要求：只看每个账号下哪个包先过期，谁先过期谁优先 ——
// 【不是】按"几天内临期额度总量"排序。
//
// 场景刻意构造得让两种策略给出【相反】结果：
//   - A：最早包 10-05 到期，只有 1 分；全部额度合计 1 分
//   - B：最早包 10-06 到期，有 5000 分
//     → 旧策略（临期总量）会选 B（5000 > 1）
//     → 正确策略（最早到期日）必须选 A
func TestPickEarliestPackageWins(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("B-more-total", 5000, pkg(5000, "2026-10-06")),
		acct("A-expires-first", 1, pkg(1, "2026-10-05")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "A-expires-first" {
		t.Errorf("选中了 %q；应选最早到期的 A-expires-first。"+
			"若这里选了 B，说明又退回了被否定的'临期总量'排序", got.ID)
	}
}

// TestPickUsesEarliestPackageNotSum 验证账号的排序键是其【最早】包，
// 而不是任何形式的额度总和。
func TestPickUsesEarliestPackageNotSum(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		// 总额度巨大，但最早包 10-20 才到期
		acct("big-late", 100000,
			pkg(90000, "2026-10-20"), pkg(10000, "2026-10-21")),
		// 总额度很小，但最早包 10-05 到期
		acct("small-early", 50,
			pkg(50, "2026-10-05"), pkg(0, "2026-10-06")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "small-early" {
		t.Errorf("选中了 %q，应选最早到期的 small-early", got.ID)
	}
}

// TestPickSkipsZeroRemainPackagesForExpiry 验证零/负额度包不参与最早到期日。
//
// 否则一个额度已用完的旧包（日期很早）会把账号错误地排到最前面。
func TestPickSkipsZeroRemainPackagesForExpiry(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		// 有一个 0 额度的包在 10-01（很早），但真正有额度的是 10-20
		acct("has-dead-old-package", 300, pkg(0, "2026-10-01"), pkg(300, "2026-10-20")),
		// 最早有效包 10-15
		acct("real-early", 100, pkg(100, "2026-10-15")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	// dead 包的 10-01 不该算数，所以 real-early(10-15) 应先于 has-dead(10-20)
	if got.ID != "real-early" {
		t.Errorf("选中了 %q；0 额度包的到期日不该参与排序，应选 real-early", got.ID)
	}
}

// TestEarliestExpirySkipsZeroAndInvalid 验证 EarliestExpiry 的过滤规则。
func TestEarliestExpirySkipsZeroAndInvalid(t *testing.T) {
	cases := []struct {
		name string
		pkgs []Package
		want string
	}{
		{"正常取最早", []Package{pkg(5, "2026-12-01"), pkg(5, "2026-10-05")}, "2026-10-05"},
		{"跳过零额度", []Package{pkg(0, "2026-10-01"), pkg(5, "2026-10-20")}, "2026-10-20"},
		{"跳过负额度", []Package{pkg(-7, "2026-10-01"), pkg(5, "2026-10-20")}, "2026-10-20"},
		{"跳过空日期", []Package{pkg(5, ""), pkg(5, "2026-10-20")}, "2026-10-20"},
		{"跳过非法日期", []Package{pkg(5, "not-a-date"), pkg(5, "2026-10-20")}, "2026-10-20"},
		{"全是零额度", []Package{pkg(0, "2026-10-01")}, ""},
		{"全无日期", []Package{pkg(5, "")}, ""},
		{"无包", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Account{Packages: c.pkgs}
			if got := a.EarliestExpiry(); got != c.want {
				t.Errorf("EarliestExpiry = %q，期望 %q", got, c.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────
// tie-break
// ─────────────────────────────────────────────────────────────

// TestTieBreakSameExpiryUsesThatDateOnly 验证同到期日时，
// 只用【该日期】的额度比较，不混入其他日期。
func TestTieBreakSameExpiryUsesThatDateOnly(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		// 同为 10-05 到期：该日 10 分，但另有 9000 分在 12-01
		acct("other-date-rich", 9010, pkg(10, "2026-10-05"), pkg(9000, "2026-12-01")),
		// 同为 10-05 到期：该日 500 分
		acct("same-date-rich", 500, pkg(500, "2026-10-05")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "same-date-rich" {
		t.Errorf("选中了 %q；同到期日应比【该日期】的额度，不应把 12-01 的 9000 分算进来",
			got.ID)
	}
}

// TestTieBreakFallsBackToTotalCredits 验证同到期日、同该日额度时按总额度。
func TestTieBreakFallsBackToTotalCredits(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("small", 100, pkg(100, "2026-10-05")),
		acct("big", 5000, pkg(100, "2026-10-05")), // 该日同为 100，总数更大
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "big" {
		t.Errorf("选中了 %q，该日额度相同时应选总额度更多的 big", got.ID)
	}
}

// TestPickStableTieBreak 验证完全相同时按 ID 稳定排序。
func TestPickStableTieBreak(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("bbb", 100, pkg(100, "2035-01-01")),
		acct("aaa", 100, pkg(100, "2035-01-01")),
	}

	for i := 0; i < 5; i++ {
		got, err := sel.Pick(accounts)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "aaa" {
			t.Fatalf("第 %d 次结果不稳定: %q（应稳定选 aaa）", i, got.ID)
		}
	}
}

// TestNoExpiryRanksLast 验证所有包都无到期日的账号排在最后。
func TestNoExpiryRanksLast(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("unknown-expiry", 99999, pkg(99999, "")),
		acct("known-expiry", 1, pkg(1, "2035-01-01")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "known-expiry" {
		t.Errorf("选中了 %q；到期日未知的账号应排在最后", got.ID)
	}
}

// TestNoExpiryAmongThemselves 验证都无到期日时按额度降序。
func TestNoExpiryAmongThemselves(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("small", 10, pkg(10, "")),
		acct("big", 8000, pkg(8000, "")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "big" {
		t.Errorf("都无到期日时应按额度降序，实际 %q", got.ID)
	}
}

// ─────────────────────────────────────────────────────────────
// 排除规则
// ─────────────────────────────────────────────────────────────

// TestPickSkipsDisabled 验证人工禁用账号被跳过（无论额度多少）。
func TestPickSkipsDisabled(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		{ID: "disabled", Credits: 9999, CreditsKnown: true, Disabled: true,
			Packages: []Package{pkg(9999, "2026-10-05")}},
		acct("usable", 10, pkg(10, "2035-01-01")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "usable" {
		t.Errorf("选中了 %q，禁用的账号不应被选", got.ID)
	}
}

// TestPickSkipsCooldown 验证冷却中的账号被跳过。
func TestPickSkipsCooldown(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		{ID: "cooling", Credits: 9999, CreditsKnown: true,
			CooldownUntil: fixedNow().Add(time.Hour),
			Packages:      []Package{pkg(9999, "2026-10-05")}},
		acct("ok", 10, pkg(10, "2035-01-01")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ok" {
		t.Errorf("选中了 %q，冷却中的账号不应被选", got.ID)
	}
}

// TestPickAllowsExpiredCooldown 验证冷却已过的账号可被选。
func TestPickAllowsExpiredCooldown(t *testing.T) {
	sel := newTestSelector()

	a := acct("was-cooling", 100, pkg(100, "2035-01-01"))
	a.CooldownUntil = fixedNow().Add(-time.Hour)

	got, err := sel.Pick([]Account{a})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "was-cooling" {
		t.Errorf("冷却已过应可被选，实际 %q", got.ID)
	}
}

// TestPickSkipsNonSchedulableStatus 验证不可自愈的状态被跳过。
func TestPickSkipsNonSchedulableStatus(t *testing.T) {
	sel := newTestSelector()

	for _, st := range []Status{
		StatusBanned, StatusDisabled, StatusAuthExpired, StatusNoCredit,
	} {
		t.Run(string(st), func(t *testing.T) {
			a := acct("x", 1000, pkg(1000, "2026-10-05"))
			a.Status = st
			if _, err := sel.Pick([]Account{a}); err != ErrNoAvailable {
				t.Errorf("状态 %s 不应可调度，实际 err=%v", st, err)
			}
		})
	}
}

// TestPickAllowsSelfHealingStatus 验证可自愈状态（冷却过后）能参与调度。
func TestPickAllowsSelfHealingStatus(t *testing.T) {
	sel := newTestSelector()

	for _, st := range []Status{
		StatusNormal, StatusRateLimited, StatusTransient, StatusUnknown,
	} {
		t.Run(string(st), func(t *testing.T) {
			a := acct("x", 1000, pkg(1000, "2026-10-05"))
			a.Status = st
			a.CooldownUntil = fixedNow().Add(-time.Minute) // 冷却已过
			if _, err := sel.Pick([]Account{a}); err != nil {
				t.Errorf("状态 %s 冷却过后应可调度，实际 err=%v", st, err)
			}
		})
	}
}

// TestPickRequiresKnownCredits 验证"额度未知"与"额度为零"被区别对待。
//
// CreditsKnown=false 时既不该当成有额度（可能没查成功），
// 但要能通过 ErrNoAvailable 表达出来，而不是崩。
func TestPickRequiresKnownCredits(t *testing.T) {
	sel := newTestSelector()

	unknown := Account{ID: "unknown", Credits: 0, CreditsKnown: false}
	knownZero := Account{ID: "known-zero", Credits: 0, CreditsKnown: true}

	if _, err := sel.Pick([]Account{unknown}); err != ErrNoAvailable {
		t.Errorf("额度未知不应被调度，实际 err=%v", err)
	}
	if _, err := sel.Pick([]Account{knownZero}); err != ErrNoAvailable {
		t.Errorf("零额度不应被调度，实际 err=%v", err)
	}
}

// TestPickSkipsZeroCredits 验证零/负额度账号被跳过。
func TestPickSkipsZeroCredits(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("empty", 0),
		acct("negative", -5),
	}

	if _, err := sel.Pick(accounts); err != ErrNoAvailable {
		t.Errorf("无可用账号时应返回 ErrNoAvailable，实际 %v", err)
	}
}

// TestPickNoAccounts 验证空列表。
func TestPickNoAccounts(t *testing.T) {
	sel := newTestSelector()
	if _, err := sel.Pick(nil); err != ErrNoAvailable {
		t.Errorf("空列表应返回 ErrNoAvailable，实际 %v", err)
	}
}

// ─────────────────────────────────────────────────────────────
// 换号重试支持
// ─────────────────────────────────────────────────────────────

// TestPickExcluding 验证换号时不会重新选回失败的账号。
func TestPickExcluding(t *testing.T) {
	sel := newTestSelector()

	accounts := []Account{
		acct("first", 100, pkg(100, "2026-10-05")),
		acct("second", 200, pkg(200, "2026-10-06")),
	}

	got, err := sel.Pick(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "first" {
		t.Fatalf("首选应为 first，实际 %q", got.ID)
	}

	// 排除 first 后应选 second
	got2, err := sel.PickExcluding(accounts, map[string]bool{"first": true})
	if err != nil {
		t.Fatal(err)
	}
	if got2.ID != "second" {
		t.Errorf("排除 first 后应选 second，实际 %q", got2.ID)
	}

	// 两个都排除 → 无可用
	if _, err := sel.PickExcluding(accounts,
		map[string]bool{"first": true, "second": true}); err != ErrNoAvailable {
		t.Errorf("全部排除后应返回 ErrNoAvailable，实际 %v", err)
	}
}

// ─────────────────────────────────────────────────────────────
// 展示统计（不参与调度）
// ─────────────────────────────────────────────────────────────

// TestExpiringWithinIsDisplayOnly 验证 ExpiringWithin 仍可用于面板展示。
//
// ⚠️ 关键：它【不参与】Pick。这里同时断言这一点，防止将来被误接回调度。
func TestExpiringWithinIsDisplayOnly(t *testing.T) {
	now := fixedNow()
	acc := Account{
		Packages: []Package{
			{Name: "今天到期", Remain: 10, ExpireAt: "2026-10-04"},
			{Name: "明天到期", Remain: 20, ExpireAt: "2026-10-05"},
			{Name: "第7天", Remain: 40, ExpireAt: "2026-10-11"},
			{Name: "第8天", Remain: 80, ExpireAt: "2026-10-12"},
			{Name: "很久以后", Remain: 160, ExpireAt: "2035-01-01"},
			{Name: "零额度", Remain: 0, ExpireAt: "2026-10-04"},
			{Name: "非法", Remain: 5, ExpireAt: "oops"},
		},
	}

	// 7 天窗口：10-11 是 now+7d 当天，应算；10-12 不算
	if got, want := acc.ExpiringWithin(7*24*time.Hour, now), int64(10+20+40); got != want {
		t.Errorf("7 天窗口 = %d，期望 %d", got, want)
	}
	if got, want := acc.ExpiringWithin(24*time.Hour, now), int64(10+20); got != want {
		t.Errorf("1 天窗口 = %d，期望 %d", got, want)
	}

	// 展示统计与调度无关：这个账号最早有效到期日是 10-04
	if got := acc.EarliestExpiry(); got != "2026-10-04" {
		t.Errorf("EarliestExpiry = %q，期望 2026-10-04", got)
	}
}

// TestRemainOn 验证 RemainOn 只统计指定日期。
func TestRemainOn(t *testing.T) {
	a := Account{Packages: []Package{
		pkg(10, "2026-10-05"),
		pkg(20, "2026-10-05"),
		pkg(999, "2026-10-06"),
		pkg(0, "2026-10-05"),
	}}
	if got := a.RemainOn("2026-10-05"); got != 30 {
		t.Errorf("RemainOn(10-05) = %d，期望 30（10 月 5 日的两个包，零额度不计）", got)
	}
	if got := a.RemainOn(""); got != 0 {
		t.Errorf("RemainOn(\"\") = %d，期望 0", got)
	}
	if got := a.RemainOn("2035-01-01"); got != 0 {
		t.Errorf("不存在的日期应为 0，实际 %d", got)
	}
}
