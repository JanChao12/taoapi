package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// 本文件守 2026-10-09 委托方反馈的五项面板需求。
//
// 委托方原话：
//
//	「把模型用量和账号用量的积分消耗改为积分。
//	  为什么账号用量的缓存命中率没有显示，如果获取不到信息就删了不显示。
//	  调用记录和模型用量的输入输出后面都加一个合计。
//	  能不能获取到首字耗时和流信息。」

// ─────────────────────────────────────────────────────────────
// ① 账号用量的缓存命中率（委托方实测"没显示"）
// ─────────────────────────────────────────────────────────────

// TestAccountStatHasCacheHitRate 守：账号统计**必须**带算好的 cache_hit_rate。
//
// 🔴 这是委托方实测报的 bug：后端只给了分子分母两个原始值，
//
//	**没给算好的比率**，而前端读 `a.cache_hit_rate` ⇒ 永远 undefined
//	⇒ 每行显示「—」。数据其实一直在（实测命中 1.09 亿 / 未命中 115 万）。
//
// 本测试从 **JSON 字段名**层面断言 —— 那是前后端的契约点，
// 光测 Go 结构体字段存在还不够（有人改了 json tag 一样会坏）。
func TestAccountStatHasCacheHitRate(t *testing.T) {
	rate := 0.5
	as := accountStat{
		Account:         "a",
		CacheHitTokens:  100,
		CacheMissTokens: 100,
		CacheHitRate:    &rate,
	}
	b, err := json.Marshal(as)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["cache_hit_rate"]; !has {
		t.Fatal("accountStat 序列化后没有 cache_hit_rate 字段 —— " +
			"前端读的就是这个名字，缺了它账号用量的命中率永远显示「—」")
	}
	if got["cache_hit_rate"] != 0.5 {
		t.Errorf("cache_hit_rate = %v，期望 0.5", got["cache_hit_rate"])
	}
	// 分子分母也要保留（面板可能用于 tooltip / 排查）
	for _, k := range []string{"cache_hit_tokens", "cache_miss_tokens"} {
		if _, has := got[k]; !has {
			t.Errorf("accountStat 缺 %s", k)
		}
	}
}

// TestAccountCacheHitRateNilWhenNoSamples 守：无样本时是 **null 不是 0**。
//
// 🔴 语义区分（与 modelStat 同一口径，两处不能分叉）：
//
//	null = 该账号没被调用过 / 上游没报缓存 ⇒ 显示「—」
//	0    = 调用过但一次都没命中（真实 0%）   ⇒ 显示「0.0%」
//
//	把前者显示成 0% 会让人以为"缓存完全没生效"，是误导。
func TestAccountCacheHitRateNilWhenNoSamples(t *testing.T) {
	as := accountStat{Account: "a"} // 没有任何缓存样本
	b, err := json.Marshal(as)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	v, has := got["cache_hit_rate"]
	if !has {
		t.Fatal("cache_hit_rate 字段缺失 —— 前端无法区分「无样本」与「0%」")
	}
	if v != nil {
		t.Errorf("无样本时 cache_hit_rate = %v，期望 null —— "+
			"0 会被显示成 0.0%%（真实命中率零），与「没有数据」含义不同", v)
	}
}

// TestStatsAggregatesAccountCacheRate 守：聚合逻辑真的算出了账号命中率。
//
// 🔴 只测结构体字段存在是不够的 —— 必须测"聚合时赋值了"。
//
//	否则字段在、值恒为 nil，前端还是显示「—」。
func TestStatsAggregatesAccountCacheRate(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// 造两条带缓存数据的事件（命中 3 / 未命中 1 ⇒ 75%）
	//
	// ⚠️ Time 必须设成**当前时间**：Append 按 ev.Time 决定写入哪个
	//	日期文件，ReadResult(days=1) 只读今天那个文件。
	//	Time 留零值会写到 0001-01-01.jsonl ⇒ 统计读到空、测试假失败。
	now := time.Now()
	for _, ev := range []usagepkg.Event{
		{Time: now, Account: "acct…", Model: "m", OK: true, Status: "ok",
			UsageKnown: true, CacheHitTokens: 2, CacheMissTokens: 0},
		{Time: now, Account: "acct…", Model: "m", OK: true, Status: "ok",
			UsageKnown: true, CacheHitTokens: 1, CacheMissTokens: 1},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	srv := newPanelServer(t, store)
	code, body := doJSON(t, "GET", srv.URL+"/api/stats?days=1", "", nil)
	if code != 200 {
		t.Fatalf("状态码 = %d，body=%s", code, body)
	}
	var resp struct {
		Accounts []struct {
			Account        string   `json:"account"`
			CacheHitRate   *float64 `json:"cache_hit_rate"`
			CacheHitTokens int64    `json:"cache_hit_tokens"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, body)
	}
	if len(resp.Accounts) == 0 {
		t.Fatalf("没有任何账号统计 —— 事件没写进去？body=%s", body)
	}
	a := resp.Accounts[0]
	if a.CacheHitRate == nil {
		t.Fatal("账号 cache_hit_rate 为 null —— " +
			"聚合时没算它，前端会一直显示「—」（这正是委托方报的 bug）")
	}
	// 命中 3 / (3+1) = 0.75
	if *a.CacheHitRate < 0.74 || *a.CacheHitRate > 0.76 {
		t.Errorf("cache_hit_rate = %v，期望 ≈0.75（3 命中 / 4 总）", *a.CacheHitRate)
	}
}

// TestModelStatHasTotalTokens 守：模型统计带 total_tokens（「合计」列的数据源）。
//
// 🔴 前端「合计」列直接读后端的 total_tokens，不前端相加 ——
//
//	相加会掩盖"上游没给 usage"的差异（那种记录 token 是 0）。
func TestModelStatHasTotalTokens(t *testing.T) {
	ms := modelStat{Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	b, _ := json.Marshal(ms)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["total_tokens"] != float64(15) {
		t.Errorf("modelStat.total_tokens = %v，期望 15（「合计」列的数据源）",
			got["total_tokens"])
	}
}

// ─────────────────────────────────────────────────────────────
// ④ 首字耗时（TTFT）
// ─────────────────────────────────────────────────────────────

// TestRecorderCapturesTTFT 守：记账器在第一个内容事件时记下首字耗时。
func TestRecorderCapturesTTFT(t *testing.T) {
	rec := newUsageRecorderFor(Deps{}, "m", "up", true, "acct", "prov", "chat")
	if rec.ttftMS != nil {
		t.Fatal("初始就已有首字耗时 —— 应该等到第一个内容事件才记")
	}
	rec.noteFirstToken()
	if rec.ttftMS == nil {
		t.Fatal("noteFirstToken 之后仍是 nil —— 首字耗时没被记下")
	}
	if *rec.ttftMS < 0 {
		t.Errorf("首字耗时 = %d，不应为负", *rec.ttftMS)
	}
	// base() 必须把它带进事件
	ev := rec.base()
	if ev.TTFTMS == nil {
		t.Error("base() 没带 TTFTMS —— 首字耗时不会落盘")
	}
}

// TestNoteFirstTokenIsIdempotent 守：只认第一次。
//
// 🔴 为什么必须幂等：上游一条流会发几十个内容事件，
//
//	若每次都覆盖，记下的是"最后一个 token 的时刻"，
//	那就变成总耗时了 —— 首字耗时彻底失去意义。
//
// ⚠️ 怎么才能**真的**测出这件事（第一版测试是假的，被反向对照抓到）：
//
//	第一版写成"连调 50 次，断言值不变"。那是**无效断言** ——
//	快机器上 50 次调用都落在同一毫秒内，即使实现真的是"每次覆盖"，
//	算出来的值也一样，测试恒绿（反向对照显示它仍然通过）。
//
//	正确做法：**人为改变基准时间**（r.start 往前推 10 秒）。
//	这样"重新计算"必然得出不同结果 —— 覆盖语义立刻露馅。
func TestNoteFirstTokenIsIdempotent(t *testing.T) {
	rec := newUsageRecorderFor(Deps{}, "m", "up", true, "acct", "prov", "chat")
	rec.noteFirstToken()
	if rec.ttftMS == nil {
		t.Fatal("未记录首字耗时")
	}
	first := *rec.ttftMS

	// 关键一步：把基准时间往前挪 10 秒。
	// 之后再调 noteFirstToken，若实现是"每次重新计算并覆盖"，
	// 得到的就是 first+10000 而不是 first。
	rec.start = rec.start.Add(-10 * time.Second)
	rec.noteFirstToken()

	if *rec.ttftMS != first {
		t.Errorf("首字耗时被后续调用改写：%d → %d —— "+
			"每次覆盖会让它变成「最后一个 token 的时刻」，等于总耗时",
			first, *rec.ttftMS)
	}

	// 再连调几次，同样不该变
	for i := 0; i < 50; i++ {
		rec.noteFirstToken()
	}
	if *rec.ttftMS != first {
		t.Errorf("首字耗时被后续调用改写：%d → %d", first, *rec.ttftMS)
	}
}

// TestTTFTNilForNonStream 守：非流式请求不采集首字耗时（留 nil）。
//
// 🔴 理由：非流式要收齐才返回，"首字"与"完成"是同一刻，
//
//	报一个首字耗时是**编造**语义。前端据此退回只显示总耗时。
func TestTTFTNilForNonStream(t *testing.T) {
	rec := newUsageRecorderFor(Deps{}, "m", "up", false, "acct", "prov", "chat")
	// 非流式路径不会调用 noteFirstToken（见 chat.go 的 aggregateChat）
	ev := rec.base()
	if ev.TTFTMS != nil {
		t.Errorf("非流式请求的 TTFTMS = %v，期望 nil —— "+
			"非流式没有「首字」这个语义，不该编造", *ev.TTFTMS)
	}
}

// TestRecorderHonoursExplicitStartTime 守：记账器的起点可以被调用方指定。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 这是 2026-10-09 那个「首字全是 0ms」缺陷的**行为侧**护栏。
// ═══════════════════════════════════════════════════════════════════
//
//	缺陷成因：chat.go 原先在 `TryChat` **之后**才构造记账器，
//	而 TryChat 会阻塞直到上游首个事件到达 ⇒ start 已经晚于首字时刻
//	⇒ 量出来恒为 0ms（委托方实测反馈「首字为什么全是0ms没有意义」）。
//
//	修法：把 time.Now() 提到 TryChat 之前，经 newUsageRecorderFrom
//	把真正的起点传进来。本测试守住"传进来的起点确实被用上了"——
//	若实现退回成 `start: time.Now()`（忽略参数），下面的断言立刻红。
//
// ⚠️ 为什么必须人为把起点往前推：跟 TestNoteFirstTokenIsIdempotent
//
//	同一个道理 —— 若起点与"现在"只差几十微秒，那么"用了传入的起点"
//	和"用了 time.Now()"算出来的毫秒数都是 0，测试恒绿（假测试）。
//	把起点推到 3 秒前，两种实现的差别立刻显现。
func TestRecorderHonoursExplicitStartTime(t *testing.T) {
	const backdate = 3 * time.Second
	start := time.Now().Add(-backdate)

	rec := newUsageRecorderFrom(Deps{}, "m", "up", true, "acct", "prov", "chat", start)

	// 起点必须原样被采用
	if d := time.Since(rec.start); d < backdate {
		t.Errorf("记账器起点没有采用传入值：距现在 %v，期望 ≥ %v —— "+
			"说明实现忽略了 start 参数、退回成 time.Now()，"+
			"那正是「首字恒 0ms」的成因", d, backdate)
	}

	// 由它算出的首字耗时必须反映那 3 秒，而不是 ~0
	rec.noteFirstToken()
	if rec.ttftMS == nil {
		t.Fatal("noteFirstToken 之后仍是 nil")
	}
	if got := *rec.ttftMS; got < backdate.Milliseconds() {
		t.Errorf("首字耗时 = %dms，期望 ≥ %dms —— 起点没生效，"+
			"这就是委托方看到的「全是 0ms」", got, backdate.Milliseconds())
	}

	// 总耗时（DurationMS）同样要覆盖那 3 秒 ——
	// 同一处起点错误曾经让"总耗时"也漏掉上游等待（偏小）。
	ev := rec.base()
	if ev.DurationMS < backdate.Milliseconds() {
		t.Errorf("总耗时 = %dms，期望 ≥ %dms —— "+
			"起点必须覆盖 TryChat 里的上游等待，否则耗时统计偏小",
			ev.DurationMS, backdate.Milliseconds())
	}
}

// TestChatStartTimeTakenBeforeTryChat 守：**源码层面**起点必须在
// TryChat 之前取（结构性护栏，防止将来有人把 reqStart 挪回后面）。
//
// 🔴 为什么审源码而不是只靠行为测试：
//
//	行为测试（上一条）验证的是"记账器会用传入的起点"，
//	但**传什么值**是 chat.go 决定的。若有人把
//	`reqStart := time.Now()` 挪到 TryChat 之后，
//	记账器依然"正确地"使用它，上一条测试照样绿 —— 而缺陷复活。
//	所以必须直接盯住 chat.go 里两者的先后顺序。
//
// 反向对照：把 reqStart 那行移到 TryChat 之后，本条立刻红。
func TestChatStartTimeTakenBeforeTryChat(t *testing.T) {
	src, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	// 两个函数都要查：流式与非流式各自有 TryChat 调用。
	for _, fn := range []string{"func streamChat(", "func aggregateChat("} {
		idx := strings.Index(text, fn)
		if idx < 0 {
			t.Fatalf("找不到 %s", fn)
		}
		// 取到下一个顶层 func 为止（粗切即可，与项目既有做法一致）
		rest := text[idx:]
		if next := strings.Index(rest[1:], "\nfunc "); next >= 0 {
			rest = rest[:next+1]
		}

		startAt := strings.Index(rest, "reqStart := time.Now()")
		tryAt := strings.Index(rest, "TryChat(")
		if startAt < 0 {
			t.Errorf("%s 里没有 reqStart := time.Now() —— "+
				"起点必须显式取在 TryChat 之前，否则首字耗时恒为 0ms", fn)
			continue
		}
		if tryAt < 0 {
			t.Errorf("%s 里没有 TryChat 调用？", fn)
			continue
		}
		if startAt > tryAt {
			t.Errorf("🔴 %s 里 reqStart 取在 TryChat **之后**（offset %d > %d）——\n"+
				"  TryChat 会阻塞到上游首个事件到达，于是：\n"+
				"    · 首字耗时 = 「TryChat 返回 → 写首帧」≈ 0ms（委托方实测的缺陷）\n"+
				"    · 总耗时漏掉上游等待，偏小\n"+
				"  修法：把 reqStart 提到 TryChat 之前。", fn, startAt, tryAt)
		}
	}
}

// TestUsageLogEntryCarriesTTFT 守：**真实的明细构造路径**带上了首字耗时。
//
// 🔴 第一版这个测试是假的（被反向对照抓到）：它直接
//
//	`usageLogEntry{TTFTMS: &ms}` 构造结构体，于是只验证了 json tag，
//	**完全没验证 buildUsageLogEntry 有没有把事件里的值转过去**。
//	把 `TTFTMS: ev.TTFTMS` 删掉，它照样通过 —— 而那正是
//	"字段在、值不填"的真实故障形态（账号命中率就是这么坏的）。
//
//	修法：从 **usagepkg.Event** 出发，走真实的 buildUsageLogEntry。
func TestUsageLogEntryCarriesTTFT(t *testing.T) {
	ms := int64(800)
	ev := usagepkg.Event{
		Time:             time.Now(),
		Account:          "acct…",
		Model:            "m",
		OK:               true,
		Status:           "ok",
		Stream:           true,
		DurationMS:       40000,
		TTFTMS:           &ms,
		UsageKnown:       true,
		TotalTokens:      161,
		PromptTokens:     96,
		CompletionTokens: 65,
	}

	// 走真实构造路径（dep 只用于反查昵称，这里无账号库 ⇒ 昵称留空）
	e := buildUsageLogEntry(Deps{}, ev)

	b, _ := json.Marshal(e)
	var got map[string]any
	_ = json.Unmarshal(b, &got)

	if got["ttft_ms"] != float64(800) {
		t.Errorf("ttft_ms = %v，期望 800 —— "+
			"buildUsageLogEntry 没把事件的 TTFTMS 转过来，"+
			"前端就永远拿不到首字耗时（字段在、值不填）", got["ttft_ms"])
	}
	if got["duration_ms"] != float64(40000) {
		t.Errorf("duration_ms = %v，期望 40000", got["duration_ms"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %v，期望 true（前端靠它显示「流」列）", got["stream"])
	}
	if got["total_tokens"] != float64(161) {
		t.Errorf("total_tokens = %v，期望 161（「合计」列的数据源）",
			got["total_tokens"])
	}
}

// TestUsageLogTTFTNilWhenAbsent 守：没测到时是 null 不是 0。
func TestUsageLogTTFTNilWhenAbsent(t *testing.T) {
	e := usageLogEntry{DurationMS: 100}
	b, _ := json.Marshal(e)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["ttft_ms"] != nil {
		t.Errorf("没测到首字耗时时 ttft_ms = %v，期望 null —— "+
			"0 会被显示成「首字 0ms」，那是伪造数据", got["ttft_ms"])
	}
}
