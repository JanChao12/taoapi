package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
)

// ═══════════════════════════════════════════════════════════════════
// 账号+模型限流的**对外可见性**护栏（2026-10-09）
//
// 委托方原话：「上游的限流我实测之前使用DeepSeek过多导致限制，
//              我切换glm后能正常使用」
//
// 功能做出来了但**看不见**等于没做：限流现在记在模型级，账号本身
// 状态是「正常」—— 若面板/api 不把 model_cooldowns 交出去，
// 用户只会看到"正常"，完全不知道 deepseek 被限到几点。
// ═══════════════════════════════════════════════════════════════════

// TestAccountsAPIExposesModelCooldowns 守：/api/accounts 下发模型级限流。
func TestAccountsAPIExposesModelCooldowns(t *testing.T) {
	fx := newAccountFixture(t, []acctSpec{
		{uid: "uid-m", total: 100, packages: []auth.PackageSnapshot{
			{Name: "包", Remain: 100, ExpireAt: "2026-10-07"},
		}},
	})

	// 造出"某个模型被限流、账号本身正常"的状态
	future := time.Now().Add(2 * time.Hour)
	fx.store.Mutate("uid-m", func(a *auth.Account) bool {
		a.Status = pool.StatusNormal
		a.StatusReason = "模型限流: workbuddy/deepseek-v4.1-flash"
		a.ModelCooldowns = map[string]time.Time{
			"workbuddy/deepseek-v4.1-flash": future,
			// 这条**已过期**：不该下发（陈旧信息比没有更糟）
			"workbuddy/glm-5.3-flash": time.Now().Add(-time.Hour),
		}
		return true
	})

	srv := httptest.NewServer(newMux(fx.deps()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Accounts []struct {
			UID            string            `json:"uid"`
			Status         string            `json:"status"`
			ModelCooldowns map[string]string `json:"model_cooldowns"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(body.Accounts) == 0 {
		t.Fatal("没有账号")
	}
	a := body.Accounts[0]

	// ① 未过期的模型必须下发（否则面板看不到"哪个模型被限流"）
	if a.ModelCooldowns == nil {
		t.Fatal("model_cooldowns 缺失 —— 面板无法显示被限流的模型（功能等于不可见）")
	}
	if _, ok := a.ModelCooldowns["workbuddy/deepseek-v4.1-flash"]; !ok {
		t.Errorf("未过期的模型限流没下发，实际 %v", a.ModelCooldowns)
	}
	// ② 已过期的不下发
	if _, ok := a.ModelCooldowns["workbuddy/glm-5.3-flash"]; ok {
		t.Errorf("已过期的模型限流被下发 —— 会让用户以为还在限流，实际 %v", a.ModelCooldowns)
	}
	// ③ 账号状态本身仍是正常（限流是按模型的）
	if a.Status != "normal" {
		t.Errorf("账号状态 = %q，期望 normal（模型级限流不影响账号级状态）", a.Status)
	}
}

// TestPanelRendersModelCooldowns 守：面板 JS 真的渲染 model_cooldowns。
//
// 只测后端下发是不够的 —— 前端不读它，用户照样看不见。
func TestPanelRendersModelCooldowns(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	if !strings.Contains(js, "a.model_cooldowns") {
		t.Error("面板没有读 a.model_cooldowns —— 模型级限流不会被显示，" +
			"用户只能看到「正常」，不知道哪个模型被限流了")
	}
	if !strings.Contains(js, "acct-modellimit") {
		t.Error("缺少模型级限流的样式类 acct-modellimit —— 提示不会按预期渲染")
	}
	// 样式必须存在（否则是个无样式的裸 div，像委托方第 60 轮反馈的浮层那样）
	css := panelAsset(t, "style.css")
	if !strings.Contains(css, ".acct-modellimit") {
		t.Error("style.css 缺少 .acct-modellimit 规则 —— 提示会以裸文本铺在卡片里")
	}
	// 用橙色而不是红色：账号本身是好的，红色会误导成"整个号坏了"
	idx := strings.Index(css, ".acct-modellimit {")
	if idx < 0 {
		t.Fatal("找不到 .acct-modellimit 规则")
	}
	rule := css[idx:]
	if end := strings.Index(rule, "}"); end > 0 {
		rule = rule[:end]
	}
	if strings.Contains(rule, "#dc2626") {
		t.Errorf(".acct-modellimit 用了红色（#dc2626）—— 账号本身是正常的，"+
			"红色会让人以为整个号坏了。实际规则：%s", rule)
	}
}

// TestModelLimitSharesStatusSlot 守：模型限流与状态徽章**共用同一个槽位**，
// 因此正常卡片与被限流卡片的行数/宽度一致。
//
// ═══════════════════════════════════════════════════════════════════
// 委托方 2026-10-09 要求（原话）：
//
//	「我建议正常的账号在那里显示状态正常，限流的账号那里显示限流模型，
//	  这样每一个卡片的宽度大小都是一样的」
//
// ═══════════════════════════════════════════════════════════════════
//
// 上一版把模型限流放在**单独一行**（.acct-modellimit-row）⇒
// 被限流的卡片比正常卡片多一行，同一排高度参差。
//
// 修法：在卡头最右的**同一个槽位**二选一渲染（statusSlotHtml）。
//
// 反向对照：把模型限流改回单独一行渲染 ⇒ 本条红。
func TestModelLimitSharesStatusSlot(t *testing.T) {
	js := stripJSComments(panelAsset(t, "app.js"))

	// reBorderDecl 匹配真正的 border **声明**（border: / border-top: …），
	// 但**不**匹配 border-radius（那不是描边，不占布局空间）。
	reBorderDecl := regexp.MustCompile(`\bborder(?:-(?:top|right|bottom|left))?\s*:`)

	// ① 必须存在"单一状态槽"这个变量（二选一渲染的唯一入口）
	if !strings.Contains(js, "statusSlotHtml") {
		t.Fatal("app.js 没有 statusSlotHtml —— 模型限流没有与状态徽章共用槽位")
	}
	// ② 槽位里必须**恰好**渲染一次 statusSlotHtml（多一次就说明又分了两处）
	if n := strings.Count(js, "+   statusSlotHtml"); n != 1 {
		t.Errorf("statusSlotHtml 在卡头里出现了 %d 次，期望 1 次 —— "+
			"多一处就会多一行/多一块，卡片高度又不一致了", n)
	}
	// ③ 反向断言：不许再有"单独一行"的模型限流容器
	if strings.Contains(js, "acct-modellimit-row") {
		t.Error("仍存在 acct-modellimit-row —— 模型限流又变成单独一行了，" +
			"被限流的卡片会比正常卡片高（委托方要求两者一致）")
	}
	// ④ 样式表里那个行容器也必须已删除
	if strings.Contains(panelAsset(t, "style.css"), ".acct-modellimit-row") {
		t.Error("style.css 仍有 .acct-modellimit-row —— 应已随布局改动删除")
	}
	// ⑤ 更严重的账号级状态必须优先（不能被"模型限流"顶掉）
	if !strings.Contains(js, "a.status !== 'normal'") {
		t.Error("状态槽没有优先判断账号级异常 —— " +
			"凭证失效/封号等更严重的状态会被「模型限流」顶掉，" +
			"用户会以为只是限流、实际需要重新授权")
	}
	// ⑥ 宽度必须收得住（flex 子项默认 min-width:auto，长模型名会撑宽卡片）
	//
	// ⚠️ 必须先剥掉 CSS 注释：规则里那段"不能用 border"的**说明文字**
	//	本身含 `border:1px`，不剥注释会被自己的注释误伤
	//	（第一版就这么假失败了一次）。
	//	stripJSComments 对 CSS 块注释同样适用（它剥 /* */ 与 //）。
	css := stripJSComments(panelAsset(t, "style.css"))
	idx := strings.Index(css, ".acct-modellimit {")
	if idx < 0 {
		t.Fatal("找不到 .acct-modellimit 规则")
	}
	rule := css[idx:]
	if end := strings.Index(rule, "}"); end > 0 {
		rule = rule[:end]
	}
	for _, want := range []string{"min-width: 0", "text-overflow: ellipsis"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".acct-modellimit 缺 %q —— 长模型名会撑宽卡片，"+
				"同排卡片宽度不再一致。实际规则：%s", want, rule)
		}
	}

	// ⑦ 🔴 **不许用 border**（2026-10-09 实测踩到，必须守住）
	//
	//	状态徽章 .badge 没有 border，所以它高 23px；而这个元素加
	//	`border: 1px` 会多占 2px 布局空间 ⇒ 卡头 23→25px ⇒
	//	被限流的卡片整张比正常卡片**高 2px**（实测 185 vs 183）。
	//	委托方要求"每一个卡片的宽度大小都是一样的"，2px 肉眼可见。
	//
	//	修法：用 `box-shadow: inset 0 0 0 1px` 画描边 —— 不占布局空间。
	//
	// ⚠️ 判定必须用正则匹配 `border:` / `border-*:` 这类**声明**，
	//	不能用 strings.Contains(rule, "border:")：那会被 `border-radius:`
	//	误伤（第一版就这么假失败了）。
	//
	// 反向对照：把 box-shadow 换成 border ⇒ 本条立刻红。
	if !strings.Contains(rule, "box-shadow: inset") {
		t.Errorf(".acct-modellimit 缺 box-shadow: inset 描边 ——\n"+
			"  必须用 inset shadow 而**不是** border：.badge 没有 border，"+
			"加 border 会多占 2px，被限流的卡片比正常卡片高（实测 185 vs 183）。\n"+
			"  实际规则：%s", rule)
	}
	if reBorderDecl.MatchString(rule) {
		t.Errorf(".acct-modellimit 里出现了 border 声明（%q）—— 它会多占 2px 布局空间，"+
			"破坏「卡片等宽等高」（该用 box-shadow: inset 画描边）。实际规则：%s",
			reBorderDecl.FindString(rule), rule)
	}
}
