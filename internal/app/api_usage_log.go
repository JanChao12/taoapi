package app

import (
	"net/http"
	"strconv"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// ─────────────────────────────────────────────────────────────
// 调用明细接口（面板「调用记录」表）
//
// 委托方要求（2026-10-06 原话）：
//
//	「因为加入一个新列表显示每一次api的调用情况，包括账号、时间、token、
//	  缓存命中率、积分消耗等信息，让我每使用一次都能查询到记录。」
//
// 🔴 与 /api/stats 的分工：
//
//	/api/stats      → **聚合**后的汇总（按模型/按账号分组）
//	/api/usage/log  → **逐条**明细（一次调用一行）
//
// 🔴 为什么不做成分页的"全量导出"：
//
//	用量按日存 JSONL，一天可能上万条。一次性读全量进内存会撑爆
//	（本项目红线是内存小）。所以：
//	  · 默认只返回最近 N 条（倒序，最新的在最前）
//	  · 硬上限兜底，防止 ?limit=999999 把内存吃光
//	这是"够用且不会自伤"的取值，不是功能缺失。
// ─────────────────────────────────────────────────────────────

const (
	// usageLogDefaultLimit 默认返回条数。
	//
	// 🔴 2026-10-09 由 100 提到 1000（委托方原话：
	//	「100 条记录太少了」）。面板默认不传 limit，所以这个值
	//	**直接决定用户能看到多少历史明细** —— 100 条对"一天上万条"
	//	的用量来说只够回看十几分钟，看不出规律。
	//
	// ⚠️ 为什么直接取到上限（= usageLogMaxLimit）而不是更大：
	//	环形缓冲一次分配 limit 个 entry，这个数字同时是**默认**
	//	也是**内存边界**。1000 条 entry 约几十 KB，与 8 GB 机器上
	//	"idle RSS ≤ 60 MB"的红线相比可忽略；再往上加就得重新论证内存。
	usageLogDefaultLimit = 1000

	// usageLogMaxLimit 硬上限。
	//
	// ⚠️ 为什么要有：limit 来自 URL 参数（客户端可控）。
	//	不封顶的话一个 ?limit=100000000 就能让服务 OOM ——
	//	那是**自我 DoS**，必须在这里拦住。
	//
	// 🔴 即使默认值已经等于它，这个封顶也**必须留着**：
	//	防的是 `?limit=` 显式传入的超大值，与默认值取多少无关。
	//	（usage_log_test.go 的 "999999999 ⇒ 封顶" 那条守的就是它。）
	usageLogMaxLimit = 1000
)

// usageLogEntry 是一条调用明细（**已脱敏**）。
//
// 🔴 绝不包含：token 原文、凭据、上游原始报文。
//
//	Account 用脱敏 UID + 昵称，与账号用量表同源。
type usageLogEntry struct {
	Time string `json:"time"`

	// Account 脱敏后的账号 ID。
	Account string `json:"account"`

	// Nickname 账号昵称（查不到时为空，前端退回显示脱敏 UID）。
	Nickname string `json:"nickname,omitempty"`

	// Platform 所属平台（cn/intl）。
	Platform string `json:"platform,omitempty"`

	// Model 归一后的对外模型 ID（含平台前缀）。
	//
	// ⚠️ 用 resolveModelKey 归一，与聚合口径一致 ——
	//	否则明细里会出现 `dsf` / 裸 ID / 带前缀 三种写法混杂。
	Model string `json:"model"`

	// Status 终态（ok / upstream_error / client_disconnected / ...）。
	Status string `json:"status"`

	OK bool `json:"ok"`

	// DurationMS 耗时。
	DurationMS int64 `json:"duration_ms"`

	// TTFTMS 首字耗时（毫秒）；null = 没测到（非流式 / 首字前就失败）。
	//
	// 🔴 2026-10-09 委托方要求「能不能获取到首字耗时」。
	//
	// ⚠️ 用指针，语义三种（见 usage.Event.TTFTMS）：
	//	nil = 不适用或没测到、0 = 确实几乎立刻出字、>0 = 正常测到。
	//	前端据此决定"只显示总耗时"还是"拆成首字/总耗时"。
	TTFTMS *int64 `json:"ttft_ms"`

	// Stream 是否流式。
	Stream bool `json:"stream"`

	// UsageKnown token 是否来自上游最终 usage。
	//
	// 🔴 前端必须据此把 token 显示成 "—" 而不是 0 ——
	//	"上游没给"与"确实是 0"是两回事（见 usage.Event 的注释）。
	UsageKnown bool `json:"usage_known"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// CacheHitRate 该次调用的缓存命中率；无缓存数据时为 null。
	CacheHitRate *float64 `json:"cache_hit_rate"`

	// Credits 该次调用的积分消耗；上游未返回时为 null。
	Credits *float64 `json:"credits"`

	// Error 失败原因（已脱敏）。
	Error string `json:"error,omitempty"`
}

type usageLogResponse struct {
	GeneratedAt string          `json:"generated_at"`
	RangeDays   int             `json:"range_days"`
	Limit       int             `json:"limit"`
	Entries     []usageLogEntry `json:"entries"`

	// TotalMatched 命中的总条数（可能大于返回的 Limit）。
	// 让前端能提示"仅显示最近 N 条，共 M 条"。
	TotalMatched int64 `json:"total_matched"`
}

// handleUsageLog 返回逐条调用明细（最新的在前）。
func handleUsageLog(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 GET")
		return
	}
	if deps.Usage == nil {
		writeError(w, http.StatusServiceUnavailable, openai.ErrTypeServer,
			"no_usage", "用量存储未启用")
		return
	}

	// 复用 stats.go 的参数解析（同一个 days 语义，避免两处口径漂移）
	days := parseIntParam(r.URL.Query().Get("days"), 1)
	limit := clampLimit(r.URL.Query().Get("limit"))

	// 先收集命中项（只留最新的 limit 条）。
	//
	// 🔴 为什么用"环形覆盖"而不是全读进来再切：
	//	一天上万条时全读会占几百 MB 内存。这里只保留 limit 个元素的
	//	环形缓冲，内存与文件大小**无关** —— 这是本项目内存红线的要求。
	ring := make([]usageLogEntry, 0, limit)
	var total int64
	var writeIdx int

	_, err := deps.Usage.ReadResult(days, func(ev usagepkg.Event) error {
		total++
		e := buildUsageLogEntry(deps, ev)
		if len(ring) < limit {
			ring = append(ring, e)
			return nil
		}
		// 环形覆盖：最早的被替换
		ring[writeIdx] = e
		writeIdx = (writeIdx + 1) % limit
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, openai.ErrTypeServer,
			"usage_read_failed", err.Error())
		return
	}

	// 环形缓冲的元素顺序是"从写入点开始"的，需要还原成时间序（倒序=最新在前）
	entries := make([]usageLogEntry, 0, len(ring))
	if len(ring) < limit {
		// 没绕圈：本来就是时间序（JSONL 按写入顺序），反转即最新在前
		for i := len(ring) - 1; i >= 0; i-- {
			entries = append(entries, ring[i])
		}
	} else {
		for i := 0; i < len(ring); i++ {
			idx := (writeIdx + i) % limit
			entries = append(entries, ring[idx])
		}
		// 再反转成最新在前
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}

	writeJSON(w, http.StatusOK, usageLogResponse{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		RangeDays:    days,
		Limit:        limit,
		Entries:      entries,
		TotalMatched: total,
	})
}

// buildUsageLogEntry 把一条用量事件转成明细行（含反查账号昵称）。
func buildUsageLogEntry(deps Deps, ev usagepkg.Event) usageLogEntry {
	e := usageLogEntry{
		Time:             ev.Time.Format("2006-01-02 15:04:05"),
		Account:          ev.Account,
		Model:            deps.resolveModelKey(ev),
		Status:           ev.Status,
		OK:               ev.OK,
		DurationMS:       ev.DurationMS,
		TTFTMS:           ev.TTFTMS,
		Stream:           ev.Stream,
		UsageKnown:       ev.UsageKnown,
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		TotalTokens:      ev.TotalTokens,
		Credits:          ev.Credit,
		Error:            ev.Error,
	}
	// 缓存命中率：分母为 0 时留 nil（"没有缓存数据" ≠ "命中率 0"）
	if denom := ev.CacheHitTokens + ev.CacheMissTokens; denom > 0 {
		rate := float64(ev.CacheHitTokens) / float64(denom)
		e.CacheHitRate = &rate
	}
	// 反查昵称（账号已删则留空，前端退回脱敏 UID）
	if acct, ok := lookupAccountByMasked(deps, ev.Account); ok {
		e.Nickname = acct.Nickname
		e.Platform = acct.PlatformOf()
	}
	return e
}

// clampLimit 把 limit 参数夹到 [1, usageLogMaxLimit]。
//
// 🔴 必须夹：limit 来自客户端，不封顶就是自我 DoS 的入口。
// 缺省/非法值 ⇒ 用默认值（不是报错：让面板少传参数也能工作）。
func clampLimit(raw string) int {
	if raw == "" {
		return usageLogDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return usageLogDefaultLimit
	}
	if n > usageLogMaxLimit {
		return usageLogMaxLimit
	}
	return n
}

// platformLabelOfAccount 返回账号的平台显示名（供其他接口复用）。
func platformLabelOfAccount(a *auth.Account) string {
	if a != nil && a.IsIntl() {
		return "国际版"
	}
	return "国内版"
}
