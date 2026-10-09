package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
)

// 本文件守 2026-10-09 委托方反馈的两处「显示不真实」。
//
//	① 积分包进度条按**积分量**画长度，而不是按**占比**
//	② 「刷新」按钮与"切到本页"行为不一致

// TestPackagePercentIsRatioNotRelativeLength 守：进度条用「剩余/总量」的占比。
//
// ═══════════════════════════════════════════════════════════════════
// 委托方原话（2026-10-09）：
//
//	「这个积分包显示条的长度应该按百分比显示，这个包只有 10 积分但是
//	  没使用过所以也是 100% 满长度，而不是按积分量显示长度」
//
// ═══════════════════════════════════════════════════════════════════
//
//	原实现前端用 `remain / maxRemain`（本账号内最大的包当分母）——
//	那是**相对长度**：10 积分且没用过的包会画满格，看起来像额度充足，
//	而它其实只是"还剩 10"。委托方判断正确。
//
//	修法：后端在下发视图里直接给 percent（单一权威口径，避免前端
//	各自处理分母为 0）。本测试守后端这一半。
func TestPackagePercentIsRatioNotRelativeLength(t *testing.T) {
	now := time.Now()

	// 关键对照：10 积分但**没用过**（总量 10），与 1500 积分的包并列。
	// 旧口径下前者会因 maxRemain=1500 而画成极小；而它其实剩 100%。
	a := &auth.Account{
		UID:      "u1",
		Nickname: "测试号",
		Credit: auth.CreditSnapshot{
			Known:     true,
			Remaining: 1510,
			Packages: []auth.PackageSnapshot{
				{Name: "只有10没用过", Remain: 10, Size: 10, Used: 0, ExpireAt: "2026-12-31"},
				{Name: "大包用了一半", Remain: 1500, Size: 3000, Used: 1500, ExpireAt: "2026-12-31"},
			},
		},
	}

	v := buildAccountView(a, now)

	if len(v.Packages) != 2 {
		t.Fatalf("包数 = %d，期望 2", len(v.Packages))
	}

	// ① 小包必须报 100%（因为它没用过），**不是** 10/1500≈1%
	if v.Packages[0].Percent == nil {
		t.Fatal("小包没有 percent —— 后端必须算好下发，否则前端只能猜分母")
	}
	if got := *v.Packages[0].Percent; got != 100 {
		t.Errorf("「剩 10 / 总量 10」的百分比 = %d%%，期望 100%% —— "+
			"这是委托方实测指出的缺陷：按积分量画长度会让没用过的包显示成满格/极小，"+
			"两种做法都不对；正确口径是 remain/Size", got)
	}

	// ② 大包 1500/3000 = 50%
	if v.Packages[1].Percent == nil || *v.Packages[1].Percent != 50 {
		t.Errorf("「剩 1500 / 总量 3000」的百分比 = %v，期望 50%%", v.Packages[1].Percent)
	}

	// ③ 总量必须一起下发（前端要显示 "10 / 10"）
	if v.Packages[0].Size != 10 || v.Packages[0].Used != 0 {
		t.Errorf("小包 Size/Used = %d/%d，期望 10/0",
			v.Packages[0].Size, v.Packages[0].Used)
	}
}

// TestPackagePercentNilWhenSizeUnknown 守：无总量时 percent 是 **null**，不是 0%。
//
// 🔴 为什么必须区分（本项目反复出现的一类缺陷）：
//
//	0% = "确实用完了"；null = "上游没下发总量，算不出来"。
//	把后者显示成 0% 会让用户以为额度耗尽 —— 那是伪造数据。
//	老落盘数据（本字段加入之前）就是这种情形：只有 Remain。
func TestPackagePercentNilWhenSizeUnknown(t *testing.T) {
	now := time.Now()
	a := &auth.Account{
		UID: "u1",
		Credit: auth.CreditSnapshot{
			Known:     true,
			Remaining: 500,
			// 老数据：只有 Remain，没有 Size/Used
			Packages: []auth.PackageSnapshot{
				{Name: "老数据包", Remain: 500, ExpireAt: "2026-12-31"},
			},
		},
	}

	v := buildAccountView(a, now)
	if len(v.Packages) != 1 {
		t.Fatalf("包数 = %d", len(v.Packages))
	}
	if v.Packages[0].Percent != nil {
		t.Errorf("无总量时 percent = %d，期望 nil（JSON null）—— "+
			"0%% 会被读成「额度耗尽」，而真相是「不知道」", *v.Packages[0].Percent)
	}

	// JSON 里必须是 null（前端据 typeof === 'number' 判断）
	raw, err := json.Marshal(v.Packages[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"percent":null`) {
		t.Errorf("JSON 里 percent 应为 null，实际：%s", raw)
	}
}

// TestPackagePercentClampsToValidRange 守：百分比被夹到 [0,100]。
//
// 上游偶发口径异常（remain > size，或负值）时不能让进度条溢出容器。
func TestPackagePercentClampsToValidRange(t *testing.T) {
	cases := []struct {
		name         string
		remain, size int64
		want         int
	}{
		{"正常", 50, 100, 50},
		{"全满", 100, 100, 100},
		{"用完", 0, 100, 0},
		{"remain 超出 size（上游口径异常）", 150, 100, 100},
		{"负 remain", -5, 100, 0},
	}
	for _, c := range cases {
		p := auth.PackageSnapshot{Remain: c.remain, Size: c.size}
		got, ok := p.Percent()
		if !ok {
			t.Errorf("%s：Percent 报无总量，但 Size=%d", c.name, c.size)
			continue
		}
		if got != c.want {
			t.Errorf("%s：Percent = %d，期望 %d", c.name, got, c.want)
		}
	}

	// Size<=0 ⇒ 明确报"无数据"（不能返回 0 当"用完了"）
	if _, ok := (auth.PackageSnapshot{Remain: 10, Size: 0}).Percent(); ok {
		t.Error("Size=0 时 Percent 应报 ok=false（无总量），不能当成 0% ")
	}
}

// TestRefreshButtonMatchesPageSwitch 守：「刷新」按钮与切页做同样的事。
//
// ═══════════════════════════════════════════════════════════════════
// 委托方提问（2026-10-09）：
//
//	「调用记录的手动刷新和打开这页的自动刷新怎么不一样」
//
// ═══════════════════════════════════════════════════════════════════
//
//	确实不一样 —— 切页走 pages.stats()，它**同时**调 loadUsageLog()
//	与 loadStats()；而按钮只调 loadUsageLog()。
//	后果：点刷新只更新下方明细表，**顶部那排卡片纹丝不动**，
//	用户会以为刷新没生效。
//
// 本测试断言两者都拉两个接口。
func TestRefreshButtonMatchesPageSwitch(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	// ① 切页（pages.stats）必须两个都拉
	pIdx := strings.Index(js, "stats: function ()")
	if pIdx < 0 {
		t.Fatal("找不到 pages.stats")
	}
	pageBody := js[pIdx:]
	if n := strings.Index(pageBody, "accounts: loadAccts"); n > 0 {
		pageBody = pageBody[:n]
	}
	for _, want := range []string{"loadUsageLog()", "loadStats()"} {
		if !strings.Contains(pageBody, want) {
			t.Errorf("pages.stats 缺少 %s", want)
		}
	}

	// ② 刷新按钮必须也两个都拉（这是本次修的点）
	bIdx := strings.Index(js, "function initUsageLog()")
	if bIdx < 0 {
		t.Fatal("找不到 initUsageLog")
	}
	btnBody := js[bIdx:]
	if n := strings.Index(btnBody, "\n  function "); n > 0 {
		btnBody = btnBody[:n]
	}
	if !strings.Contains(btnBody, "loadStats()") {
		t.Error("「刷新」按钮没有调 loadStats() —— " +
			"点刷新只会更新下方明细，顶部卡片（请求数/输入/输出/总计/积分/命中率）不会动，" +
			"与切页行为不一致（委托方已指出这个差异）")
	}
	if !strings.Contains(btnBody, "loadUsageLog()") {
		t.Error("「刷新」按钮没有调 loadUsageLog() —— 明细不会刷新")
	}
}
