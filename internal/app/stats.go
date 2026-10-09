package app

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// statsResponse 是面板用的统计接口响应。
//
// 字段命名与前端约定一致；缺失值用 null/0 明确表达。
type statsResponse struct {
	GeneratedAt string `json:"generated_at"`
	RangeDays   int    `json:"range_days"`

	// UsageWritable 报告用量写入是否可用。
	//
	// 🔴 Codex 第 13 轮要求：非 Windows 平台没有跨进程文件锁，
	// 写入会失败。若只是让日志吞掉，就会表现为
	// **"统计页看着正常但一直是 0"** —— 用户以为没用过，
	// 实际是数据在悄悄丢。明确报告比静默降级好得多。
	UsageWritable bool `json:"usage_writable"`

	// UsageNote 在写入不可用时给出可读原因；可用时为空串。
	UsageNote string `json:"usage_note,omitempty"`

	// Truncated 表示读数时有文件在解析中途中断（末尾残缺或中途坏行）。
	//
	// 🔴 Codex 第 17 轮要求：读取端遇到坏行会停止（fail-closed 取舍），
	// 但**必须把"数据不完整"报出来** —— 否则面板会把部分统计当成完整统计，
	// 用户只看到数字变小却不知道原因，比直接报错更误导。
	Truncated bool `json:"truncated"`

	// TruncatedAt 是第一个发生截断的文件名（如 2026-10-05.jsonl）。
	TruncatedAt string `json:"truncated_at,omitempty"`

	Total struct {
		Requests         int64 `json:"requests"`
		OK               int64 `json:"ok"`
		Failed           int64 `json:"failed"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		ReasoningTokens  int64 `json:"reasoning_tokens"`
		CacheHitTokens   int64 `json:"cache_hit_tokens"`
		CacheMissTokens  int64 `json:"cache_miss_tokens"`

		// UsageKnownCount / UsageUnknownCount 区分"上游给了 usage"与"没给"。
		//
		// 🔴 为什么必须分开（Codex 第 11 轮）：客户端中途断开、或上游没返回
		// usage 时，我们**不伪造** token 数（写 0 且 usage_known=false）。
		// 于是 token 总和里天然不含这些请求，但 Requests 却含 ——
		// 任何"总 token ÷ 总请求数"算出来的平均值都会被拉低，
		// 看起来像"这个反代很省 token"，实际是分母掺了未知样本。
		//
		// 所以：算平均值必须用 **UsageKnownCount** 作分母。
		// 需要平均值的调用方（前端/脚本）请用 average_tokens_per_known_request。
		UsageKnownCount   int64 `json:"usage_known_count"`
		UsageUnknownCount int64 `json:"usage_unknown_count"`

		// AverageTokensPerKnownRequest 是"每次已知请求的平均 token"。
		//
		// 由服务端算好，避免每个调用方各自踩上面那个分母陷阱。
		// 没有已知样本时为 null（不是 0）—— 0 会被误读成"平均消耗 0"。
		AverageTokensPerKnownRequest *float64 `json:"average_tokens_per_known_request"`
	} `json:"total"`

	// Credits 额度消耗。null 表示样本里没有任何一次上游返回过 credit。
	Credits *float64 `json:"credits"`

	Models []modelStat `json:"models"`

	Accounts []accountStat `json:"accounts"`

	// Daily 是按**日**展开的时间序列（2026-10-09 委托方要求）。
	//
	// 🔴 为什么要加（委托方原话：要"每个模型/账号用量"两张表**旁边**
	//	再来两张图）：上面 Models/Accounts 只有窗口内的**聚合值**，
	//	看不出"用量是什么时候涨的" —— 是某天爆量，还是每天都在用。
	//	时间序列能把趋势显示出来，聚合值做不到。
	//
	// ⚠️ 纯增量字段：不改任何既有字段的含义，老调用方忽略它即可。
	//
	// ⚠️ 为什么不用 omitempty：前端**无条件**读 `d.daily.dates`
	//	（图表要画连续横轴）。字段缺失会让面板抛异常，
	//	所以即使没有用量/没有 store 也必须给出完整的零值轴。
	Daily dailyStats `json:"daily"`
}

// dailyStats 是 /api/stats 的日时间序列（契约由前端约定，名字不可改）。
//
// 🔴 硬不变量：Dates / Labels / TotalTokens / PromptTokens /
// CompletionTokens 的长度**必须都等于 len(Dates)**，
// 且每个 Accounts[].Values 也一样。少一格图表就错位 ——
// 这不是"最好满足"，是构造上必须成立（见 dailySeriesBuilder）。
type dailyStats struct {
	// Dates 升序（最旧 → 最新）的 ISO 日期，如 "2026-10-03"。
	//
	// ⚠️ 覆盖整个 days 窗口，**含零用量的日子** ——
	//	否则横轴会出现空洞，折线会把"那天没用"错画成"斜线增长"。
	Dates []string `json:"dates"`

	// Labels 横轴短标签（"10-03"），免得前端自己切字符串（容易切错）。
	Labels []string `json:"labels"`

	// TotalTokens[i] 恒等于 PromptTokens[i] + CompletionTokens[i]。
	//
	// 🔴 为什么是**算出来的**而不是各自累加 ev.TotalTokens（见 build）：
	//	前端把输入/输出堆叠起来画在同一根柱子上，总柱子如果不是
	//	两部分之和，图形就自相矛盾。宁可让总柱子"由两部分定义"。
	TotalTokens      []int64 `json:"total_tokens"`
	PromptTokens     []int64 `json:"prompt_tokens"`
	CompletionTokens []int64 `json:"completion_tokens"`

	// Accounts 每个账号一条折线，按窗口内总 token 降序。
	//
	// ⚠️ 只含**窗口内出现过**的账号（没用过就没线，不是补一条全 0 的线）。
	Accounts []dailyAccountSeries `json:"accounts"`
}

// dailyAccountSeries 是 daily 里的一条账号折线。
type dailyAccountSeries struct {
	// Name 图例显示名：有昵称用昵称，否则**退回脱敏 UID**。
	//
	// 与 accountStat.Nickname 同一条规则（都走 lookupAccountByMasked），
	// 只是这里必须有值 —— 图例不能是空白项。
	Name string `json:"name"`

	// Account 脱敏后的账号 ID（永远给，便于对日志）。
	//
	// 🔴 与全项目口径一致：**只给脱敏值 + 昵称**，
	//	绝不出现完整 UID 或任何凭据。
	Account string `json:"account"`

	// Values 该账号逐日 token（与 Dates 等长、下标一一对应）。
	Values []int64 `json:"values"`
}

type modelStat struct {
	Model            string   `json:"model"`
	Requests         int64    `json:"requests"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	Credits          *float64 `json:"credits"`
	Share            float64  `json:"share"` // 占总 token 的比例

	// 缓存命中/未命中（面板按模型显示命中率）。
	//
	// 🔴 为什么要有：委托方要求模型用量里显示缓存命中率 ——
	// 总表已有命中率，但按模型拆开后才能看出"哪个模型缓存效果好"。
	CacheHitTokens  int64 `json:"cache_hit_tokens"`
	CacheMissTokens int64 `json:"cache_miss_tokens"`

	// CacheHitRate 缓存命中率（0~1）；**分母为 0 时为 null**。
	//
	// 🔴 为什么不直接让前端算（Codex 第 40 轮指出）：
	//
	//	只给分子分母两个 0，前端无法区分这两种情况：
	//	  · 该模型从没被调用过（无样本）⇒ 应显示 —
	//	  · 调用过但一次都没命中（真实 0%）⇒ 应显示 0%
	//	把前者显示成 0% 会让人以为"缓存完全没生效"，是误导。
	//
	//	用指针表达 null，语义明确。
	//
	// ⚠️ 口径：命中率 = 命中 ÷（命中 + 未命中），
	//	是**先各自求和再相除**，不是"每次都算百分比再平均"
	//	（后者会让偶发的小请求权重过大）。
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

type accountStat struct {
	// Account 脱敏后的账号 ID（保留：用户可能对不上昵称时要拿它对日志）。
	Account string `json:"account"`

	// Nickname 账号显示名（手机号 / 邮箱）。
	//
	// 🔴 为什么必须给（2026-10-06 委托方反馈）：
	//
	//	原来只显示脱敏 UID（`eeeeeeee…`），用户原话：
	//	  「为什么不用我账号管理里面的4个账号名字，
	//	    不然我都分不清到底是哪个账号的用量」
	//	脱敏 UID 对人类不可读 —— 统计页面就是要看出"哪个号用了多少"，
	//	只给一串哈希前缀等于没给。
	//
	// ⚠️ 昵称不是凭据（面板本来就展示它），不违反任何红线。
	Nickname string `json:"nickname,omitempty"`

	// Platform 所属平台（"cn"/"intl"），用于分组与区分同名情况。
	Platform string `json:"platform,omitempty"`

	Requests int64 `json:"requests"`
	OK       int64 `json:"ok"`
	Failed   int64 `json:"failed"`

	// ── 以下为 2026-10-06 委托方要求新增 ──
	//
	// 原话：「账号用量要包括token信息和积分消耗，请求这些没用改成调用」
	//	只给"调用次数/成功/失败"信息量太少，看不出这个号实际消耗。

	// PromptTokens / CompletionTokens / TotalTokens 该账号的 token 用量。
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// Credits 该账号的积分消耗；上游从未返回过时为 null。
	//
	// ⚠️ 指针：nil = 没有任何一次拿到过 credit（未知），
	//	0 = 确实消耗为 0。两者含义不同，不能混。
	Credits *float64 `json:"credits"`

	// CacheHitTokens / CacheMissTokens 该账号的缓存命中情况。
	CacheHitTokens  int64 `json:"cache_hit_tokens"`
	CacheMissTokens int64 `json:"cache_miss_tokens"`

	// CacheHitRate 该账号的缓存命中率（0~1）；**分母为 0 时为 null**。
	//
	// 🔴 为什么必须补这个字段（2026-10-09 委托方实测反馈：
	//	「账号用量的缓存命中率没有显示」）：
	//
	//	此前只给了分子分母两个原始值，**没给算好的比率** ——
	//	而前端读的是 `a.cache_hit_rate`，于是永远拿到 undefined、
	//	每行都显示「—」。数据其实一直都在（实测命中 1.09 亿 / 未命中 115 万）。
	//
	//	⚠️ 为什么不让前端自己用分子分母算：
	//	  分母为 0 时前端无法区分「该账号没被调用过」与「调用了但一次没命中」
	//	  （前者应显示 —，后者应显示 0.0%）。用 null 表达"无样本"才不误导。
	//	  这与 modelStat.CacheHitRate 的口径**完全一致**，两处不能分叉。
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

// lookupAccountByMasked 用**脱敏后的 UID** 反查账号。
//
// 🔴 为什么需要反查（2026-10-06）：
//
//	用量事件里存的是 `auth.MaskUID(uid)`（脱敏），不是完整 UID ——
//	那是刻意的（用量日志不该含可读账号信息）。
//	但统计页面要显示"哪个号用了多少"，所以得拿脱敏值回去查表。
//
// ⚠️ 用**同一个** MaskUID 函数逐个比对，而不是自己切字符串：
//
//	MaskUID 的实现改了（比如换掩码长度），这里会自动跟着对，
//	自己切就会静默失配（表现为"昵称全空"）。
//
// 账号已被删除 ⇒ 返回 false，调用方退回显示脱敏 UID。
// 那是正确行为：不能用"猜"给一个已删账号补名字。
// 🔴 2026-10-07 修数据竞争：本函数原本返回 `*auth.Account`（**共享指针**），
// 而它由面板聚合路径**每行用量记录调用一次**（全项目调用频次最高的
// `*auth.Account` 获取点）。调用方随后读 `Nickname` / `Platform` ——
// 虽然这两个字段创建后不变、当前实际无害，但"把共享指针带出锁作用域"
// 这一形态本身就是隐患（将来有人加读一个可变字段就变成真 bug）。
//
// ⇒ 改为返回**值拷贝**（Snapshot）。调用方拿到的是自洽的副本。
func lookupAccountByMasked(deps Deps, masked string) (auth.Account, bool) {
	if deps.Accounts == nil || masked == "" {
		return auth.Account{}, false
	}
	// 先只取 UID 列表（值），匹配出目标 UID，再取该账号的快照
	for _, uid := range deps.Accounts.UIDs() {
		if auth.MaskUID(uid) != masked {
			continue
		}
		if snap, ok := deps.Accounts.Snapshot(uid); ok {
			return snap, true
		}
	}
	return auth.Account{}, false
}

// handleStats 聚合用量事件并返回 JSON。
//
// 查询参数 days：统计天数（默认 1，上限 365）。
//
// 实现要点：流式读取 JSONL 后聚合，【不把明细留在内存】。
func handleStats(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
			"method_not_allowed", "只支持 GET")
		return
	}
	if deps.Usage == nil {
		// ⚠️ 连 daily 也必须给出**非 nil 的完整结构**：
		//	前端无条件读 d.daily.dates，字段为 null 会让图表代码抛异常
		//	（表现为整个统计页"加载失败"，而不是"没有数据"）。
		//	这里给空数组而不是 nil —— JSON 里是 []，前端可安全遍历。
		//
		//	🔴 不动 RangeDays/GeneratedAt 之外的既有字段语义：
		//	本条早退路径的 RangeDays 保持原来的 0，不改（见"纯增量"要求）。
		resp := statsResponse{
			GeneratedAt: time.Now().Format(time.RFC3339),
			Daily: dailyStats{
				Dates:            []string{},
				Labels:           []string{},
				TotalTokens:      []int64{},
				PromptTokens:     []int64{},
				CompletionTokens: []int64{},
				Accounts:         []dailyAccountSeries{},
			},
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	days := parseIntParam(r.URL.Query().Get("days"), 1)
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}

	var resp statsResponse
	resp.RangeDays = days

	// 🔴 只取一次"现在"，并且**刻意用 time.Now 而不是 nowFunc()**。
	//
	//	日时间序列的横轴必须与"读取端到底翻了哪几个文件"完全对齐：
	//	usage.Store.ReadResult 内部用的是 time.Now()（见 event.go 的
	//	`today := time.Now()`）。若这里改用可被测试替换的 nowFunc，
	//	两者就会指向不同的"今天" —— 窗口与横轴错开一天，
	//	表现为"某天的数据画在隔壁那格"，而且只在测试里出现，极难排查。
	//	⇒ 与读取端同源，宁可牺牲一点可测性（横轴日期可由 days 推导验证）。
	//
	// ⚠️ 已知边界（如实记录，本轮未修）：读取端按 **本机时区** 决定翻哪几个
	//	日期文件，而下面的横轴按 **UTC+8** 标注（全项目墙钟口径）。
	//	在本项目目标机（东八区）两者恒等，所以完全对齐；
	//	但把本程序跑在非东八区机器上时，跨日那两个小时里
	//	"翻了哪天"与"标了哪天"可能差一天。
	//	要彻底消除得改 internal/usage 的读取接口（让回调带出文件名），
	//	而本次任务范围只允许动 stats.go —— 记在这里，不假装不存在。
	now := time.Now()
	resp.GeneratedAt = now.Format(time.RFC3339)

	// 明确报告 usage 写入是否可用（Codex 第 13 轮要求）。
	//
	// 非 Windows 没有文件锁实现 → 写入会失败。若不报告，
	// 表现为"统计页一直 0"，用户以为没用过，实际是数据在丢。
	resp.UsageWritable = deps.Usage.LockAvailable()
	if !resp.UsageWritable {
		resp.UsageNote = "用量写入不可用：当前平台不支持跨进程文件锁，" +
			"本次会话的用量不会被记录"
	}

	modelMap := make(map[string]*modelStat)
	acctMap := make(map[string]*accountStat)
	var creditSum float64
	var creditSeen bool

	// 日时间序列累加器（2026-10-09 新增）。
	//
	// 🔴 内存口径：O(days × 账号数)，**与记录条数无关**。
	//	它只是一组长度固定为 days 的切片，绝不缓存任何一条事件 ——
	//	否则"一天上万条"会把内存吃光（见 dailyBuilder 的注释）。
	daily := newDailyBuilder(days, now)

	// 用 ReadResult 而不是 Read：需要知道"数据是否完整"，
	// 否则截断时面板会把部分统计显示成完整统计（Codex 第 17 轮要求）。
	rr, err := deps.Usage.ReadResult(days, func(ev usagepkg.Event) error {
		// 日序列先累计（与下面各聚合器互不影响）。
		//
		// 🔴 归属日必须用**事件自己的时间戳**（见 dailyBuilder.add 的说明）。
		daily.add(ev)
		resp.Total.Requests++
		if ev.OK {
			resp.Total.OK++
		} else {
			resp.Total.Failed++
		}
		resp.Total.PromptTokens += ev.PromptTokens
		resp.Total.CompletionTokens += ev.CompletionTokens
		resp.Total.TotalTokens += ev.TotalTokens
		resp.Total.ReasoningTokens += ev.ReasoningTokens
		resp.Total.CacheHitTokens += ev.CacheHitTokens
		resp.Total.CacheMissTokens += ev.CacheMissTokens

		// 区分"上游给了 usage"与"没给"（见 statsResponse 的字段注释）。
		// 这些记录的 token 是 0（我们不伪造），若混入平均值分母会拉低结果。
		if ev.UsageKnown {
			resp.Total.UsageKnownCount++
		} else {
			resp.Total.UsageUnknownCount++
		}

		// credit：只有上游真返回过才累计（nil 不参与）
		if ev.Credit != nil {
			creditSeen = true
			creditSum += *ev.Credit
		}

		// 按模型聚合 —— 先归一化 ID。
		//
		// 🔴 为什么必须归一化（2026-10-06 委托方实测反馈）：
		//
		//	同一个模型会因为**客户端写法不同**被记成多条：
		//	  "dsf"                        （别名）
		//	  "deepseek-v4.1-flash"        （裸 ID）
		//	  "workbuddy/deepseek-v4.1-flash"（带前缀的完整 ID）
		//	三条其实是**同一个模型**，分成三行让用户以为用了三种模型。
		//	委托方原话：「这个dsf和deepseek-v4.1-flash使用的都是
		//	workbuddy/deepseek-v4.1-flash，为什么三种显示，
		//	都显示workbuddy/deepseek-v4.1-flash就行」
		//
		//	⚠️ 只在**聚合时**归一化，**不改历史记录** ——
		//	  JSONL 里的原始值保持不动（那是事实），
		//	  归一化只是"展示口径"，将来还能按原文重新聚合。
		//
		// 🔴 优先用记录里的**真实路由身份**（Codex 第 40 轮建议）：
		//	新记录带 ProviderID + ResolvedModel，直接拼出准确 ID，
		//	不依赖注册表当前状态 —— 这样接入第二个平台后，
		//	老记录的归属不会被改写。
		//	老记录（这两个字段为空）才回退到 CanonicalModelID。
		modelKey := deps.resolveModelKey(ev)

		ms, ok := modelMap[modelKey]
		if !ok {
			ms = &modelStat{Model: modelKey}
			modelMap[modelKey] = ms
		}
		ms.Requests++
		ms.PromptTokens += ev.PromptTokens
		ms.CompletionTokens += ev.CompletionTokens
		ms.TotalTokens += ev.TotalTokens
		ms.ReasoningTokens += ev.ReasoningTokens
		ms.CacheHitTokens += ev.CacheHitTokens
		ms.CacheMissTokens += ev.CacheMissTokens
		if ev.Credit != nil {
			if ms.Credits == nil {
				v := 0.0
				ms.Credits = &v
			}
			*ms.Credits += *ev.Credit
		}

		// 按账号聚合
		if ev.Account != "" {
			as, ok := acctMap[ev.Account]
			if !ok {
				// 🔴 昵称与平台在**首次遇到该账号时**填一次（2026-10-06）。
				//
				//	数据来源是当前账号表（deps.Accounts），而不是用量事件 ——
				//	事件里只有脱敏 UID（那是刻意的：用量日志不该含账号可读信息）。
				//
				//	⚠️ 账号可能已被删除：此时账户表里查不到，
				//	  昵称留空、平台留空，前端会退回显示脱敏 UID。
				//	  这是正确行为 —— 不能用"猜"来补一个已删账号的名字。
				as = &accountStat{Account: ev.Account}
				if acct, found := lookupAccountByMasked(deps, ev.Account); found {
					as.Nickname = acct.Nickname
					as.Platform = acct.PlatformOf()
				}
				acctMap[ev.Account] = as
			}
			as.Requests++
			if ev.OK {
				as.OK++
			} else {
				as.Failed++
			}
			// token 与积分也按账号累计（2026-10-06 委托方要求）。
			as.PromptTokens += ev.PromptTokens
			as.CompletionTokens += ev.CompletionTokens
			as.TotalTokens += ev.TotalTokens
			as.CacheHitTokens += ev.CacheHitTokens
			as.CacheMissTokens += ev.CacheMissTokens
			if ev.Credit != nil {
				if as.Credits == nil {
					v := 0.0
					as.Credits = &v
				}
				*as.Credits += *ev.Credit
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "stats_failed", err.Error())
		return
	}

	// 把"数据是否完整"透给前端（Codex 第 17 轮要求）。
	// 截断时面板必须显式提示，而不是把部分统计当完整统计显示。
	resp.Truncated = rr.Truncated
	resp.TruncatedAt = rr.TruncatedAt

	if creditSeen {
		resp.Credits = &creditSum
	}

	// 平均 token：分母只用"已知 usage"的请求数（Codex 第 11 轮要求）。
	//
	// 用总请求数当分母会把客户端断开/上游没给 usage 的记录算进去，
	// 而那类记录 token 是 0（我们不伪造），结果平均值被无端拉低。
	// 没有已知样本时留 nil —— 0 会被误读成"平均消耗 0"。
	if resp.Total.UsageKnownCount > 0 {
		avg := float64(resp.Total.TotalTokens) / float64(resp.Total.UsageKnownCount)
		resp.Total.AverageTokensPerKnownRequest = &avg
	}

	// 展开并算占比
	total := resp.Total.TotalTokens
	for _, ms := range modelMap {
		if total > 0 {
			ms.Share = float64(ms.TotalTokens) / float64(total)
		}
		// 缓存命中率：分母为 0 时留 nil（= null），**不是** 0。
		// 详见 modelStat.CacheHitRate 的注释（"无样本" ≠ "命中率 0"）。
		if denom := ms.CacheHitTokens + ms.CacheMissTokens; denom > 0 {
			rate := float64(ms.CacheHitTokens) / float64(denom)
			ms.CacheHitRate = &rate
		}
		resp.Models = append(resp.Models, *ms)
	}
	sort.Slice(resp.Models, func(i, j int) bool {
		if resp.Models[i].TotalTokens != resp.Models[j].TotalTokens {
			return resp.Models[i].TotalTokens > resp.Models[j].TotalTokens
		}
		return resp.Models[i].Model < resp.Models[j].Model
	})

	for _, as := range acctMap {
		// 账号缓存命中率：与 modelStat 同一口径（分母为 0 时留 nil，不是 0）。
		//
		// 🔴 见 accountStat.CacheHitRate 的注释：前端读这个字段，
		//	不补它每行都显示「—」。
		if denom := as.CacheHitTokens + as.CacheMissTokens; denom > 0 {
			rate := float64(as.CacheHitTokens) / float64(denom)
			as.CacheHitRate = &rate
		}
		resp.Accounts = append(resp.Accounts, *as)
	}
	sort.Slice(resp.Accounts, func(i, j int) bool {
		return resp.Accounts[i].Requests > resp.Accounts[j].Requests
	})

	// 日序列成型。
	//
	// ⚠️ 昵称反查放在**这里**（每个账号一次），不能放进 add()：
	//	add 是**每条记录**都调用的热点，而 lookupAccountByMasked 要
	//	遍历账号表 —— 放进去就是"每行用量都查一次表"，
	//	一天上万条时纯属浪费（结果还必然相同）。
	//	账号数是个位数，所以这里最多查几次。
	//
	// 与 resp.Accounts 完全同源（同一个 lookupAccountByMasked），
	// 保证两处显示的账号名不会分叉。
	resp.Daily = daily.build(func(masked string) string {
		if acct, ok := lookupAccountByMasked(deps, masked); ok {
			return acct.Nickname
		}
		return ""
	})

	writeJSON(w, http.StatusOK, resp)
}

// ─────────────────────────────────────────────────────────────
// 日时间序列累加器（2026-10-09，委托方要求"两张图"）
// ─────────────────────────────────────────────────────────────

// dailyBuilder 把流式读到的用量事件累加成"逐日 × 逐账号"的矩阵。
//
// 🔴 内存为什么是 O(days × 账号数) 而不是 O(记录数)：
//
//	它持有的是**定长**结构 —— 长度 days 的六个切片 + 每个账号一条
//	长度 days 的切片。事件**读一条、加一格、立刻丢弃**，绝不入队/缓存。
//	所以一天一万条与一条，占用完全相同（账号数是个位数）。
//
//	这是本项目内存红线（8 GB 机器 / idle RSS ≤ 60 MB）的直接要求：
//	若为了画图先把事件收集成 []Event 再聚合，一天的量就够把内存吃光。
//
// ⚠️ 为什么"日期 → 下标"用一张小 map，而不是线性查找：
//
//	dayKeys 是**升序**的（今天的在最后），而绝大多数事件是今天的 ——
//	线性从头扫就等于每次都扫满整个窗口（days 最大 365）。
//	map 的规模被 days 上限封死在 365 项（约几十 KB），
//	换来每条记录的 O(1) 定位，在"一天上万条"下差别很大。
type dailyBuilder struct {
	// days 窗口天数，同时也是所有切片的长度（硬不变量的来源）。
	days int

	// dayKeys[i] 是第 i 格的 UTC+8 日期（"2006-01-02"）。
	// 升序：i=0 最旧，i=days-1 是今天 —— 与前端"从左到右"一致。
	dayKeys []string

	// dayIndex 是 dayKeys 的反查表（日期串 → 下标）。
	dayIndex map[string]int

	// 固定窗口的定长切片（长度恒为 days）。
	total      []int64
	prompt     []int64
	completion []int64

	// acctIdx 把脱敏账号 ID 映射到 accts 的下标。
	//
	// 🔴 只按**脱敏 ID** 聚合，不按昵称 —— 昵称可变/可重复，
	//	而脱敏 ID 与用量事件里的值同源（见 lookupAccountByMasked）。
	acctIdx map[string]int

	// accts 是窗口内出现过的账号（每个一条定长 values）。
	accts []*dailyAcct
}

// dailyAcct 是一个账号的累加状态（**不导出**，仅内部用）。
type dailyAcct struct {
	account  string
	nickname string
	values   []int64
}

// newDailyBuilder 按 days 窗口预生成连续日期轴（含零用量的日子）。
//
// now 由调用方传入（而非这里再取一次时间），保证与读取窗口同源 ——
// 见 handleStats 里 `now := time.Now()` 的说明。
func newDailyBuilder(days int, now time.Time) *dailyBuilder {
	if days < 1 {
		days = 1
	}
	b := &dailyBuilder{
		days:       days,
		dayKeys:    make([]string, days),
		dayIndex:   make(map[string]int, days),
		total:      make([]int64, days),
		prompt:     make([]int64, days),
		completion: make([]int64, days),
		acctIdx:    make(map[string]int),
	}

	// 🔴 归属日按 **UTC+8** 算，与全项目一致（auth/pool/provider/CLI
	//	的 zone 都是 `time.FixedZone("UTC+8", 8*3600)`）。
	//
	//	⚠️ 不能用 time.Local：本机恰好是东八区所以看不出差别，
	//	换台机器就会整体错一天（与 pool.go 里记的同一个坑）。
	//	用量事件里的 Time 是绝对时刻（time.Time），转成 UTC+8 才是
	//	用户认知里的"那一天"。
	today := now.In(cnZone)
	for i := 0; i < days; i++ {
		// i=0 最旧 ⇒ 从 (days-1) 天前数到今天。
		d := today.AddDate(0, 0, -(days - 1 - i))
		b.dayKeys[i] = d.Format("2006-01-02")
		b.dayIndex[b.dayKeys[i]] = i
	}
	return b
}

// add 把一条事件累加进对应日期格。
//
// 🔴 归属日取 `ev.Time`（事件**自己的**时间戳），不是文件名。
//
//	理由：usage.Store 虽然按日分文件写（fileName(ev.Time)），但
//	`ReadResult(days)` 只按"文件名 = 今天-N"翻文件，**回调里不带文件名**
//	（签名是 func(Event) error）。也就是说读取端根本不给"这条来自哪个
//	文件"这个信息；若要用文件名归属，就得改 internal/usage 的接口 ——
//	而本任务是"只动 stats.go / api_usage_log.go"。
//
//	好在 ev.Time 是**写盘时就在事件里的**（usage_record.go 的 base()
//	用 r.start，即请求开始时刻），与文件名同源同值，
//	所以用它可以得到与文件名完全一致的归属日，且不依赖任何外部状态。
//
//	⚠️ 越界的事件直接忽略（不进任何格）：理论上 ev.Time 一定落在
//	  窗口内（文件按日分），但若有人手动改了系统时间或补写旧记录，
//	  落到窗口外就会让下标越界 panic。宁可少算一格，不可崩整个统计页。
func (b *dailyBuilder) add(ev usagepkg.Event) {
	key := ev.Time.In(cnZone).Format("2006-01-02")
	idx := b.indexOf(key)
	if idx < 0 {
		return
	}

	// 🔴 total 由 prompt + completion **算出来**，不累加 ev.TotalTokens。
	//
	//	为什么（前端堆叠图要求）：图表把输入/输出堆起来当总柱子，
	//	若 total 另取一个来源，遇到上游给的 total 与两部分不一致
	//	（实测上游偶尔如此）就会出现"总柱子 ≠ 两段之和"的自相矛盾图形。
	//	定义成"由两部分求和"，图形永远自洽。
	//
	//	⚠️ 这**不改** resp.Total.TotalTokens 的口径（仍累加 ev.TotalTokens，
	//	既有字段含义不许变）。只有 daily 用这个自洽口径。
	b.prompt[idx] += ev.PromptTokens
	b.completion[idx] += ev.CompletionTokens
	b.total[idx] += ev.PromptTokens + ev.CompletionTokens

	if ev.Account == "" {
		return
	}
	i, ok := b.acctIdx[ev.Account]
	if !ok {
		i = len(b.accts)
		b.acctIdx[ev.Account] = i
		b.accts = append(b.accts, &dailyAcct{
			account: ev.Account,
			values:  make([]int64, b.days),
		})
	}
	b.accts[i].values[idx] += ev.PromptTokens + ev.CompletionTokens
}

// indexOf 返回某个日期串在窗口里的下标；不在窗口内返回 -1。
func (b *dailyBuilder) indexOf(key string) int {
	if i, ok := b.dayIndex[key]; ok {
		return i
	}
	return -1
}

// build 把累加结果转成对外契约（dailyStats）。
//
// 🔴 硬不变量在这里被**构造性**保证：所有切片都由 b.days 一次分配，
//
//	下标全部来自同一个 indexOf —— 不存在"少写一格"的路径。
//	stats_daily_test.go 里另有一条断言守着它（防止将来有人改成 append 式）。
//
// accountName 用于把脱敏 ID 翻成图例显示名（昵称优先）。
// 传 nil 表示没有账号表 —— 此时所有账号都退回脱敏 ID。
func (b *dailyBuilder) build(accountName func(masked string) string) dailyStats {
	out := dailyStats{
		Dates:            b.dayKeys,
		Labels:           make([]string, b.days),
		TotalTokens:      b.total,
		PromptTokens:     b.prompt,
		CompletionTokens: b.completion,
		// ⚠️ 必须是非 nil 空切片：JSON 输出 `[]` 而不是 `null`，
		//	前端才能无条件 .forEach（null 会抛异常）。
		Accounts: []dailyAccountSeries{},
	}

	for i, k := range b.dayKeys {
		// 短标签 "10-03"：只切 ISO 串的后 5 位。
		//
		// ⚠️ 为什么在后端切而不是让前端切：契约里 labels 是独立字段，
		//	前端拿到就能直接用；让 4 个调用方各自 substring 迟早会漂移。
		if len(k) >= 5 {
			out.Labels[i] = k[5:]
		} else {
			out.Labels[i] = k
		}
	}

	// 按窗口内总 token 降序（与 resp.Accounts 的"按用量排序"同一精神：
	// 用得多的画在上面/排前面）。同量时按脱敏 ID 定序 —— 让输出**稳定**，
	// 否则 map 迭代顺序会让每次请求的折线顺序都变（前端图例乱跳）。
	type ranked struct {
		acct *dailyAcct
		sum  int64
	}
	list := make([]ranked, 0, len(b.accts))
	for _, a := range b.accts {
		var sum int64
		for _, v := range a.values {
			sum += v
		}
		list = append(list, ranked{acct: a, sum: sum})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].sum != list[j].sum {
			return list[i].sum > list[j].sum
		}
		return list[i].acct.account < list[j].acct.account
	})

	for _, r := range list {
		name := r.acct.account // 兜底：图例不能空白
		if accountName != nil {
			if n := accountName(r.acct.account); n != "" {
				name = n
			}
		}
		out.Accounts = append(out.Accounts, dailyAccountSeries{
			Name:    name,
			Account: r.acct.account,
			Values:  r.acct.values,
		})
	}
	return out
}

// parseIntParam 解析正整数参数，失败返回默认值。
func parseIntParam(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return def
		}
	}
	return n
}

// 保证 json 包被使用（statsResponse 需要序列化）。
var _ = json.Marshal
