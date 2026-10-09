package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// modelsMaxResponseBytes 模型目录响应的读取上限。
// 上游实测 31 个模型约 27 KB；留足余量但防止异常响应撑爆内存。
const modelsMaxResponseBytes = 4 << 20 // 4 MiB

// badgeTagPrefix 是运营标签在 tags 里的前缀（实测 "badge:夜间折扣:#1E90FF"）。
const badgeTagPrefix = "badge:"

// wireModel 是上游 /models 响应里的单个模型（字段名直连实测）。
type wireModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Vendor          string `json:"vendor"`
	Credits         string `json:"credits"`
	MaxInputTokens  int64  `json:"maxInputTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	MaxAllowedSize  int64  `json:"maxAllowedSize"`
	SupportsImages  bool   `json:"supportsImages"`
	SupportsTool    bool   `json:"supportsToolCall"`
	SupportsReason  bool   `json:"supportsReasoning"`
	OnlyReasoning   bool   `json:"onlyReasoning"`

	// ContextWindow 只有部分模型提供；不提供时用 MaxInputTokens。
	ContextWindow *struct {
		DefaultLength    int64   `json:"defaultLength"`
		SupportedLengths []int64 `json:"supportedLengths"`
	} `json:"contextWindow"`

	// Tags 上游标签。除普通标记（如 "craft"）外，
	// 运营标签的形态是 "badge:<文案>:<色值>"（实测）。
	Tags []string `json:"tags"`

	// Reasoning 两种形态（见 docs/upstream-contract.md §七）：
	//   A) 声明多档：supportedEfforts + defaultEffort
	//   B) 固定档：effort
	Reasoning *struct {
		Effort             string   `json:"effort"`
		DefaultEffort      string   `json:"defaultEffort"`
		SupportedEfforts   []string `json:"supportedEfforts"`
		CanDisableThinking *bool    `json:"canDisableThinking"`
	} `json:"reasoning"`
}

// wireModelsResponse 是上游响应信封。
type wireModelsResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Models []wireModel `json:"models"`

		// Agents 是各"角色"可用模型的**子集**声明（实测字段）。
		//
		// 🔴 为什么要用它来裁剪模型列表（2026-10-06 委托方实测反馈）：
		//
		//	上游 models 返回 31 个，但 CodeBuddy 客户端里**只能选到 17 个**。
		//	委托方原话：「这17才是我在 workbuddy 客户端里能选择使用的模型，
		//	你之前给我的很多在客户端都看不到，应该是有些过期的老模型」
		//
		//	实测这 17 个 = agents 里 name="cli" 且 tags 含 "default" 的
		//	那条记录的 models 数组（逐条比对过，含倍率数字都对得上）。
		//
		//	⇒ 那 31 个里没被引用的就是**已下架/不再投放**的老模型，
		//	  继续列出来会让用户选到「调不通」的模型。
		Agents []wireAgent `json:"agents"`

		// ModelPromotions 运营活动（2026-10-06 发现并启用）。
		//
		// 🔴 我们此前**完全没读这个字段** —— 只用了 tags 里的 badge 简写。
		//	实测它才是「Free now / 限时免费 / 夜间折扣」的权威来源，
		//	带开关、时间窗、折扣系数与说明文案。
		ModelPromotions []wirePromotion `json:"modelPromotions"`
	} `json:"data"`
}

// wireV3Config 是国际版 /v3/config 的响应信封（2026-10-07 发现并接入）。
//
// 🔴 为什么引入它（补 /v2 的真实缺口）：
//
//	/v2 对国际版只给 18 条（13 真实 + 5 抽象），**不含** 5 个能正常调用
//	的模型（deepseek-v4.1-flash / -sg、glm-5.3-flash、kimi-k2.8-preview、
//	gpt-6-astra）。/v3/config 给 22 条，**包含它们**，且每条 15 个字段
//	（比 /v2 多 contextWindow / maxInputTokens / reasoning 等）。
//
// ⚠️ 与 /v2 的三处关键差异（决定了下面的合并与裁剪策略）：
//  1. **没有 modelPromotions** ⇒ 活动信息只能取 /v2，故必须两源合并。
//  2. **没有 tags** ⇒ badge 简写只在 /v2 有（置 nil 即可，不臆造）。
//  3. **agents.cli 只列 21/22 条，漏掉 deepseek-v4.1-flash-sg**
//     ⇒ 对 /v3 绝不能套用 agents 裁剪，否则会把它砍掉。
//     实测依据：/v3 的 22 条里 `deepseek-v4.1-flash-sg` 不在 cli 的
//     models 数组内，而它在 models 里且有 credits=x0.03。
//
// 字段名与 wireModel 完全一致（实测），所以直接复用 wireModel。
type wireV3Config struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Models []wireModel `json:"models"`
	} `json:"data"`
}

// wireAgent 是一条"角色→可用模型"的声明。
type wireAgent struct {
	Name   string   `json:"name"`
	Tags   []string `json:"tags"`
	Models []string `json:"models"`
}

// wirePromotion 是一条**运营活动**声明（2026-10-06 发现）。
//
// 🔴 为什么它比 tags 里的 badge 更重要：
//
//	我们此前只用 `tags` 里的 `badge:文案:#色值`（那是个**简写**），
//	而 `modelPromotions` 才是**权威来源** —— 它带：
//	  · `enabled` 开关
//	  · `schedule` 生效时间窗（validFrom / validUntil + 时区）
//	  · `discount.factor` 折扣系数（0 = 免费）
//	  · `hover.textZh` 活动说明文案
//	  · `priority` 同一模型有多条活动时的优先级
//
//	实测实例（国际版）：
//	  {"id":"hy3-free-trial-202608","modelIds":["hy3"],
//	   "badge":{"label":"Free now","color":"#FF0000"},
//	   "discount":{"factor":0,"discountedCredits":"0x"},
//	   "schedule":{"timezone":"Asia/Shanghai",
//	               "validFrom":"2026-07-06T00:00:00+08:00",
//	               "validUntil":"2026-09-30T00:00:00+08:00"}}
//
// ⚠️ 与 `credits` 是**两套机制**：
//
//	credits = 基础倍率；promotion = 当前活动（可能覆盖基础倍率）。
//	展示时要分清，不能混成一个数。
type wirePromotion struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // 实测 "discount"
	Enabled  bool     `json:"enabled"`
	ModelIDs []string `json:"modelIds"`
	Priority int      `json:"priority"`

	Badge *struct {
		Label   string `json:"label"`
		Color   string `json:"color"`
		Display string `json:"display"` // 实测 "activeOnly"
	} `json:"badge"`

	Discount *struct {
		// Factor 折扣系数：0 = 完全免费；0.5 = 五折。
		Factor float64 `json:"factor"`
		// DiscountedCredits 上游给的显示串（如 "0x"）。
		DiscountedCredits string `json:"discountedCredits"`
		// DisplayMode 实测 "replace"（用折扣值替换原价显示）。
		DisplayMode string `json:"displayMode"`
	} `json:"discount"`

	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`

	Schedule *struct {
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// defaultReasoningEffort 是委托方要求的默认思考档位。
//
// 依据：委托方明确「默认思考 high」。
// 且实测证明 deepseek 系「不传 = 完全不思考」，所以服务端必须显式注入。
const defaultReasoningEffort = "high"

// validUpstreamEfforts 是上游实测接受的档位白名单。
//
// 🔴 2026-10-09 起改为**转发到 provider 层的唯一权威定义**。
//
//	起因：`off` 曾被本项目当作合法档位对外声明，而直连实测上游对它返回
//	HTTP 400 code=11150。根因就是"哪些档位上游真的接受"这个事实存在
//	多份副本（白名单在 models.go、翻译逻辑在 chat.go、新协议又各写一份），
//	修一处漏一处。现在只有 provider.IsUpstreamEffort 一份。
//
// ⚠️ 保留这个变量名是为了不动 cleanEfforts 的调用点；它的内容
//
//	由 provider.UpstreamEfforts() 派生，不要在这里直接写字面量。
var validUpstreamEfforts = func() map[string]bool {
	m := make(map[string]bool)
	for _, e := range provider.UpstreamEfforts() {
		m[e] = true
	}
	return m
}()

// fetchV3Models 拉取国际版 /v3/config 的模型表。
//
// 返回 error 表示该端点不可用 —— **调用方必须降级而不是失败**
// （/v3 只是补充源，挂了不该让整个模型目录消失）。
//
// ⚠️ 「返回 0 个模型」也按失败处理：若上游改结构导致解析成空，
//
//	静默接受会让那 5 个补充模型**无声消失**，而我们又不会走静态兜底。
//	（wild-work 也有 "v3 config empty models" 这条错误串，同样把空当失败。）
func (c *Client) fetchV3Models(ctx context.Context, cred Credential) ([]wireModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.V3ConfigURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造 /v3/config 请求失败: %w", err)
	}
	applyModelsHeaders(req, cred)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 /v3/config 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 /v3/config 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       snippet(body),
			Op:         "v3-config",
		}
	}

	var payload wireV3Config
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 /v3/config 失败: %w (前 200 字节: %s)", err, snippet(body))
	}
	if payload.Code != 0 {
		return nil, &UpstreamError{
			StatusCode: resp.StatusCode,
			BizCode:    payload.Code,
			BizMsg:     payload.Msg,
			Op:         "v3-config",
		}
	}
	if len(payload.Data.Models) == 0 {
		return nil, fmt.Errorf("/v3/config 返回 0 个模型（按失败处理，避免补充模型无声消失）")
	}
	return payload.Data.Models, nil
}

// convertCatalogModels 把上游模型记录转成对外模型，并按规则裁剪。
//
// useAgents=false 表示**不套用 agents 裁剪** —— 仅供 /v3/config 使用，
// 因为它的 agents.cli 漏掉了 deepseek-v4.1-flash-sg（见 wireV3Config 注释）。
//
// 无论哪个源，`auto` 与（国际版的）5 个抽象档位**一律排除**：
// 这是委托方明确要求，且必须独立于裁剪判断（见下方 Models 的说明）。
func convertCatalogModels(wms []wireModel, allowed map[string]bool, useAgents bool, plat Platform) []provider.Model {
	out := make([]provider.Model, 0, len(wms))
	for _, wm := range wms {
		id := strings.TrimSpace(wm.ID)
		if id == "" {
			// 没有 id 的记录跳过，而不是让整个目录失败。
			continue
		}
		if id == autoModelID {
			continue
		}
		if plat.DropsAbstractModels() && isAbstractModel(id) {
			continue
		}
		if useAgents && len(allowed) > 0 && !allowed[id] {
			continue
		}
		m, ok := convertModel(wm, plat)
		if !ok {
			continue
		}
		out = append(out, m)
	}
	return out
}

// mergeModels 合并两份模型列表，**first 优先**（同裸 ID 时保留 first 的）。
//
// 按 UpstreamID（裸 ID）判重，因为两个端点给的是同一个上游模型：
// /v3 的 gpt-6-astra 与 /v2 的 gpt-6-astra 是同一个东西，不能重复列出。
//
// ⚠️ first 的字段被整条保留（不是逐字段合并）：/v3 的记录字段更全
// （15 vs 13），逐字段合并反而要用"零值判断"猜哪个字段有效，
// 容易把合法的 0 当成"没给"（本项目在 cache 字段上踩过同类坑）。
func mergeModels(first, second []provider.Model) []provider.Model {
	seen := make(map[string]bool, len(first)+len(second))
	out := make([]provider.Model, 0, len(first)+len(second))
	for _, list := range [][]provider.Model{first, second} {
		for _, m := range list {
			if seen[m.UpstreamID] {
				continue
			}
			seen[m.UpstreamID] = true
			out = append(out, m)
		}
	}
	return out
}

// Models 拉取并转换上游模型目录。
//
// 转换规则（经 DSH × Codex 确认）：
//   - 模型 ID 加**平台前缀**（国内 workbuddy/、国际 workbuddyai/）
//   - 上下文取【最大值】（maxInputTokens），不是默认档（contextWindow.defaultLength）
//   - 档位按【实测】裁剪，不是照抄上游声明
//
// ⚠️ plat 决定前缀与是否砍抽象档位 —— 两版的模型集**部分重叠**，
// 用同一前缀会让同名模型互相覆盖（见 ProviderIDIntl 的注释）。
func (c *Client) Models(ctx context.Context, cred Credential, plat Platform) ([]provider.Model, error) {
	if plat == "" {
		plat = PlatformCN
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelsURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造模型目录请求失败: %w", err)
	}
	applyModelsHeaders(req, cred)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求模型目录失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取模型目录响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       snippet(body),
			Op:         "models",
		}
	}

	var payload wireModelsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w (前 200 字节: %s)", err, snippet(body))
	}
	if payload.Code != 0 {
		return nil, &UpstreamError{
			StatusCode: resp.StatusCode,
			BizCode:    payload.Code,
			BizMsg:     payload.Msg,
			Op:         "models",
		}
	}

	// 客户端实际可选的模型集合（见 wireModelsResponse.Agents 的说明）。
	allowed := clientSelectableModels(payload.Data.Agents)

	// ── /v2 的模型：按 agents 裁剪 ──
	//
	//	为什么必须裁：上游 models 里含**已下架/不再投放**的老模型
	//	（委托方实测：31 个里客户端只能选 17 个）。列出来会让用户
	//	选到调不通的模型 —— 那是"看起来能用、实际报错"的坏体验。
	v2Models := convertCatalogModels(payload.Data.Models, allowed, true, plat)

	// ── /v3/config 的模型（仅国际版）：**不裁剪** ──
	//
	// 🔴 2026-10-07 接入。它是 /v2 的**补充源**，不是替代：
	//   · 补上 /v2 未发布的 5 个真实模型（deepseek-v4.1-flash 等）
	//   · 字段更全（15 vs 13），重叠模型取值与 /v2 实测 15/15 一致
	//   · 但**没有 modelPromotions** ⇒ 活动仍取 /v2，故必须合并
	//
	// ⚠️ 失败必须降级为"只用 /v2 + 静态兜底"，绝不能整体失败 ——
	//	/v3 挂了不该让整个模型目录消失。
	v3Models := []provider.Model{}
	if plat == PlatformIntl {
		if wm3, err := c.fetchV3Models(ctx, cred); err == nil {
			// ⚠️ useAgents=false：/v3 的 agents.cli 漏掉 deepseek-v4.1-flash-sg，
			//	套用裁剪会把它砍掉（见 wireV3Config 注释）。
			v3Models = convertCatalogModels(wm3, nil, false, plat)
		}
	}

	// /v3 优先（字段更全、且是"当前客户端"的视角）；
	// /v2 补上它独有的（如 gpt-5.3-codex）。
	models := mergeModels(v3Models, v2Models)

	// 稳定排序，保证 /v1/models 输出可复现（便于测试与人工比对）。
	sortModelsByID(models)

	// ── 静态兜底：**已删除**（2026-10-09 委托方要求）──
	//
	// 🔴 这里原先在 `/v3` 拉取失败时，用一张内嵌的**静态倍率快照**把
	//	5 个"目录外模型"补回来。委托方要求删除，原话：
	//
	//	  「赶紧将静态表删了，如果以后上游改动了，静态表的数据都是假数据，
	//	    没有意义。」
	//
	//	理由（详见 supplement.go 的包注释，那里留了完整因果）：
	//	那张表只在"上游拉取失败"时启用，而那一刻恰恰最可能是
	//	"上游改了定价 / 下架了模型" —— 它会在最不该被信任的时候
	//	冒充权威数据（显示过期倍率、复活已下架模型）。
	//
	//	⇒ 现在 /v3 失败就是**如实少列**那 5 个模型，
	//	  显示的每个数字都来自上游，没有一个是编的。
	//	  （实测：/v3 正常时这 5 个本来就由上游给出，正常运行一个都不少。）

	// ── 应用运营活动（2026-10-06）──
	//
	// 🔴 放在最后统一应用，而不是在循环里逐条查：
	//	一个模型可能被多条活动命中（不同 priority/时间窗），
	//	统一处理才好挑出"当前生效且优先级最高"的那条。
	applyPromotions(models, payload.Data.ModelPromotions, time.Now())

	return models, nil
}

// applyPromotions 把生效中的活动应用到模型上。
//
// 规则（按重要性）：
//  1. 只应用 `enabled == true` 的
//  2. 只应用**当前在生效期内**的（按 schedule 的时间窗）
//  3. 同一模型有多条时取 `priority` 最高的
//  4. 活动**覆盖** tags 里的 badge 简写（它更权威）
//
// ⚠️ 时间窗解析失败时**跳过该活动**（宁可不显示，也不要显示一个
//
//	可能已过期的"免费"标签 —— 那会误导用户以为不花钱）。
func applyPromotions(models []provider.Model, promos []wirePromotion, now time.Time) {
	if len(promos) == 0 {
		return
	}

	// 先挑出"该模型当前最该显示的那条活动"
	best := map[string]*wirePromotion{}
	for i := range promos {
		p := &promos[i]
		if !p.Enabled {
			continue
		}
		if !promotionActive(p, now) {
			continue
		}
		for _, mid := range p.ModelIDs {
			cur := best[mid]
			if cur == nil || p.Priority > cur.Priority {
				best[mid] = p
			}
		}
	}
	if len(best) == 0 {
		return
	}

	for i := range models {
		// Model.ID 带平台前缀，活动里的 modelIds 是裸 ID
		up := models[i].UpstreamID
		p, ok := best[up]
		if !ok {
			continue
		}
		models[i].Promotion = promotionOf(p)
	}
}

// promotionActive 判断活动当前是否在生效期内。
//
// ⚠️ 时间窗缺失或无法解析 ⇒ **视为不生效**（保守）。
//
//	理由同上：宁可漏显示一个免费标签，也不要显示一个已过期的。
func promotionActive(p *wirePromotion, now time.Time) bool {
	if p.Schedule == nil {
		// 没有时间窗：视为长期有效（上游确实可能不给 schedule）
		return true
	}
	if p.Schedule.ValidFrom != "" {
		from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom)
		if err != nil {
			return false
		}
		if now.Before(from) {
			return false
		}
	}
	if p.Schedule.ValidUntil != "" {
		until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil)
		if err != nil {
			return false
		}
		if now.After(until) {
			return false
		}
	}
	return true
}

// promotionOf 把上游活动转成对外结构。
//
// 颜色同样做安全校验（只放行 #RGB/#RRGGBB）——
// 它会进内联 style，不能让上游塞任意 CSS。
func promotionOf(p *wirePromotion) *provider.Promotion {
	out := &provider.Promotion{
		ID:       p.ID,
		Kind:     p.Kind,
		Priority: p.Priority,
	}
	if p.Badge != nil {
		out.Label = p.Badge.Label
		if isSafeColor(strings.TrimSpace(p.Badge.Color)) {
			out.Color = strings.TrimSpace(p.Badge.Color)
		}
	}
	if p.Discount != nil {
		out.Factor = p.Discount.Factor
		// Factor==0 表示**完全免费**（实测 hy3 就是 0）。
		// 这与"没有折扣信息"含义不同，所以用显式布尔表达。
		out.Free = p.Discount.Factor == 0
	}
	if p.Hover != nil {
		out.Note = p.Hover.TextZh
	}
	if p.Schedule != nil {
		out.ValidUntil = p.Schedule.ValidUntil
	}
	return out
}

// abstractModelIDs 是国际版的 5 个**抽象档位**模型。
//
// 🔴 委托方要求砍掉（原话）：
//
//	「5 个抽象档位：default-model/fast-model/balanced-model/
//	  primary-model/deep-model 砍掉我不要」
//
// 这些不是具体模型，而是"按用途自动选"的包装（类似 auto）：
// 倍率固定写死但没有明确的能力语义，用户不知道该期待什么。
// 实测其中一个（deep-model）调用还直接 500。
//
// ⚠️ 只在**国际版**排除 —— 国内版目录里本来就没有这些 ID。
var abstractModelIDs = map[string]bool{
	"default-model":  true,
	"fast-model":     true,
	"balanced-model": true,
	"primary-model":  true,
	"deep-model":     true,
}

// isAbstractModel 报告某个（裸）模型 ID 是否是抽象档位。
func isAbstractModel(id string) bool { return abstractModelIDs[id] }

// DropsAbstractModels 报告该平台是否要砍掉抽象档位。
//
// 目前只有国际版有这些条目，所以只对国际版生效；
// 独立成方法是为了将来国内版若也出现同类条目时能一处改。
func (p Platform) DropsAbstractModels() bool { return p == PlatformIntl }

// autoModelID 是上游的"自动选模型"条目。
//
// 🔴 为什么要显式排除它（2026-10-06 委托方要求，原话）：
//
//	「还有我建议不要auto这个模型，因为这是自动模型他的倍率本来就不固定，
//	  而且不建议使用这个，所以别用这个模型了。」
//
// 这是有依据的：上游自己的描述就是"积分倍率随之浮动"
// （descriptionZh: "平衡效果与速度。自动为每个任务匹配最优模型，积分倍率随之浮动"），
// 因此它的 credits 字段**本来就是空的** —— 面板只能显示 unknown，
// 用户看到一个没有价格的选项，反而更困惑。
//
// ⚠️ 排除的**只有这一个 ID**，不是按"倍率为空"批量排除 ——
// 后者会误伤将来上游新增的、尚未定价的正常模型。
const autoModelID = "auto"

// clientSelectableModels 提取"客户端实际能选"的模型 ID 集合。
//
// 实测（2026-10-06）：agents 里 name="cli" 且 tags 含 "default" 的那条，
// 其 models 数组就是 CodeBuddy 客户端下拉框里的模型列表。
//
// ⚠️ 不硬编码 name/tags 的**具体值**做唯一判据：优先找带 "default" 标签的；
// 找不到时退回找 name=="cli" 的 —— 上游改命名时不至于整个失效。
func clientSelectableModels(agents []wireAgent) map[string]bool {
	var picked *wireAgent
	for i := range agents {
		if hasTag(agents[i].Tags, "default") {
			picked = &agents[i]
			break
		}
	}
	if picked == nil {
		for i := range agents {
			if agents[i].Name == "cli" {
				picked = &agents[i]
				break
			}
		}
	}
	if picked == nil || len(picked.Models) == 0 {
		return nil // 上游没给可用信息 ⇒ 调用方不裁剪
	}
	set := make(map[string]bool, len(picked.Models))
	for _, id := range picked.Models {
		set[strings.TrimSpace(id)] = true
	}
	return set
}

// hasTag 报告 tags 里是否含某个标签。
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.TrimSpace(t) == want {
			return true
		}
	}
	return false
}

// applyModelsHeaders 注入模型目录请求头。
func applyModelsHeaders(req *http.Request, cred Credential) {
	commonHeaders(req)
	if cred.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	}
	if cred.UID != "" {
		req.Header.Set("X-User-Id", cred.UID)
		req.Header.Set("X-Machine-ID", cred.MachineID())
		req.Header.Set("X-Session-ID", cred.SessionID())
	}
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if cred.Domain != "" {
		req.Header.Set("X-Domain", cred.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Agent-Purpose", agentPurpose)
	req.Header.Set("X-IDE-Name", ideName)
	req.Header.Set("X-IDE-Type", ideType)
	req.Header.Set("X-IDE-Version", ideVersion())
	req.Header.Set("X-Product", ideName)
	setTraceHeaders(req, newMessageID())
}

// convertModel 把一个上游模型转换成对外模型。
//
// 返回 ok=false 表示该记录不可用（缺 id），调用方应跳过。
func convertModel(wm wireModel, plat Platform) (provider.Model, bool) {
	id := strings.TrimSpace(wm.ID)
	if id == "" {
		return provider.Model{}, false
	}

	// 上下文取最大值：优先 maxInputTokens，其次 contextWindow 的最大档。
	ctxWin := wm.MaxInputTokens
	if wm.ContextWindow != nil && len(wm.ContextWindow.SupportedLengths) > 0 {
		maxLen := wm.ContextWindow.SupportedLengths[0]
		for _, l := range wm.ContextWindow.SupportedLengths {
			if l > maxLen {
				maxLen = l
			}
		}
		if maxLen > ctxWin {
			ctxWin = maxLen
		}
	}

	name := strings.TrimSpace(wm.Name)
	if name == "" {
		name = id
	}

	// 倍率（实测字段 credits，形如 "x0.03"、"x0.00 credits"、"x2.20 credits"）。
	// 空值 ⇒ 上游没定价 ⇒ HasMultiplier=false（**不是** 0=免费）。
	pr := parseCredits(wm.Credits)

	// 运营标签（实测藏在 tags 里，形如 "badge:夜间折扣:#1E90FF"）。
	bd := parseBadge(wm.Tags)

	m := provider.Model{
		ID:         plat.ProviderID() + "/" + id,
		UpstreamID: id,
		Name:       name,
		Capabilities: provider.Capabilities{
			ContextWindow:   ctxWin,
			MaxOutputTokens: wm.MaxOutputTokens,
			SupportsImages:  wm.SupportsImages,
			SupportsTools:   wm.SupportsTool,
			Reasoning:       resolveReasoning(id, wm),
		},
	}
	if pr != nil {
		m.Pricing = pr
	}
	if bd != nil {
		m.Badge = bd
	}
	return m, true
}

// parseCredits 解析上游的倍率字符串。
//
// 实测原文形态（2026-10-06 采样）：
//
//	"x0.03"           → 0.03
//	"x0.00 credits"   → 0.00（免费）
//	"x2.20 credits"   → 2.20
//	""                → nil（**上游未定价**）
//
// 🔴 返回 nil 与返回 0 含义完全不同：
//
//	nil ⇒ 上游没标价（未知），面板显示"unknown"
//	0   ⇒ 明确免费，面板显示"Free"
//
// 混用会把"没标价"显示成"免费"，直接误导用户。
//
// 🔴 另外拒绝**负数与非有限值**（Codex 第 40 轮指出）：
// 倍率为负没有业务含义，出现即说明上游格式变了或有脏数据；
// NaN/Inf 更会让前端显示成 "xNaN"。这类值一律归为"未知"，
// **不能**落成 0（那会被显示成 Free，是最坏的误读方向）。
func parseCredits(s string) *provider.Pricing {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// 去掉前导 x/X，再取开头的数字部分（后面的 "credits" 等后缀忽略）
	s = strings.TrimLeft(s, "xX")

	// 允许前导负号 —— 但要显式检查：直接跳过 '-' 会让 "-0.5" 被解析成 0.5。
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}

	end := 0
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	if end == 0 {
		// 有值但不是数字（上游改了格式）——宁可当未知，也不要瞎猜
		return nil
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return nil
	}
	if neg {
		// 负倍率无意义 —— 归为未知，**不是** 0
		return nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &provider.Pricing{Multiplier: v, HasMultiplier: true}
}

// parseBadge 从 tags 里找运营标签。
//
// 实测原文形态："badge:夜间折扣:#1E90FF"
// 约定：badge:<文案>:<颜色>。颜色缺失时仍取文案（面板用默认色兜底）。
//
// ⚠️ 文案由上游实时下发，会随时段/活动变化
// （实测同一天不同时刻拿到过「限时免费」与「夜间免费」）。
// 所以**不要**把它当静态配置缓存起来。
func parseBadge(tags []string) *provider.Badge {
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if !strings.HasPrefix(t, badgeTagPrefix) {
			continue
		}
		rest := strings.TrimPrefix(t, badgeTagPrefix)
		if rest == "" {
			continue
		}
		text, color := rest, ""
		if i := strings.LastIndex(rest, ":"); i > 0 {
			text = rest[:i]
			color = rest[i+1:]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		// 🔴 颜色只接受 #RRGGBB / #RGB（Codex 第 40 轮：颜色会被写进
		//   内联 style，不能把上游给的任意字符串直接拼进 CSS）。
		//   不合法就当没有颜色 —— 前端会用中性灰兜底，
		//   而不是渲染出一个可能带 url()/expression 的危险值。
		if !isSafeColor(strings.TrimSpace(color)) {
			color = ""
		}
		return &provider.Badge{Text: text, Color: strings.TrimSpace(color)}
	}
	return nil
}

// isSafeColor 校验颜色值是否是可安全内联的十六进制色。
//
// 只放行 #RGB / #RRGGBB —— 这是上游实测给的形态（如 "#FF0000"）。
// 其余（含 rgb()、命名色、空串、以及任何可能携带 CSS 语义的字符串）
// 一律拒绝，由前端用默认色兜底。
func isSafeColor(s string) bool {
	if len(s) != 4 && len(s) != 7 {
		return false
	}
	if s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// resolveReasoning 决定一个模型对外暴露的思考档位。
//
// ⚠️ 关键设计：**以实测为准，不以声明为准**。
//
// 实测结论（docs/upstream-contract.md §七）：
//   - space-bunny         → knob，5 档真实有效
//   - glm-5.3             → knob-flat，仅 max 显著
//   - deepseek-v4.1-flash → switch，档位无差异但"不传"= 完全不思考
//
// 因此：
//   - 已实测的模型 → 用实测结果（verifiedEfforts）
//   - 未实测的模型 → 退回上游声明，但标记 Verified=false
func resolveReasoning(id string, wm wireModel) *provider.ReasoningCapability {
	// 1) 先查实测表（权威）
	if known, ok := probedReasoning[id]; ok {
		rc := known // 复制，避免调用方改到全局表
		return &rc
	}

	// 2) 未实测：按上游声明做保守推断
	if wm.Reasoning == nil {
		if !wm.SupportsReason && !wm.OnlyReasoning {
			return nil // 明确不支持思考
		}
		// 声明支持思考但没给档位信息 → 视为开关型，只给 off/high
		return &provider.ReasoningCapability{
			Mode:     provider.ModeSwitch,
			Levels:   []string{"off", defaultReasoningEffort},
			Default:  defaultReasoningEffort,
			Verified: false,
		}
	}

	efforts := cleanEfforts(wm.Reasoning.SupportedEfforts)
	if len(efforts) > 0 {
		def := strings.TrimSpace(wm.Reasoning.DefaultEffort)
		if def == "" || !containsStr(efforts, def) {
			def = highestEffort(efforts)
		}
		// 声明多档 → 先当作 knob（未实测，标记 Verified=false）
		return &provider.ReasoningCapability{
			Mode:     provider.ModeKnob,
			Levels:   append([]string{"off"}, efforts...),
			Default:  def,
			Verified: false,
		}
	}

	// 只有固定档（形态 B）→ 开关型
	fixed := strings.TrimSpace(wm.Reasoning.Effort)
	if fixed == "" {
		fixed = defaultReasoningEffort
	}
	return &provider.ReasoningCapability{
		Mode:     provider.ModeSwitch,
		Levels:   []string{"off", fixed},
		Default:  fixed,
		Verified: false,
	}
}

// probedReasoning 是【实测校正表】。
//
// 每个条目都来自直连实测（n=6/档），证据见 docs/upstream-contract.md。
// 新增模型实测后往这里加即可；未列入的走 resolveReasoning 的保守推断。
var probedReasoning = map[string]provider.ReasoningCapability{
	// 实测：真旋钮，5 档单调递增
	// 均值 44 → 53.7 → 61.7 → 79.7 → 83.7；low 上限 47 < xhigh 下限 55
	"space-bunny": {
		Mode:     provider.ModeKnob,
		Levels:   []string{"off", "low", "medium", "high", "xhigh", "max"},
		Default:  "high", // 委托方要求默认 high（上游默认是 max，我们统一为 high）
		Verified: true,
	},

	// 实测：低档≈不思考，max 突变
	// low 中位 78 / high 178 / max 3232（跳 18 倍）
	"glm-5.3": {
		Mode:     provider.ModeKnobFlat,
		Levels:   []string{"off", "low", "high", "max"},
		Default:  defaultReasoningEffort,
		Verified: true,
	},

	// 实测：纯开关。各档 178–257 随机排列；不传 = 0（完全不思考）
	// → 只给 off/high 两档，服务端在不传时注入 high
	"deepseek-v4.1-flash": {
		Mode:     provider.ModeSwitch,
		Levels:   []string{"off", defaultReasoningEffort},
		Default:  defaultReasoningEffort,
		Verified: true,
	},
}

// cleanEfforts 清洗上游声明的档位列表：
//   - 去掉空白、空串
//   - 去掉上游不认的值（避免透传后被 400）
//   - 去重并保持原有顺序
func cleanEfforts(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		if !validUpstreamEfforts[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// highestEffort 返回档位列表里最高的一档（按预定义顺序）。
func highestEffort(levels []string) string {
	rank := map[string]int{
		"minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6, "ultra": 7,
	}
	best, bestRank := "", 0
	for _, l := range levels {
		if r := rank[l]; r > bestRank {
			best, bestRank = l, r
		}
	}
	if best == "" {
		return defaultReasoningEffort
	}
	return best
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// snippet 截断响应体用于错误信息（避免把大响应或凭据写进日志）。
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
