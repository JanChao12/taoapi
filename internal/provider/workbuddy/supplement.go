package workbuddy

import (
	"sort"
	"strings"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// ── 目录外模型补充表 ──
//
// 🔴 这是什么、为什么存在（2026-10-06 委托方拍板）：
//
//	国际版上游 `data.models` 只列 13 个可用模型，但**实测还有 5 个
//	真实模型能正常调用**，只是不在目录里 —— 走目录裁剪就永远拿不到。
//	它们的基础倍率**不在上游任何字段里**（`data.models` 没有、
//	`data.agents` 没有、`modelPromotions` 只有参与活动的），
//	所以只能在这里静态兜底。
//
// ⚠️ 数据来源与可信度（**必须如实标注，不要当成上游实时值**）：
//
//	取自参考项目 wild-work 的 `data/pricing-cache.json`
//	（抓取时间 2026-10-06T18:25:27+08:00，比本项目该次编译还新）。
//
//	**已做交叉验证**：拿该 cache 中 13 个「与我们目录重叠」的条目
//	逐条比对上游实时 `credits`，**12/13 完全吻合** ——
//	⇒ 该 cache 的 rate 是**真实上游倍率**，不是它自己的估算。
//	（唯一不符的 `hy4-preview` 已查明原因，见下。）
//
// 🔴 为什么内嵌而不读那个文件（这是**刻意**的，别改回去）：
//
//	全局评审纪律里最重要的一条否决是「**不要把参考项目的凭据目录
//	变成运行时依赖**」，理由是「那会把外部文件格式变成隐性依赖」。
//	本项目同理：若运行时去读 `D:\tools\wild-work\data\*.json`，
//	就变成「TAOAPI 的正常工作依赖另一个项目的私有文件」——
//	那个文件被删/改名/改格式，本服务就坏了或静默降级。
//	⇒ 所以取**一次性抄录 + 内嵌**，并在下面写清抓取日期。
//
// ⚠️ 已知会过期：倍率可能被上游调整。这是**可接受的**——
//
//	比"整个模型不可见"好。更新方式：重跑
//	`go run ./tools/wbaiprobe -only models` 与上游比对后改这里。
//
// ⚠️ 与 `hy4-preview` 的差异（说明我们的口径更准，不是 bug）：
//
//	cache 记 `hy4-preview rate=0.29 note="Free now"`，而上游目录给
//	`credits="x0.00"`。真相是：`hy4-free-trial-202608` 这条活动
//	`validUntil=2026-09-08` **已过期**，上游目录把活动期的免费值
//	写死进了 credits，cache 则保留基础倍率但留着一个过期标签。
//	我们第 51 轮实现的**时间窗判断**能正确排除过期活动。
type supplementalModel struct {
	// UpstreamID 上游裸 ID（不含渠道前缀）。
	UpstreamID string

	// Name 显示名。上游目录里没有这些模型，拿不到官方 name，
	// 所以按 ID 生成（比留空好，面板不会显示空白行）。
	Name string

	// Multiplier 基础倍率（已交叉验证，见上）。
	Multiplier float64

	// Free 是否明确免费。
	//
	// 🔴 用**显式布尔**而不是 `Multiplier == 0` 判断：
	//	两者含义不同 —— 0 是"明确免费"，而这里没有"未知"的条目
	//	（能进这张表的都有确切倍率）。显式写出来是为了让
	//	将来新增条目时**必须想一下**，而不是默认落成免费。
	Free bool
}

// intlSupplementalModels 是**国际版**目录外的 5 个可调用模型。
//
// 🔴 为什么只给国际版：这 5 个 ID 在国际版能调用、且不在国际版目录里。
// 国内版的情况**完全不同** —— 国内版目录（31 条）里这些 ID 大多存在，
// 只是被 `data.agents` 裁剪规则挡掉了，而**那个裁剪是委托方明确要求的**
// （「我只用客户端能选的那 17 个，老模型明确不要」）。
//
//	⇒ 绝不要把这张表用到国内版上，那会**违反已冻结的产品决策**。
var intlSupplementalModels = []supplementalModel{
	// 免费模型（rate=0）。
	{UpstreamID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash", Multiplier: 0, Free: true},

	// 新加坡节点版本 —— 与上面同名但**是另一个模型**，别合并。
	{UpstreamID: "deepseek-v4.1-flash-sg", Name: "DeepSeek-V4.1-Flash-SG", Multiplier: 0.03},

	{UpstreamID: "glm-5.3-flash", Name: "GLM-5.3-Flash", Multiplier: 0.06},
	{UpstreamID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", Multiplier: 0.77},

	// 倍率最高（6.67），用户最需要看到价格才不会误用。
	{UpstreamID: "gpt-6-astra", Name: "GPT-6-Astra", Multiplier: 6.67},
}

// applySupplementalModels 把目录外模型补进列表（**仅国际版**）。
//
// 规则：
//   - 只对国际版生效（见 intlSupplementalModels 的说明）
//   - **已存在的 ID 不覆盖** —— 上游目录若将来收录了这些模型，
//     以**上游实时数据为准**（那才是权威），本表只做"上游没有时"的兜底。
//     这条很重要：否则上游改了倍率，我们还在显示旧值。
//   - 补齐后再排序，保证 /v1/models 输出稳定可复现
func applySupplementalModels(models []provider.Model, plat Platform) []provider.Model {
	if plat != PlatformIntl {
		return models
	}

	// 上游已有的裸 ID 集合 —— 用来避免覆盖权威数据。
	existing := make(map[string]bool, len(models))
	for _, m := range models {
		existing[m.UpstreamID] = true
	}

	prefix := plat.ProviderID() + "/"
	for _, s := range intlSupplementalModels {
		if existing[s.UpstreamID] {
			// 上游已收录 ⇒ 用上游的（含它的 maxOutput/上下文/倍率）。
			continue
		}
		m := provider.Model{
			ID:         prefix + s.UpstreamID,
			UpstreamID: s.UpstreamID,
			Name:       s.Name,
			Pricing: &provider.Pricing{
				Multiplier:    s.Multiplier,
				HasMultiplier: true,
			},
		}
		// 上下文/能力未知：**留零值**，不臆造。
		//
		//	上游目录没有这些模型的字段，我们无从得知上下文窗口与
		//	是否支持图片/工具。按纪律「宁缺勿假」，不填猜测值 ——
		//	前端对 0 值应显示为"未知"而不是"0 tokens"。
		//
		//	⚠️ 已知局限：面板/客户端可能把 0 显示成 0。
		//	  这是**如实反映"上游未提供"**的代价，优于编造数字。
		models = append(models, m)
		existing[s.UpstreamID] = true
	}

	// 重新排序（新增项要落到正确位置，
	// 否则 /v1/models 的顺序会随"上游是否收录"而变，不利于比对）。
	sortModelsByID(models)
	return models
}

// sortModelsByID 按对外 ID 排序（抽出来供两处复用，避免两套排序规则）。
func sortModelsByID(models []provider.Model) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}

// isSupplementalModel 报告某个裸 ID 是否来自补充表。
//
// 用途：测试与未来面板若想区分"上游目录"与"静态兜底"两类模型。
func isSupplementalModel(upstreamID string) bool {
	for _, s := range intlSupplementalModels {
		if strings.EqualFold(s.UpstreamID, upstreamID) {
			return true
		}
	}
	return false
}
