package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/router"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// ─────────────────────────────────────────────────────────────
// /api/stats 的 daily 日时间序列护栏（2026-10-09）
//
// 委托方要求：在「模型用量」「账号用量」两张表旁边再加两张图 ——
// 一张总 token 随时间变化（面积/折线，y 轴按亿），
// 一张每账号一条折线的多线图（图例 = 昵称）。
//
// 🔴 前端是**照着契约**写的，字段名对不上就是空白图。所以这里：
//  1. 钉死字段名与形状（TestStatsDailyJSONContract）
//  2. 钉死"所有数组等长"这条硬不变量（图错位就全废）
//  3. 钉死 total = prompt + completion（堆叠图自洽的前提）
//  4. 钉死"零用量的日子也在横轴上"（否则折线会画成假增长）
// ─────────────────────────────────────────────────────────────

// dailyTestEnv 起一个带用量存储 + 账号表的服务。
//
// 用账号表是为了验证图例名（昵称优先 / 缺失时退回脱敏 UID）。
func dailyTestEnv(t *testing.T, store *usagepkg.Store, accounts *auth.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux(Deps{
		Logger:   log.New(io.Discard, "", 0),
		Usage:    store,
		Accounts: accounts,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fetchDaily 请求 /api/stats 并返回 daily 部分 + 原始 body。
func fetchDaily(t *testing.T, baseURL, query string) (dailyStats, string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/stats" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var got statsResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, raw)
	}
	return got.Daily, string(raw)
}

// TestStatsDailyArrayLengthsMatch 是本轮最重要的护栏。
//
// 🔴 硬不变量：dates / labels / total_tokens / prompt_tokens /
// completion_tokens 长度全部相等，且每个账号的 values 也相等。
//
//	少一格 => 折线整体错位一格（数据画在隔壁那天），
//	而且**不会报错**，只会显示一张看着合理但完全错的图。
//	这类错误靠肉眼极难发现，所以必须由测试钉死。
func TestStatsDailyArrayLengthsMatch(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// 三天前、两天前、今天各写一条 —— 中间留一天空着（见下一条测试）。
	now := time.Now()
	for i, day := range []int{3, 2, 0} {
		ev := usagepkg.Event{
			Time:             now.AddDate(0, 0, -day),
			Account:          "acct…",
			Model:            "workbuddy/glm-5.3",
			Protocol:         "chat",
			OK:               true,
			UsageKnown:       true,
			PromptTokens:     int64(10 * (i + 1)),
			CompletionTokens: int64(5 * (i + 1)),
			TotalTokens:      int64(15 * (i + 1)),
		}
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	// days=4 => 横轴应是「三天前、前两天、昨天、今天」
	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=4")

	if len(d.Dates) != 4 {
		t.Fatalf("dates 长度 = %d，期望 4（days 窗口）；dates=%v body=%s",
			len(d.Dates), d.Dates, body)
	}

	n := len(d.Dates)
	checks := []struct {
		name string
		got  int
	}{
		{"labels", len(d.Labels)},
		{"total_tokens", len(d.TotalTokens)},
		{"prompt_tokens", len(d.PromptTokens)},
		{"completion_tokens", len(d.CompletionTokens)},
	}
	for _, c := range checks {
		if c.got != n {
			t.Errorf("%s 长度 = %d，期望 %d（与 dates 等长是硬不变量）",
				c.name, c.got, n)
		}
	}
	for _, a := range d.Accounts {
		if len(a.Values) != n {
			t.Errorf("账号 %s 的 values 长度 = %d，期望 %d",
				a.Account, len(a.Values), n)
		}
	}
	// 🔴 逐模型折线与逐账号折线受同一条硬不变量约束（2026-10-09 加）。
	for _, m := range d.Models {
		if len(m.Values) != n {
			t.Errorf("模型 %s 的 values 长度 = %d，期望 %d",
				m.Model, len(m.Values), n)
		}
	}
}

// TestStatsDailyTotalEqualsPromptPlusCompletion 守堆叠图的自洽性。
//
// 🔴 total_tokens[i] 必须**恒等于** prompt+completion。
//
//	前端把输入/输出堆起来当总柱子：若 total 另取一个来源
//	（例如累加 ev.TotalTokens），一旦上游给的 total 与两部分不一致，
//	图上就会出现"总柱子 ≠ 两段之和"的自相矛盾。
func TestStatsDailyTotalEqualsPromptPlusCompletion(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// ⚠️ 故意让 TotalTokens **不等于** prompt+completion（上游真会这样），
	//	以此证明 daily 用的是"由两部分求和"的自洽口径，
	//	而不是照抄 ev.TotalTokens。
	now := time.Now()
	events := []usagepkg.Event{
		{Time: now, Account: "a…", Model: "m", OK: true, UsageKnown: true,
			PromptTokens: 100, CompletionTokens: 20, TotalTokens: 999},
		{Time: now.AddDate(0, 0, -1), Account: "a…", Model: "m", OK: true,
			UsageKnown: true, PromptTokens: 7, CompletionTokens: 3, TotalTokens: 0},
	}
	for _, ev := range events {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=3")

	for i := range d.Dates {
		want := d.PromptTokens[i] + d.CompletionTokens[i]
		if d.TotalTokens[i] != want {
			t.Errorf("第 %d 天（%s）total_tokens = %d，期望 %d "+
				"(prompt %d + completion %d)",
				i, d.Dates[i], d.TotalTokens[i], want,
				d.PromptTokens[i], d.CompletionTokens[i])
		}
	}

	// 今天那格必须是 100+20=120，**不是** 999（照抄 ev.TotalTokens 就会得到 999）
	today := d.Dates[len(d.Dates)-1]
	if got := d.TotalTokens[len(d.TotalTokens)-1]; got != 120 {
		t.Errorf("今天的 total_tokens = %d，期望 120 —— "+
			"daily 必须用 prompt+completion 的自洽口径（body=%s）", got, body)
	}
	if len(d.Dates) > 0 && d.Dates[len(d.Dates)-1] != today {
		t.Errorf("dates 末尾 = %q，期望今天 %q", d.Dates[len(d.Dates)-1], today)
	}
}

// TestStatsDailyIncludesZeroUsageDays 守横轴的连续性。
//
// 🔴 中间没用过的日子**必须也在轴上**（值为 0）。
//
//	若跳过零用量的日子，横轴就会把"10-05 → 10-08"画成相邻两点，
//	折线看起来是"平滑增长"，而事实是中间三天完全没用 —— 图形撒谎。
func TestStatsDailyIncludesZeroUsageDays(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// 只写「两天前」一条。昨天与今天都没数据。
	now := time.Now()
	if err := store.Append(usagepkg.Event{
		Time: now.AddDate(0, 0, -2), Account: "a…", Model: "m", OK: true,
		UsageKnown: true, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}); err != nil {
		t.Fatal(err)
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=3")

	if len(d.Dates) != 3 {
		t.Fatalf("dates 长度 = %d，期望 3（含零用量的日子）；body=%s", len(d.Dates), body)
	}
	// 三天前那格有数据
	if d.TotalTokens[0] != 15 {
		t.Errorf("两天前那格 total_tokens = %d，期望 15", d.TotalTokens[0])
	}
	// 昨天、今天必须**存在且为 0**（不是被跳过）
	for _, i := range []int{1, 2} {
		if d.TotalTokens[i] != 0 {
			t.Errorf("第 %d 天（%s）total_tokens = %d，期望 0",
				i, d.Dates[i], d.TotalTokens[i])
		}
		if d.PromptTokens[i] != 0 || d.CompletionTokens[i] != 0 {
			t.Errorf("第 %d 天（%s）输入/输出应为 0，实际 %d/%d",
				i, d.Dates[i], d.PromptTokens[i], d.CompletionTokens[i])
		}
	}
	// 零用量的日子也必须有横轴标签
	for i, l := range d.Labels {
		if l == "" {
			t.Errorf("第 %d 天缺少短标签（labels[%d] 为空）", i, i)
		}
	}

	// 反向对照：若只返回"有用量的日子"，长度会是 1 —— 那样本测试就该红。
	if len(d.Dates) == 1 {
		t.Error("只返回了有用量的日子 —— 横轴不连续（折线会撒谎）")
	}
}

// TestStatsDailyAscendingAndTodayLast 守横轴方向与"今天在最右"。
//
// 🔴 前端按数组顺序从左到右画。倒序会让图整个左右反过来 ——
//
//	同样"看着合理但完全错"。
func TestStatsDailyAscendingAndTodayLast(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	if err := store.Append(usagepkg.Event{
		Time: time.Now(), Account: "a…", Model: "m", OK: true, TotalTokens: 1,
	}); err != nil {
		t.Fatal(err)
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=5")

	if len(d.Dates) != 5 {
		t.Fatalf("dates 长度 = %d，期望 5；body=%s", len(d.Dates), body)
	}
	// 升序（字符串比较对 ISO 日期等价于时间序）
	for i := 1; i < len(d.Dates); i++ {
		if d.Dates[i-1] >= d.Dates[i] {
			t.Errorf("dates 非升序：dates[%d]=%q >= dates[%d]=%q",
				i-1, d.Dates[i-1], i, d.Dates[i])
		}
	}
	// 最后一格必须是今天（UTC+8）
	wantToday := time.Now().In(cnZone).Format("2006-01-02")
	if got := d.Dates[len(d.Dates)-1]; got != wantToday {
		t.Errorf("dates 末尾 = %q，期望今天 %q（UTC+8）", got, wantToday)
	}
	// 短标签 = ISO 去掉年份
	if got := d.Labels[len(d.Labels)-1]; got != wantToday[5:] {
		t.Errorf("labels 末尾 = %q，期望 %q", got, wantToday[5:])
	}
}

// TestStatsDailyAccountsLegendName 守图例名：昵称优先，查不到退回脱敏 UID。
//
// 🔴 与账号用量表**同一条规则**（同一个 lookupAccountByMasked）——
//
//	两处显示的账号名不能分叉，否则用户对不上"图上这条线是哪个号"。
//
// 🔴 且绝不能出现完整 UID：只允许"昵称"或"脱敏值"。
func TestStatsDailyAccountsLegendName(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	accounts := auth.NewStore()

	uid := "11111111-2222-3333-4444-555555555555"
	accounts.Put(&auth.Account{UID: uid, Nickname: "13800000001"})
	masked := auth.MaskUID(uid)

	// 两个账号：一个有昵称，一个账号表里查不到（模拟已删除的账号）
	now := time.Now()
	events := []usagepkg.Event{
		{Time: now, Account: masked, Model: "m", OK: true,
			UsageKnown: true, PromptTokens: 300, CompletionTokens: 0, TotalTokens: 300},
		{Time: now, Account: "deadbeef…", Model: "m", OK: true,
			UsageKnown: true, PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10},
	}
	for _, ev := range events {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	srv := dailyTestEnv(t, store, accounts)
	d, body := fetchDaily(t, srv.URL, "?days=1")

	if len(d.Accounts) != 2 {
		t.Fatalf("账号折线数 = %d，期望 2；body=%s", len(d.Accounts), body)
	}

	// 排序：按窗口内总 token 降序 ⇒ 有昵称那个（300）在前
	if d.Accounts[0].Name != "13800000001" {
		t.Errorf("第一条折线 name = %q，期望昵称 13800000001（图例要可读）",
			d.Accounts[0].Name)
	}
	if d.Accounts[0].Account != masked {
		t.Errorf("第一条折线 account = %q，期望脱敏值 %q",
			d.Accounts[0].Account, masked)
	}
	// 查不到的账号：name 退回脱敏值（不能是空串 —— 图例会成空白项）
	if d.Accounts[1].Name != "deadbeef…" {
		t.Errorf("已删账号的 name = %q，期望退回脱敏值 deadbeef…",
			d.Accounts[1].Name)
	}

	// 🔴 红线：响应里绝不能出现完整 UID
	if len(uid) > 0 && strings.Contains(body, uid) {
		t.Error("响应含完整 UID —— 账号身份只能以脱敏值 + 昵称出现")
	}
}

// TestStatsDailyJSONContract 钉死前端依赖的字段名与形状。
//
// 🔴 前端是照着契约写的，字段名一改就是空白图（且不报错）。
// 这条测试把契约**写进代码**：改名即红。
func TestStatsDailyJSONContract(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	accounts := auth.NewStore()
	uid := "99999999-1111-2222-3333-444444444444"
	accounts.Put(&auth.Account{UID: uid, Nickname: "昵称A"})

	if err := store.Append(usagepkg.Event{
		Time: time.Now(), Account: auth.MaskUID(uid), Model: "m", OK: true,
		UsageKnown: true, PromptTokens: 1000, CompletionTokens: 234, TotalTokens: 1234,
	}); err != nil {
		t.Fatal(err)
	}

	srv := dailyTestEnv(t, store, accounts)
	raw, _ := func() (string, int) {
		resp, err := http.Get(srv.URL + "/api/stats?days=1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode
	}()

	// 用 map 解析：断言的是**原始 JSON 的键名**，不受 Go 标签影响
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("顶层解析失败: %v\n%s", err, raw)
	}
	dailyRaw, ok := top["daily"]
	if !ok {
		t.Fatalf("响应缺少顶层字段 daily；body=%s", raw)
	}
	// 不能是 null（前端无条件读 d.daily.dates）
	if string(dailyRaw) == "null" {
		t.Fatal("daily 为 null —— 前端读 d.daily.dates 会抛异常")
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(dailyRaw, &obj); err != nil {
		t.Fatalf("daily 不是对象: %v\n%s", err, raw)
	}
	for _, key := range []string{
		"dates", "labels", "total_tokens", "prompt_tokens",
		"completion_tokens", "accounts", "models",
	} {
		if _, ok := obj[key]; !ok {
			t.Errorf("daily 缺少字段 %q（前端按这个名字读）", key)
		}
	}

	// accounts 元素形状
	var accs []map[string]json.RawMessage
	if err := json.Unmarshal(obj["accounts"], &accs); err != nil {
		t.Fatalf("accounts 不是数组: %v", err)
	}
	if len(accs) != 1 {
		t.Fatalf("accounts 数 = %d，期望 1", len(accs))
	}
	for _, key := range []string{"name", "account", "values"} {
		if _, ok := accs[0][key]; !ok {
			t.Errorf("accounts[0] 缺少字段 %q", key)
		}
	}

	// models 元素形状（2026-10-09 加）：只有 model + values 两个字段，
	// **刻意没有 name** —— 见 dailyModelSeries 的注释（模型 ID 就是图例名）。
	var mods []map[string]json.RawMessage
	if err := json.Unmarshal(obj["models"], &mods); err != nil {
		t.Fatalf("models 不是数组: %v", err)
	}
	if len(mods) != 1 {
		t.Fatalf("models 数 = %d，期望 1（body=%s）", len(mods), raw)
	}
	for _, key := range []string{"model", "values"} {
		if _, ok := mods[0][key]; !ok {
			t.Errorf("models[0] 缺少字段 %q", key)
		}
	}
	if _, ok := mods[0]["name"]; ok {
		t.Error("models[0] 出现了 name 字段 —— 模型折线的图例就是 model，" +
			"多一个可与之不一致的字段没有信息增益")
	}

	// 实际值：1000 输入 + 234 输出 = 1234
	var d dailyStats
	if err := json.Unmarshal(dailyRaw, &d); err != nil {
		t.Fatal(err)
	}
	if d.PromptTokens[0] != 1000 || d.CompletionTokens[0] != 234 ||
		d.TotalTokens[0] != 1234 {
		t.Errorf("daily 数值不符: prompt=%d completion=%d total=%d",
			d.PromptTokens[0], d.CompletionTokens[0], d.TotalTokens[0])
	}
	if d.Accounts[0].Values[0] != 1234 {
		t.Errorf("账号折线值 = %d，期望 1234", d.Accounts[0].Values[0])
	}
	// 模型折线用**同一个自洽口径**（prompt + completion），不是 ev.TotalTokens。
	if len(d.Models) == 0 {
		t.Fatal("daily.models 为空 —— 逐模型折线没生成")
	}
	if d.Models[0].Values[0] != 1234 {
		t.Errorf("模型折线值 = %d，期望 1234（与账号折线、total_tokens 同口径）",
			d.Models[0].Values[0])
	}

	// 把真实样本打出来，便于人工核对契约（-v 时可见）
	t.Logf("daily 实际 JSON: %s", string(dailyRaw))
}

// TestStatsDailyBuilderMemoryIsBoundedByDays 守本项目最硬的一条约束：
// **内存不随记录条数增长**。
//
// 🔴 为什么值得一条测试（而不是只写注释）：
//
//	"把事件先收集起来再聚合"是最自然的写法，也是最容易被后来者
//	"顺手改成"的写法（比如为了算中位数/去重）。一旦那样改，
//	一天上万条就够把内存吃光 —— 而**没有任何功能会立刻变坏**，
//	所以不会有别的测试变红。这条测试是唯一会响的警报。
//
// 做法：喂进远超窗口天数的记录，然后用反射断言
// dailyBuilder 内部**每一个切片字段**的长度都不超过 days。
//
// ⚠️ 2026-10-09 补逐模型折线时对本测试的改动（以及为什么没被削弱）：
//
//	加了 field `modelValues []*dailyModel` 与 `modelIdx map[string]int`。
//	反射那一段**本来就自动覆盖新字段**（它扫所有字段，不写死名字），
//	所以 len(modelValues)=1 ≤ days 是通过的 —— 但"自动通过"不等于
//	"守住了"，因为这条只能证明"没存 5000 条明细"，证明不了
//	"每条折线的 values 长度是 days"。
//
//	⇒ 这里**补强**两处（只加不减）：
//	  ① 把 dailyModel 也纳入逐字段扫描（原先只扫 dailyAcct）；
//	  ② 新增硬不变量断言：**每个** acct/model 的 values 长度必须
//	     **恰好等于** days（不是 ≤）。少一格就是图错位，多一格也一样错。
//
//	⚠️ 诚实说明这条测试**覆盖不到**的部分：模型线的条数上界是
//	  "窗口内出现过的模型数"（本机 34 个），它由模型注册表封顶，
//	  不由 days 封顶。所以"模型数 ≤ days"在 days 很小时**并不天然成立**
//	  （days=1 且一天用了 3 个模型时，len(modelValues)=3 > days，反射那条
//	  断言的是**切片长度**而不是总内存）。这里不去伪造一个假的通过：
//	  内存真正的上界是 O(days × 实体数)，实体数有独立上界（账号数个位数、
//	  模型数几十个），与记录条数无关正是本测试要守的东西。
func TestStatsDailyBuilderMemoryIsBoundedByDays(t *testing.T) {
	const days = 7
	b := newDailyBuilder(days, time.Now())

	now := time.Now()
	// 5000 条：远多于 days，也远多于账号数/模型数。
	// 若实现里存了明细，这里就会留下 5000 个元素。
	for i := 0; i < 5000; i++ {
		b.add(usagepkg.Event{
			Time: now.AddDate(0, 0, -(i % days)), Account: "acct…",
			Model: "m", OK: true, UsageKnown: true,
			PromptTokens: 1, CompletionTokens: 2,
		}, "workbuddy/m")
	}

	// 累加结果正确（5000 × 3 = 15000）
	var sum int64
	for _, v := range b.total {
		sum += v
	}
	if sum != 15000 {
		t.Errorf("总量 = %d，期望 15000", sum)
	}

	// 🔴 结构性断言：反射扫 dailyBuilder 的所有字段，
	//	任何长度 > days 的切片都说明"记录被留下来了"。
	checkSlices := func(name string, v any) {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Struct {
			t.Fatalf("%s 不是结构体", name)
		}
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Field(i)
			if f.Kind() != reflect.Slice {
				continue
			}
			// 只判外层长度：切片元素本身是"每条定长"的
			if f.Len() > days {
				t.Errorf("%s.%s 长度 = %d，超过 days=%d —— "+
					"日聚合器的内存必须与**记录条数无关**",
					name, rv.Type().Field(i).Name, f.Len(), days)
			}
		}
	}
	// checkValues 断言"每条折线的 values 恰好 days 格"（硬不变量）。
	checkValues := func(name string, v any) {
		rv := reflect.ValueOf(v)
		f := rv.FieldByName("values")
		if !f.IsValid() {
			t.Fatalf("%s 没有 values 字段 —— 结构变了，本测试必须跟着改", name)
		}
		if f.Len() != days {
			t.Errorf("%s.values 长度 = %d，期望恰好 %d "+
				"（少一格或多一格都会让折线错位）", name, f.Len(), days)
		}
	}
	checkSlices("dailyBuilder", *b)
	for _, a := range b.accts {
		checkSlices("dailyAcct", *a)
		checkValues("dailyAcct", *a)
	}
	for _, m := range b.modelValues {
		checkSlices("dailyModel", *m)
		checkValues("dailyModel", *m)
	}

	// 账号数/模型数也是个位数（不随记录数增长）
	if len(b.accts) != 1 {
		t.Errorf("账号数 = %d，期望 1", len(b.accts))
	}
	if len(b.modelValues) != 1 {
		t.Errorf("模型数 = %d，期望 1（5000 条记录只是同一个模型）", len(b.modelValues))
	}
}

// TestStatsDailyNonNullWhenNoUsageStore 守"没有 store 时 daily 也不是 null"。
//
// 🔴 面板与 /api/stats 是分开开发的：没有 store 时若 daily 是 null，
//
//	前端读 d.daily.dates 会抛异常，整个统计页显示"加载失败"，
//	而不是正常的"暂无数据"。
func TestStatsDailyNonNullWhenNoUsageStore(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stats?days=7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, raw)
	}
	if string(top["daily"]) == "null" {
		t.Errorf("无 store 时 daily 为 null —— 前端会抛异常。body=%s", raw)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(top["daily"], &obj); err != nil {
		t.Fatalf("daily 不是对象: %v", err)
	}
	for _, key := range []string{
		"dates", "labels", "total_tokens", "prompt_tokens",
		"completion_tokens", "accounts", "models",
	} {
		v, ok := obj[key]
		if !ok {
			t.Errorf("无 store 时 daily 缺少字段 %q", key)
			continue
		}
		if string(v) == "null" {
			t.Errorf("无 store 时 daily.%s 为 null，期望空数组 []", key)
		}
	}
}

// TestStatsDailyOutOfWindowEventIgnored 守越界事件不会 panic。
//
// 🔴 ReadResult 只按 days 翻文件，理论上事件都落在窗口内；
//
//	但若有人手动补写旧文件、或系统时钟被改过，越界下标会让
//	**整个统计接口 panic**（panic 在 http handler 里 = 500 + 日志噪音）。
//	宁可少算一格，不可崩。
func TestStatsDailyOutOfWindowEventIgnored(t *testing.T) {
	b := newDailyBuilder(3, time.Now())
	// 30 天前 —— 远在窗口外
	b.add(usagepkg.Event{
		Time: time.Now().AddDate(0, 0, -30), Account: "old…",
		PromptTokens: 1, CompletionTokens: 1,
	}, "workbuddy/old")
	// 窗口内的今天
	b.add(usagepkg.Event{
		Time: time.Now(), Account: "now…", PromptTokens: 5, CompletionTokens: 5,
	}, "workbuddy/now")

	d := b.build(nil)
	if len(d.TotalTokens) != 3 {
		t.Fatalf("长度 = %d，期望 3", len(d.TotalTokens))
	}
	var sum int64
	for _, v := range d.TotalTokens {
		sum += v
	}
	if sum != 10 {
		t.Errorf("窗口内总量 = %d，期望 10（越界那条必须被丢弃）", sum)
	}
	if len(d.Accounts) != 1 {
		t.Errorf("账号数 = %d，期望 1（越界账号不该建折线）", len(d.Accounts))
	}
	// 🔴 模型折线同理：越界那条不能凭空造出一条线
	if len(d.Models) != 1 {
		t.Errorf("模型折线数 = %d，期望 1（越界模型不该建折线）", len(d.Models))
	}
	if len(d.Models) == 1 && d.Models[0].Model != "workbuddy/now" {
		t.Errorf("模型折线 = %q，期望 workbuddy/now", d.Models[0].Model)
	}
}

// TestStatsDailyAccountsOrderedByTotalDesc 守折线排序。
//
// 🔴 与 resp.Accounts 同一精神（用得多的在前）。
//
//	且必须**稳定**：同量时按脱敏 ID 定序 —— 若靠 map 迭代顺序，
//	每次刷新图例顺序都在跳，用户会以为数据在变。
func TestStatsDailyAccountsOrderedByTotalDesc(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	now := time.Now()
	events := []usagepkg.Event{
		{Time: now, Account: "small…", Model: "m", OK: true,
			PromptTokens: 1, CompletionTokens: 0},
		{Time: now, Account: "big…", Model: "m", OK: true,
			PromptTokens: 900, CompletionTokens: 0},
		{Time: now, Account: "mid…", Model: "m", OK: true,
			PromptTokens: 50, CompletionTokens: 0},
	}
	for _, ev := range events {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=1")

	if len(d.Accounts) != 3 {
		t.Fatalf("账号数 = %d，期望 3；body=%s", len(d.Accounts), body)
	}
	want := []string{"big…", "mid…", "small…"}
	for i, w := range want {
		if d.Accounts[i].Account != w {
			t.Errorf("第 %d 条折线 = %q，期望 %q（按总量降序）",
				i, d.Accounts[i].Account, w)
		}
	}
}

// containsStr 是 strings.Contains 的小包装（避免为本文件再加一个 import）。
func containsStr(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// ─────────────────────────────────────────────────────────────
// daily.models 逐模型折线护栏（2026-10-09）
//
// 委托方实测反馈：「模型用量那张图为什么只有一条 total 线」——
// 账号图是一号一线，模型图却只有总量，同一个页面上两套口径。
// 本节守的就是"模型图也一号一线"，并钉死它与模型用量表**同源**。
// ─────────────────────────────────────────────────────────────

// fetchStatsRaw 请求 /api/stats 并返回**整个**响应 + 原始 body。
//
// ⚠️ 与 fetchDaily 的区别：这里要同时看 daily.models 和顶层 models
//
//	（模型用量表），两者必须对得上 —— 那是本节的中心论点。
func fetchStatsRaw(t *testing.T, baseURL, query string) (statsResponse, string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/stats" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var got statsResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, raw)
	}
	return got, string(raw)
}

// TestStatsDailyModelsSeriesPerModel 是逐模型折线的主护栏。
//
// 守四件事（缺一条图就是错的）：
//  1. **一个模型一条线**（不是一条 total 线 —— 那正是委托方报的问题）
//  2. values 长度 == len(dates)（与账号折线同一条硬不变量）
//  3. 逐日数值真的落在**正确的那一天**（不是全堆在最后一格）
//  4. 顺序按窗口内总量**降序**（用得多的在前）
func TestStatsDailyModelsSeriesPerModel(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// 三天前：big 用了 300
	// 今天：    big 用了 30 + small 用了 5（两个模型同一天）
	now := time.Now()
	events := []usagepkg.Event{
		{Time: now.AddDate(0, 0, -2), Account: "a…",
			Model: "workbuddy/big", ProviderID: "workbuddy", ResolvedModel: "big",
			OK: true, UsageKnown: true, PromptTokens: 300, CompletionTokens: 0},
		{Time: now, Account: "a…",
			Model: "workbuddy/big", ProviderID: "workbuddy", ResolvedModel: "big",
			OK: true, UsageKnown: true, PromptTokens: 20, CompletionTokens: 10},
		{Time: now, Account: "a…",
			Model: "workbuddy/small", ProviderID: "workbuddy", ResolvedModel: "small",
			OK: true, UsageKnown: true, PromptTokens: 5, CompletionTokens: 0},
	}
	for _, ev := range events {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=3")

	if len(d.Models) != 2 {
		t.Fatalf("模型折线数 = %d，期望 2（一个模型一条线）；body=%s",
			len(d.Models), body)
	}

	// ② 长度不变量
	for _, m := range d.Models {
		if len(m.Values) != len(d.Dates) {
			t.Errorf("模型 %q 的 values 长度 = %d，期望 %d（与 dates 等长）",
				m.Model, len(m.Values), len(d.Dates))
		}
	}

	// ④ 排序：big 总 330 > small 总 5
	if d.Models[0].Model != "workbuddy/big" {
		t.Errorf("第一条模型折线 = %q，期望 workbuddy/big（按总量降序）",
			d.Models[0].Model)
	}
	if d.Models[1].Model != "workbuddy/small" {
		t.Errorf("第二条模型折线 = %q，期望 workbuddy/small", d.Models[1].Model)
	}

	// ③ 逐日数值
	if len(d.Models[0].Values) == 3 {
		// dates = [两天前, 昨天, 今天]，下标 0 是两天前
		if got := d.Models[0].Values[0]; got != 300 {
			t.Errorf("big 在两天前 = %d，期望 300", got)
		}
		if got := d.Models[0].Values[1]; got != 0 {
			t.Errorf("big 在昨天 = %d，期望 0（那天没用）", got)
		}
		if got := d.Models[0].Values[2]; got != 30 {
			t.Errorf("big 在今天 = %d，期望 30（20 输入 + 10 输出）", got)
		}
		if got := d.Models[1].Values[2]; got != 5 {
			t.Errorf("small 在今天 = %d，期望 5", got)
		}
	}

	// 🔴 反向对照：若实现只画一条总量线，len(d.Models) 会是 1 —— 上面已红。
	//	这里再显式点一句，免得将来有人把断言放宽时丢掉这个意图。
	if len(d.Models) == 1 {
		t.Error("只有一条模型折线 —— 这正是「模型图只有 total 线」那个 bug")
	}

	t.Logf("daily.models 实际 JSON: %s", modelsJSON(t, body))
}

// TestStatsDailyModelsOnlyInWindow 守"只含窗口内出现过的模型"。
//
// 🔴 为什么要（与账号同一条规则）：
//
//	补一条全 0 的线会让图例里出现该窗口内根本没用的模型，
//	图看上去"有 34 条线"，实际只有 2 个模型被调用过 —— 图在说谎。
func TestStatsDailyModelsOnlyInWindow(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	now := time.Now()

	// 今天用过的模型
	if err := store.Append(usagepkg.Event{
		Time: now, Account: "a…", Model: "workbuddy/recent",
		ProviderID: "workbuddy", ResolvedModel: "recent",
		OK: true, UsageKnown: true, PromptTokens: 10, CompletionTokens: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 10 天前的模型（days=3 的窗口外）
	if err := store.Append(usagepkg.Event{
		Time: now.AddDate(0, 0, -10), Account: "a…", Model: "workbuddy/ancient",
		ProviderID: "workbuddy", ResolvedModel: "ancient",
		OK: true, UsageKnown: true, PromptTokens: 9999, CompletionTokens: 0,
	}); err != nil {
		t.Fatal(err)
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=3")

	if len(d.Models) != 1 {
		t.Fatalf("模型折线数 = %d，期望 1（窗口内只有 recent 被用过）；body=%s",
			len(d.Models), body)
	}
	if d.Models[0].Model != "workbuddy/recent" {
		t.Errorf("模型折线 = %q，期望 workbuddy/recent", d.Models[0].Model)
	}
	// 窗口外的那个模型绝不能出现（哪怕是全 0 的补位线）
	if containsStr(body, `"workbuddy/ancient"`) {
		t.Error("窗口外的模型出现在了 daily.models —— 不该有全 0 补位线")
	}
	// 且它的 9999 token 不能漏进任何模型线
	var sum int64
	for _, v := range d.Models[0].Values {
		sum += v
	}
	if sum != 11 {
		t.Errorf("recent 的合计 = %d，期望 11（窗口外那 9999 不能混进来）", sum)
	}
}

// TestStatsDailyModelsKeyMatchesAggregateTable 是本任务最关键的一条：
// **模型折线的 key 必须与模型用量表的行 key 完全一致**。
//
// 🔴 为什么这比"值对不对"更要紧：
//
//	图表存在的意义就是回答"表里这一行是哪条线"。若两处各自归一化
//	（或一处用 ev.Model 原文），就会出现**表里有这行、图上没这条线**，
//	而且**不报错**、只是图上少一条 —— 肉眼极难发现。
//
// 做法：用三种**客户端写法不同**的记录（别名 / 裸 ID / 完整 ID，
// 这是 2026-10-06 真实踩过的坑）打同一个模型，然后断言
// 表里恰好一行、图里恰好一条线、且两者的字符串完全相同。
func TestStatsDailyModelsKeyMatchesAggregateTable(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// 老式记录（没有 ProviderID/ResolvedModel）—— 走 CanonicalModelID 回退，
	// 所以必须有个带别名表的 Router 才能把三种写法归一。
	r := router.New()
	if err := r.Register(context.Background(), &stubProvider{
		id: "workbuddy",
		models: []provider.Model{
			{ID: "workbuddy/deepseek-v4.1-flash",
				UpstreamID: "deepseek-v4.1-flash", Name: "DeepSeek"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetAliases(map[string]string{
		"dsf": "workbuddy/deepseek-v4.1-flash",
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	const canonical = "workbuddy/deepseek-v4.1-flash"
	for i, form := range []string{
		"dsf",                           // 别名
		"deepseek-v4.1-flash",           // 裸 ID
		"workbuddy/deepseek-v4.1-flash", // 标准形态
	} {
		if err := store.Append(usagepkg.Event{
			Time: now, Account: "a…", Model: form,
			OK: true, UsageKnown: true,
			PromptTokens: int64(10 * (i + 1)), CompletionTokens: 0,
			// ⚠️ 必须同时给 TotalTokens：模型的**汇总表**口径是累加
			//	ev.TotalTokens（既有行为，本次不许改），而 daily 折线
			//	用的是 prompt+completion 的自洽口径。真实上游两条都给，
			//	所以这里也照实给 —— 否则测出来的是"两种口径本来就不同"，
			//	而不是本条要守的"key 是否同源"。
			TotalTokens: int64(10 * (i + 1)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(newMux(Deps{
		Logger: log.New(io.Discard, "", 0),
		Usage:  store,
		Router: r,
	}))
	defer srv.Close()

	got, body := fetchStatsRaw(t, srv.URL, "?days=1")

	// 表：三种写法必须合成**一行**
	if len(got.Models) != 1 {
		t.Fatalf("模型用量表行数 = %d，期望 1（三种写法是同一个模型）；body=%s",
			len(got.Models), body)
	}
	if got.Models[0].Model != canonical {
		t.Fatalf("表里的模型 key = %q，期望 %q", got.Models[0].Model, canonical)
	}

	// 图：也必须只有**一条**线，且 key 与表**逐字节相同**
	if len(got.Daily.Models) != 1 {
		t.Fatalf("模型折线条数 = %d，期望 1；body=%s", len(got.Daily.Models), body)
	}
	if got.Daily.Models[0].Model != got.Models[0].Model {
		t.Errorf("模型折线 key = %q 与模型用量表 key = %q 不一致 —— "+
			"表里有这行、图上就会没有这条线（且不报错）",
			got.Daily.Models[0].Model, got.Models[0].Model)
	}
	if got.Daily.Models[0].Model != canonical {
		t.Errorf("模型折线 key = %q，期望 %q", got.Daily.Models[0].Model, canonical)
	}

	// 值也要对得上：10+20+30 = 60，表与图两处同值
	if got.Models[0].TotalTokens != 60 {
		t.Errorf("表里 total_tokens = %d，期望 60", got.Models[0].TotalTokens)
	}
	var lineSum int64
	for _, v := range got.Daily.Models[0].Values {
		lineSum += v
	}
	if lineSum != 60 {
		t.Errorf("折线合计 = %d，期望 60（与表同源）", lineSum)
	}

	// 图与表**同长**（硬不变量）
	if len(got.Daily.Models[0].Values) != len(got.Daily.Dates) {
		t.Errorf("models[0].values 长度 = %d，期望 %d",
			len(got.Daily.Models[0].Values), len(got.Daily.Dates))
	}
}

// TestStatsDailyModelsKeyUsesRoutedIdentity 守：新记录用**路由当时的身份**
// （ProviderID + ResolvedModel），与模型用量表同一规则。
//
// 🔴 为什么值得单独测：这条路径不经过别名表/注册表，是 resolveModelKey
//
//	的第一优先级分支。若 daily 那边错误地用了 ev.Model 原文，
//	这里就会露馅（原文是客户端写的 "dsf"，与表里的完整 ID 不同）。
func TestStatsDailyModelsKeyUsesRoutedIdentity(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	now := time.Now()
	// 客户端写别名 "dsf"，但请求当时路由到 workbuddy/deepseek-v4.1-flash
	if err := store.Append(usagepkg.Event{
		Time: now, Account: "a…", Model: "dsf",
		ProviderID: "workbuddy", ResolvedModel: "deepseek-v4.1-flash",
		OK: true, UsageKnown: true, PromptTokens: 7, CompletionTokens: 3,
	}); err != nil {
		t.Fatal(err)
	}

	// ⚠️ 刻意**不给** Router：证明新记录不需要任何注册表/别名表，
	//	用的就是记录里带的路由身份（接入第二平台后老记录归属不会被改写）。
	srv := dailyTestEnv(t, store, nil)
	got, body := fetchStatsRaw(t, srv.URL, "?days=1")

	const want = "workbuddy/deepseek-v4.1-flash"
	if len(got.Models) != 1 || got.Models[0].Model != want {
		t.Fatalf("模型用量表 = %+v，期望一行 %q；body=%s", got.Models, want, body)
	}
	if len(got.Daily.Models) != 1 {
		t.Fatalf("模型折线条数 = %d，期望 1；body=%s", len(got.Daily.Models), body)
	}
	if got.Daily.Models[0].Model != want {
		t.Errorf("模型折线 key = %q，期望 %q（路由身份，不是客户端原文 %q）",
			got.Daily.Models[0].Model, want, "dsf")
	}
	if containsStr(body, `"model":"dsf"`) {
		t.Error("daily.models 里出现了客户端原文 —— 必须用归一化后的 ID")
	}
}

// TestStatsDailyModelsOrderedByTotalDesc 守折线排序（含同量时的**稳定性**）。
//
// 🔴 为什么"稳定"也要测：若排序只看 sum 不看 key，
//
//	map 迭代顺序会让每次刷新的图例顺序都变，用户会以为数据在变。
func TestStatsDailyModelsOrderedByTotalDesc(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	now := time.Now()
	// tiny/a… 与 mid/b… 刻意**同量**（都是 50）—— 用来验证 tie-break。
	for _, ev := range []usagepkg.Event{
		{Time: now, Model: "workbuddy/small", ProviderID: "workbuddy",
			ResolvedModel: "small", OK: true, PromptTokens: 1, CompletionTokens: 0},
		{Time: now, Model: "workbuddy/big", ProviderID: "workbuddy",
			ResolvedModel: "big", OK: true, PromptTokens: 900, CompletionTokens: 0},
		{Time: now, Model: "workbuddy/aaa", ProviderID: "workbuddy",
			ResolvedModel: "aaa", OK: true, PromptTokens: 50, CompletionTokens: 0},
		{Time: now, Model: "workbuddy/zzz", ProviderID: "workbuddy",
			ResolvedModel: "zzz", OK: true, PromptTokens: 50, CompletionTokens: 0},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	srv := dailyTestEnv(t, store, nil)
	d, body := fetchDaily(t, srv.URL, "?days=1")

	if len(d.Models) != 4 {
		t.Fatalf("模型折线数 = %d，期望 4；body=%s", len(d.Models), body)
	}
	// big(900) 最前；aaa/zzz 同量 50 ⇒ 按 key 升序 aaa 在 zzz 前；small(1) 最后
	want := []string{
		"workbuddy/big", "workbuddy/aaa", "workbuddy/zzz", "workbuddy/small",
	}
	for i, w := range want {
		if d.Models[i].Model != w {
			t.Errorf("第 %d 条模型折线 = %q，期望 %q（按总量降序，同量按 ID 升序）",
				i, d.Models[i].Model, w)
		}
	}
}

// TestStatsDailyModelsNonNullInEmptyWindow 守"没有数据时 models 是 [] 不是 null"。
//
// 🔴 与 accounts 同一理由：前端会无条件 .forEach，
//
//	null 会让整个统计页抛异常（显示"加载失败"而不是"暂无数据"）。
//
// ⚠️ 断言必须**限定在 daily 里**（第一版写成全串匹配 `"models":null`，
//
//	结果匹配到的是**顶层**那个 models —— 它在空窗口下本来就是 null
//	（既有行为，不属于本次改动范围）。那样的断言会永远红，
//	而且红在一个跟本改动无关的地方。必须解析出 daily 再判。
func TestStatsDailyModelsNonNullInEmptyWindow(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	srv := dailyTestEnv(t, store, nil)
	_, body := fetchStatsRaw(t, srv.URL, "?days=3")

	got := modelsJSON(t, body)
	if got != "[]" {
		t.Errorf("空窗口下 daily.models = %s，期望 []（非 null —— 前端 .forEach 会抛异常）；body=%s",
			got, body)
	}

	// 对照：daily.accounts 一直是 []，两者应当一致（同一族字段）
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	var daily map[string]json.RawMessage
	if err := json.Unmarshal(top["daily"], &daily); err != nil {
		t.Fatal(err)
	}
	if string(daily["accounts"]) != string(daily["models"]) {
		t.Errorf("daily.accounts = %s 而 daily.models = %s —— "+
			"同族字段的空值表达应当一致",
			daily["accounts"], daily["models"])
	}
}

// modelsJSON 从完整响应体里抠出 daily.models 的原始 JSON 片段。
//
// 为什么不直接重新序列化 got.Daily.Models：那样看到的是**我方的结构体**，
// 而不是服务端真正发出去的字节 —— 契约测试就该看后者。
func modelsJSON(t *testing.T, body string) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("顶层解析失败: %v", err)
	}
	var daily map[string]json.RawMessage
	if err := json.Unmarshal(top["daily"], &daily); err != nil {
		t.Fatalf("daily 解析失败: %v", err)
	}
	return string(daily["models"])
}
