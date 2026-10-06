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
		writeJSON(w, http.StatusOK, statsResponse{GeneratedAt: time.Now().Format(time.RFC3339)})
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
	resp.GeneratedAt = time.Now().Format(time.RFC3339)

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

	// 用 ReadResult 而不是 Read：需要知道"数据是否完整"，
	// 否则截断时面板会把部分统计显示成完整统计（Codex 第 17 轮要求）。
	rr, err := deps.Usage.ReadResult(days, func(ev usagepkg.Event) error {
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
		resp.Accounts = append(resp.Accounts, *as)
	}
	sort.Slice(resp.Accounts, func(i, j int) bool {
		return resp.Accounts[i].Requests > resp.Accounts[j].Requests
	})

	writeJSON(w, http.StatusOK, resp)
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
