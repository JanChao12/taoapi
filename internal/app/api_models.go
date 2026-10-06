package app

import (
	"net/http"
	"sort"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// ─────────────────────────────────────────────────────────────
// 面板「API 接入」页的模型列表接口
//
// 🔴 为什么需要这个接口（真实 bug，2026-10-06 委托方实测反馈）：
//
//	面板原本直接请求 `/v1/models` 来渲染「模型列表」，但那是**对外 OpenAI
//	兼容接口**，被 requireAPIKey 包着 —— 一旦用户设了密钥，面板（只带
//	X-WBAPI-Panel CSRF 令牌、不带 Bearer）就会被 **401 拒绝**，
//	页面上显示「加载失败」。
//
//	而同一个页面上的「模型数 31」却是正常的 —— 因为它取自 `/status`
//	（同源内部接口，不校验 Bearer）。**两条数据来源鉴权要求不同**，
//	于是出现"数得出来 31 个，却列不出来"这一自相矛盾的现象。
//
// 🔴 修法的方向是【改面板怎么拿数据】，**不是**放开 /v1/models 的鉴权 ——
// 后者是对外契约，必须保持鉴权。本接口是面板自己的同源只读接口，
// 与 /status 同级。
//
// 为什么返回的是 openai.ModelList（而不是裸数组）：
// 面板 renderApiModels 读的是 d.data，且条目里要 context_length /
// reasoning_efforts 等扩展字段 —— 直接复用 /v1/models 的转换函数
// 可以保证两者**永远形状一致**，不会出现"面板显示的和客户端拿到的不一样"。
// ─────────────────────────────────────────────────────────────

// handlePanelModels 返回模型目录，供面板渲染。
//
// 与 /v1/models 的区别只有一个：**不校验 API key**（同源面板接口）。
// 数据本身取自 Router 的模型目录；与 /v1/models 的**唯一**差别是
// **不含别名**（面板只列真实模型，见下面 ModelsWithoutAliases 的说明）。
//
// 为什么不需要 guardManagementAPI：本接口是**只读**的（仅 GET），
// 而 CSRF 防护针对的是有副作用的写操作（参照 /status 也不套）。
func handlePanelModels(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, openai.ErrTypeInvalidRequest,
			"method_not_allowed", "只支持 GET")
		return
	}

	// Router 未装配时返回空列表而不是错误 —— 与 /v1/models 的行为保持一致，
	// 这样面板能正常渲染成「暂无模型」，而不是「加载失败」。
	if deps.Router == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"object":     "list",
			"data":       []openai.Model{},
			"aliasCount": 0,
		})
		return
	}

	// 🔴 用 ModelsWithoutAliases：面板只列"真实模型"。
	//
	//	委托方原话：「映射的不需要出现在这里」——
	//	别名（形如 `dsf → workbuddy/...`）是给客户端调用的内部映射，
	//	混在模型目录里只会干扰阅读。
	//
	// ⚠️ 但 /v1/models **仍然包含别名**（对外契约不能动，见 router.Models 注释）。
	//	两者不一致是**刻意的**：面板看"有哪些模型"，客户端看"能调哪些 ID"。
	models := deps.Router.ModelsWithoutAliases()

	// 用带 aliasCount 的信封返回，让面板能解释"为什么这里比 /v1/models 少几条"。
	// 单独包一层而不是塞进 openai.ModelList：那个结构体是对外契约的一部分，
	// 不该被面板的展示需求污染（而且加字段会让上一条测试的"两接口逐字节相等"失效
	// —— 那条断言已经按新契约改为"同一批模型"）。
	all := openai.FromProviderModels(models).Data

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   all,

		// 🔴 平台分组（2026-10-06 委托方要求）。
		//
		//	原话：「接口列表我说过要有不同平台的分组……我们应该做两个这样的
		//	分组可以选，将模型数也显示在这里面，选中哪个分组下方的模型列表
		//	就只显示对应平台的」。
		//
		//	为什么由**后端**给分组而不是前端按 ID 前缀切：
		//	  前端切前缀等于把"前缀规则"复制一份到前端，
		//	  规则一变两处就会不一致（本项目的模型前缀已经改过一次）。
		//	  后端本来就持有 provider 列表，是唯一权威来源。
		"groups":     platformGroups(deps, all),
		"aliasCount": deps.Router.AliasCount(),
	})
}

// modelGroup 是一个平台分组（面板顶部的可切换标签）。
type modelGroup struct {
	// ID 平台标识（"cn"/"intl"），与账号的 platform 字段同源。
	ID string `json:"id"`

	// Label 显示名（"国内版"/"国际版"）。
	Label string `json:"label"`

	// Prefix 该平台模型 ID 的前缀（"workbuddy/"），面板用于提示用户。
	Prefix string `json:"prefix"`

	// Count 该平台模型数 —— 委托方明确要求显示在分组标签上。
	Count int `json:"count"`
}

// platformGroups 按渠道前缀把模型分组。
//
// 分组的权威来源是 **Router 里已注册的 provider**（不是硬编码平台列表）：
// 这样"只用一个平台"的部署不会出现一个空分组标签。
//
// ⚠️ 分组顺序：国内版在前、国际版在后（稳定，避免每次刷新位置乱跳）。
func platformGroups(deps Deps, models []openai.Model) []modelGroup {
	if deps.Router == nil {
		return []modelGroup{}
	}

	// 统计各前缀下的模型数
	counts := map[string]int{}
	for _, m := range models {
		for i := 0; i < len(m.ID); i++ {
			if m.ID[i] == '/' {
				counts[m.ID[:i]]++
				break
			}
		}
	}

	// 固定顺序：国内版 → 国际版 → 其他（未知前缀放最后，仍然列出）
	order := []string{workbuddy.ProviderID, workbuddy.ProviderIDIntl}
	seen := map[string]bool{}
	groups := make([]modelGroup, 0, len(counts))

	add := func(prefix string) {
		if seen[prefix] {
			return
		}
		n, ok := counts[prefix]
		if !ok || n == 0 {
			return // 该平台没注册或没模型 —— 不显示空标签
		}
		seen[prefix] = true
		groups = append(groups, modelGroup{
			ID:     platformIDOfPrefix(prefix),
			Label:  platformLabelOfPrefix(prefix),
			Prefix: prefix + "/",
			Count:  n,
		})
	}

	for _, p := range order {
		add(p)
	}
	// 其余未知前缀（将来接第三个平台时自动出现，不必改这里）
	rest := make([]string, 0, len(counts))
	for p := range counts {
		if !seen[p] {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	for _, p := range rest {
		add(p)
	}
	return groups
}

// platformIDOfPrefix 把渠道前缀转成平台标识（与账号的 platform 同源）。
func platformIDOfPrefix(prefix string) string {
	switch prefix {
	case workbuddy.ProviderID:
		return auth.PlatformCN
	case workbuddy.ProviderIDIntl:
		return auth.PlatformIntl
	}
	return prefix
}

// platformLabelOfPrefix 返回渠道前缀的中文显示名。
func platformLabelOfPrefix(prefix string) string {
	switch prefix {
	case workbuddy.ProviderID:
		return "国内版"
	case workbuddy.ProviderIDIntl:
		return "国际版"
	}
	return prefix
}

// ─────────────────────────────────────────────────────────────
// 面板「重新拉取上游目录」
// ─────────────────────────────────────────────────────────────

// modelRefreshSource 报告某个平台这次刷新的结果。
type modelRefreshSource struct {
	// Prefix 渠道前缀（workbuddy/ / workbuddyai/）。
	Prefix string `json:"prefix"`

	// OK 本次是否成功刷新。
	OK bool `json:"ok"`

	// Count 刷新后的模型数（OK=false 时为**原有的**数量，不是 0）。
	Count int `json:"count"`

	// Error 失败原因（已脱敏；OK=true 时为空）。
	Error string `json:"error,omitempty"`
}

// modelRefreshResult 是刷新接口的响应。
type modelRefreshResult struct {
	OK bool `json:"ok"`

	// Total 刷新后的总模型数。
	Total int `json:"total"`

	// Sources 逐平台结果。
	Sources []modelRefreshSource `json:"sources"`

	// Failed 有多少个平台刷新失败（>0 时前端应提示"部分平台未更新"）。
	Failed int `json:"failed"`
}

// handlePanelModelsRefresh 让服务**真的去上游重拉**模型目录。
//
// 🔴 为什么需要它（2026-10-07）：
//
//	面板原有的「刷新」按钮只调 `/api/models`，而那个接口读的是
//	`Router` 的**内存**；而内存只在**启动时**写入一次
//	（serve.go 的 registerOnePlatform）。⇒ 上游改了模型（新增/下架/改价），
//	点刷新**看不到**，必须重启服务。
//
//	本接口补上"服务端重拉"这一环，让那个按钮名副其实。
//
// 安全与语义约定：
//   - **有副作用**（会改路由表）⇒ 必须走 guardManagementAPI（CSRF 防护）
//   - **失败不清空**：某平台拉取失败时保留该平台原有列表
//     （`Router.Refresh` 已保证：失败或返回 0 条都不动原列表）
//   - **不因部分失败而整体失败**：一个平台挂了，另一个平台照常刷新
//   - 返回逐平台结果，让面板能如实说明"哪个平台没更新"
func handlePanelModelsRefresh(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Router == nil || deps.Accounts == nil {
		writeError(w, http.StatusServiceUnavailable, openaiErrTypeServer,
			"not_ready", "服务尚未装配完成")
		return
	}

	client := workbuddy.NewClient()
	out := modelRefreshResult{OK: true, Sources: []modelRefreshSource{}}

	for _, plat := range []workbuddy.Platform{workbuddy.PlatformCN, workbuddy.PlatformIntl} {
		prefix := plat.ProviderID()
		src := modelRefreshSource{Prefix: prefix}

		candidates := activeAccountsOfPlatform(deps.Accounts, plat)
		if len(candidates) == 0 {
			// 该平台没有账号：不是错误（用户可能只用一版）
			src.Count = deps.Router.CountByProvider(prefix)
			out.Sources = append(out.Sources, src)
			continue
		}

		ok, used, errs := refreshOnePlatform(deps.Router, client, candidates, 30*time.Second)
		// ⚠️ Count 取**当前路由里**的数量：失败时它自然等于原值。
		src.Count = deps.Router.CountByProvider(prefix)
		if ok {
			src.OK = true
			if used > 0 {
				deps.logf("刷新 %s 平台模型目录成功（第 %d 个账号才成功）", prefix, used+1)
			} else {
				deps.logf("刷新 %s 平台模型目录成功", prefix)
			}
		} else {
			out.OK = false
			out.Failed++
			// 只报最后一个错误，避免把整串失败都塞给前端
			if len(errs) > 0 {
				src.Error = errs[len(errs)-1].Error()
			} else {
				src.Error = "刷新失败"
			}
			// 失败也要报明细（哪个账号挂了），便于用户定位是否该重新登录
			deps.logf("⚠️ 刷新 %s 平台模型目录失败（%d 个账号都试过）: %s",
				prefix, len(candidates), src.Error)
		}
		out.Sources = append(out.Sources, src)
	}

	out.Total = len(deps.Router.ModelsWithoutAliases())
	writeJSON(w, http.StatusOK, out)
}
