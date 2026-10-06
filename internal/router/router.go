// Package router 负责把入站请求路由到对应的 provider，
// 并做模型名前缀的解析与错误隔离。
package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// ErrModelNotFound 请求的模型不在任何 provider 里。
var ErrModelNotFound = errors.New("router: 找不到该模型")

// ErrInvalidAlias 别名表不合法（见 SetAliases 的校验规则）。
//
// 单独定义成一个哨兵错误：设置层（面板/设置 API）要能把它与
// 「模型不存在」区分开，前者是用户填错了表单，后者是渠道侧的问题。
var ErrInvalidAlias = errors.New("router: 别名表不合法")

// MaxAliasKeyLen 别名 key 的最大长度（按字节）。
//
// 上限理由：别名会出现在 /v1/models 的 id 里、请求体的 model 字段里，
// 以及用量记录里；不设上限时一个超长 key 能让这些地方同时膨胀。
// 64 是够用且远小于任何 HTTP 头上限的值。
const MaxAliasKeyLen = 64

// Router 按模型前缀选择 provider。
type Router struct {
	mu        sync.RWMutex
	providers map[string]provider.Provider // key = 渠道名（前缀）
	models    map[string]routeEntry        // key = 完整模型 ID（含前缀）

	// aliasTargets 别名 → 目标模型 ID（对外的完整 ID，如 workbuddy/space-bunny）。
	//
	// 为什么不复用 models map：models 的 key 是 provider 注册时写入的、
	// 可以整体替换的渠道目录；别名是用户可随时改写的另一层。
	// 混在一起会让 Register（可能被反复调用）和 SetAliases 互相覆盖。
	//
	// 并发：任何读写都在 mu 之下。SetAliases 采用【整体替换 map 指针】而不是
	// 原地增删，这样即使将来有人误在锁外读了旧引用，也不会看到半更新的表。
	aliasTargets map[string]string
}

type routeEntry struct {
	providerID string
	upstreamID string
	model      provider.Model
}

// New 创建一个路由器。
func New() *Router {
	return &Router{
		providers:    make(map[string]provider.Provider),
		models:       make(map[string]routeEntry),
		aliasTargets: make(map[string]string),
	}
}

// Register 注册一个 provider，并拉取它的模型表。
//
// 一个 provider 拉取失败不应影响其他 provider —— 这是「故障隔离」要求。
//
// ⚠️ 本方法是**合并**语义：它只往 `r.models` 里写，**不清理**该 provider
// 的旧条目。⇒ **只适合启动时用一次**。
//
// 🔴 运行期重新拉取目录请用 `Refresh`（它会先删该 provider 的旧条目）——
// 否则上游**下架**的模型会永远留在列表里（"看起来能用、实际报错"）。
func (r *Router) Register(ctx context.Context, p provider.Provider) error {
	models, err := p.Models(ctx)
	if err != nil {
		return fmt.Errorf("注册 provider %q 失败: %w", p.ID(), err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.providers[p.ID()] = p
	for _, m := range models {
		r.models[m.ID] = routeEntry{
			providerID: p.ID(),
			upstreamID: m.UpstreamID,
			model:      m,
		}
	}
	return nil
}

// Refresh 重新拉取某个 provider 的模型表，并**原子替换**它原有的条目。
//
// 🔴 与 Register 的关键区别（2026-10-07 为「刷新模型列表」而加）：
//
//	Register 是**合并**：只写不删 ⇒ 上游**下架**的模型会永远留着。
//	运行期刷新必须用本方法，否则"刷新"反而会让列表只增不减。
//
// 安全约定（**失败不清空**）：
//   - 拉取失败 ⇒ 返回 error，**原有列表原封不动**（不会把好列表清成空）
//   - 拉取成功但返回 **0 个模型** ⇒ 视为失败（同样不动原列表）。
//     理由：上游抖动/改结构时返回空目录，若照单全收会让整个平台
//     的模型消失 —— 那是用户可见的严重回归。
//
// 原子性：删除旧条目与写入新条目在**同一次持锁**内完成，
// 不会出现"清空了但还没写新的"的中间态被读到。
func (r *Router) Refresh(ctx context.Context, p provider.Provider) error {
	models, err := p.Models(ctx)
	if err != nil {
		return fmt.Errorf("刷新 provider %q 失败: %w", p.ID(), err)
	}
	if len(models) == 0 {
		return fmt.Errorf("刷新 provider %q 失败: 上游返回 0 个模型（保留原有列表）", p.ID())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// ① 先删该 provider 的所有旧条目
	//    （🔴 Refresh 与 Register 的本质区别就在这一步 —— Register 只写不删，
	//      下架的模型会永远滞留。证伪实验证明：注释掉这段，
	//      TestRefreshRemovesDelistedModels 立刻红。）
	for id, e := range r.models {
		if e.providerID == p.ID() {
			delete(r.models, id)
		}
	}
	// ② 再写入新条目（同一次持锁内 ⇒ 外部读不到中间态）
	r.providers[p.ID()] = p
	for _, m := range models {
		r.models[m.ID] = routeEntry{
			providerID: p.ID(),
			upstreamID: m.UpstreamID,
			model:      m,
		}
	}
	return nil
}

// Providers 返回已注册的渠道名。
func (r *Router) Providers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for id := range r.providers {
		out = append(out, id)
	}
	return out
}

// CountByProvider 返回某个渠道（**不含别名**）当前的模型数。
//
// 用途（2026-10-07）：面板"重新拉取目录"要**逐平台**报告刷新结果，
// 失败时也要显示"该平台现在还有多少个模型"（即刷新前的原值）。
//
// ⚠️ 不含别名：与 `ModelsWithoutAliases` 口径一致 ——
// 面板看到的是"真实模型"，别名是内部映射（见该方法的说明）。
func (r *Router) CountByProvider(providerID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.models {
		if e.providerID == providerID {
			n++
		}
	}
	return n
}

// Models 返回全部模型（不含路由内部信息）。
//
// ⚠️ 按模型 ID 升序排序：底层是 map，迭代顺序随机。
// 不排序的话，每次 /v1/models 与面板模型列表的顺序都会变 ——
// 用户会以为"列表在乱跳"（委托方实测反馈）。
//
// 别名条目【包含在内】。理由（Codex 指出）：不少客户端会先查 /v1/models
// 再决定是否发起调用；别名不在列表里，客户端会先一步拒绝调用它。
// 别名的 ID 用别名本身，Name 标成「别名 → 真实模型」，能力字段照抄目标模型
// （这样客户端拿到别名的上下文长度/模态能力与真实模型一致，不会误判）。
func (r *Router) Models() []provider.Model {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]provider.Model, 0, len(r.models)+len(r.aliasTargets))
	for _, e := range r.models {
		out = append(out, e.model)
	}
	for alias, target := range r.aliasTargets {
		e, ok := r.models[target]
		if !ok {
			// 目标模型当前不可用（如注册失败/目录变化）。保守做法：
			// 列表里跳过它，而不是伪造一个能力为空的条目。
			// 解析别名时仍会返回明确错误（ErrModelNotFound）。
			continue
		}
		m := e.model
		m.ID = alias
		// Name 里带上【真实模型 ID】而不是只带显示名：
		// 显示名（如 "Space-Bunny"）对上 /v1/models 的 id 是无从查证的，
		// 而面板/用户真正要确认的是"这个别名最终打到哪个模型"。
		// 显示名一并保留，方便肉眼对照。
		m.Name = alias + " → " + e.model.ID + " (" + e.model.Name + ")"
		out = append(out, m)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ModelsWithoutAliases 返回**不含别名**的模型目录。
//
// 🔴 为什么需要它（2026-10-06 委托方要求：「映射的不需要出现在这里」）：
//
//	面板的「模型列表」把别名条目（形如 `dsf → workbuddy/deepseek-v4.1-flash`）
//	也列了出来，委托方认为这不是他要看的"模型"。
//
// 🔴 但**绝不能**因此改 Models()：别名必须留在 /v1/models ——
//
//	不少客户端会先查 /v1/models 再决定是否发起调用，别名不在列表里
//	客户端会先一步拒绝调用它（见 Models() 的注释）。
//
// ⇒ 所以只给**面板专用**的接口用这个方法：过滤依据是
//
//	r.aliasTargets 这份**明确元数据**，不是猜 Name 里的 "→" 字符串
//	（Codex 第 37 轮 C 裁定：不要由前端按字符串猜测）。
func (r *Router) ModelsWithoutAliases() []provider.Model {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]provider.Model, 0, len(r.models))
	for _, e := range r.models {
		out = append(out, e.model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AliasCount 返回当前已配置的别名数量（面板用于给出提示语）。
func (r *Router) AliasCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.aliasTargets)
}

// Resolve 把对外模型 ID 解析为 (provider, 上游模型 ID, 模型信息)。
//
// 支持四种写法：
//
//	"workbuddy/space-bunny"  全名（推荐）
//	"space-bunny"            省略前缀（单渠道时可用）
//	"dsf"                    用户自定义别名（SetAliases 注入，服务端解析）
//	"bunny"                  纯客户端侧映射（服务端不内置，这里解析不到）
//
// 别名解析【只有一层】：命中别名后用其目标直接走下面的常规查找，
// 不会对目标再做一次别名解析。这既排除了循环，也让"别名指向别名"
// 的行为是可预期的（指向别名的目标会被当成"不是有效模型"而在
// SetAliases 阶段就被拒绝）。
func (r *Router) Resolve(modelID string) (provider.Provider, string, provider.Model, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, "", provider.Model{}, fmt.Errorf("%w: 模型名为空", ErrModelNotFound)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// 0) 别名优先。
	//
	// 顺序理由：别名是用户【显式】配置的，全名匹配是渠道目录。
	// 若有别名恰好等于某个真实模型 ID（例如把 "workbuddy/glm-5.3" 别名到
	// 另一个模型），用户的显式意图应当胜过隐式目录 —— 否则别名会被
	// 「悄悄忽略」，面板上显示了却不起作用，属于最难查的一类问题。
	lookupID := modelID
	if target, ok := r.aliasTargets[modelID]; ok {
		lookupID = target
	}

	// 1) 全名精确匹配
	if e, ok := r.models[lookupID]; ok {
		p, ok := r.providers[e.providerID]
		if !ok {
			return nil, "", provider.Model{}, fmt.Errorf("%w: 渠道 %q 未注册", ErrModelNotFound, e.providerID)
		}
		return p, e.upstreamID, e.model, nil
	}

	// 2) 无前缀：尝试在唯一渠道里找
	if !strings.Contains(lookupID, "/") {
		for _, e := range r.models {
			if e.upstreamID == lookupID {
				p, ok := r.providers[e.providerID]
				if !ok {
					continue
				}
				return p, e.upstreamID, e.model, nil
			}
		}
	}

	// 报错时回显【用户原本请求的名字】，而不是别名展开后的目标：
	// 客户端只知道它发的是什么，回显目标会让错误信息对不上请求。
	return nil, "", provider.Model{}, fmt.Errorf("%w: %s", ErrModelNotFound, modelID)
}

// UpstreamModelID 返回去前缀后的上游模型 ID（供请求体使用）。
func (r *Router) UpstreamModelID(modelID string) (string, error) {
	_, up, _, err := r.Resolve(modelID)
	return up, err
}

// ─────────────────────────────────────────────────────────────
// 别名（映射表）
// ─────────────────────────────────────────────────────────────

// SetAliases 原子替换别名表。返回错误表示别名表非法（不落盘、不生效）。
//
// 契约（docs/第9轮-接口契约-冻结.md §4）逐步对应：
//
//  1. key 非空、不含 '/'、长度 ≤ MaxAliasKeyLen（按字节）。
//  2. 目标必须能被 Resolve 解析成功 —— 注意这里【不能】用 r.Resolve 查，
//     因为若当前已有别名 a→b，用 Resolve 校验 "a" 会"成功"，等于允许
//     别名指向别名（二层解析）。校验一律走 findModel（只查真实目录），
//     从而"只有一层"这条规则在【写入时就成立】，不依赖解析时的时序。
//  3. 自映射 a→a 拒绝。
//  4. 循环 a→b, b→a 拒绝。因为规则 2 已保证目标必须是【真实模型】，
//     严格来说循环在新表内构造不出来；但调用方可能传一个历史遗留的
//     自引用表，且将来若放宽规则 2 这里就会成为漏洞，所以显式再查一遍，
//     不把安全性寄托在另一条规则的实现细节上。
//  5. 大小写敏感：全程用 map 精确匹配，不做任何 ToLower。
//
// 原子性：先在一个全新的 map 上完成全部校验，任何一条不通过就【直接返回】，
// 不触碰 r.aliasTargets。整个替换在 mu 内一次完成，因此不存在
// 「校验通过一半、表已被改了一半」的中间态 —— 面板保存失败时旧别名继续可用。
func (r *Router) SetAliases(m map[string]string) error {
	// 先在读锁下做【纯校验】：目标解析要读 models，别名表本身还没动。
	// 校验失败时直接返回，r.aliasTargets 一个字节都没变 —— 这就是原子性。
	if err := r.validateAliases(m); err != nil {
		return err
	}

	// 校验通过才构造新表并整体替换（写锁）。
	next := make(map[string]string, len(m))
	for k, v := range m {
		next[k] = strings.TrimSpace(v)
	}

	r.mu.Lock()
	r.aliasTargets = next
	r.mu.Unlock()
	return nil
}

// validateAliases 校验别名表（不修改任何状态）。
//
// 拆出来的理由：SetAliases 需要在写锁之外先完成全部校验，
// 而 Go 的 RWMutex 不支持锁升级（读锁 → 写锁），必须"校验完再重新加锁"。
//
// 契约（docs/第9轮-接口契约-冻结.md §4）逐条对应：
//
//  1. key 非空、不含 '/'、长度 ≤ MaxAliasKeyLen（按字节）。
//  2. 目标必须能被解析成一个【真实模型】。这里刻意不用 r.Resolve：
//     若当前已有别名 a→b，用 Resolve 校验目标 "a" 会"成功"，
//     等于允许别名指向别名（即二层解析）。改用只查真实目录的
//     findModel，使"只有一层"这条规则在【写入时就成立】，
//     而不是依赖解析时的时序。
//  3. 自映射 a→a 拒绝。
//  4. 循环 a→b, b→a 拒绝。规则 2 已保证目标必须是真实模型，
//     严格说循环在新表里构造不出来；但调用方可能传入历史遗留的
//     自引用表，且将来若放宽规则 2 这里就是漏洞 —— 显式再查一遍，
//     不把安全性寄托在另一条规则的实现细节上。
//  5. 大小写敏感：全程 map 精确匹配，不做任何 ToLower。
func (r *Router) validateAliases(m map[string]string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// findModel 只认真实模型目录，不认别名。
	findModel := func(id string) (routeEntry, bool) {
		if e, ok := r.models[id]; ok {
			return e, true
		}
		// 与 Resolve 的「无前缀」兜底保持一致：目标允许写成不带前缀的
		// 上游 ID（如 space-bunny），只要唯一命中一个真实模型。
		if !strings.Contains(id, "/") {
			for _, e := range r.models {
				if e.upstreamID == id {
					return e, true
				}
			}
		}
		return routeEntry{}, false
	}

	for alias, target := range m {
		if alias == "" {
			return fmt.Errorf("%w: 别名不能为空", ErrInvalidAlias)
		}
		if strings.Contains(alias, "/") {
			return fmt.Errorf("%w: 别名 %q 不能包含 %q（会与渠道前缀混淆）",
				ErrInvalidAlias, alias, "/")
		}
		if len(alias) > MaxAliasKeyLen {
			return fmt.Errorf("%w: 别名 %q 长度 %d 超过上限 %d",
				ErrInvalidAlias, alias, len(alias), MaxAliasKeyLen)
		}

		target = strings.TrimSpace(target)
		if target == "" {
			return fmt.Errorf("%w: 别名 %q 的目标为空", ErrInvalidAlias, alias)
		}
		if target == alias {
			return fmt.Errorf("%w: 别名 %q 不能指向自己（自映射）", ErrInvalidAlias, alias)
		}
		if _, ok := findModel(target); !ok {
			return fmt.Errorf("%w: 别名 %q 指向的模型 %q 不存在",
				ErrInvalidAlias, alias, target)
		}
	}

	// 循环检测放在最后，且要看完【整张新表】才能下结论：
	// a→b 与 b→a 谁先被遍历到是不确定的（map 顺序随机），
	// 边查边判会漏掉一半方向。这里对每条边看它的反向边是否也在表里。
	for alias, target := range m {
		back, ok := m[strings.TrimSpace(target)]
		if !ok {
			continue
		}
		if strings.TrimSpace(back) == alias {
			return fmt.Errorf("%w: 别名 %q 与 %q 构成循环（只允许一层解析）",
				ErrInvalidAlias, alias, target)
		}
	}
	return nil
}

// Aliases 返回当前别名表的副本。
//
// 返回副本而不是内部 map：调用方（面板/设置 API）会把它序列化出去，
// 直接返回内部引用等于把 Router 的并发安全交到调用方手里，
// 任何一次误写都会变成难以复现的 map 并发读写崩溃。
func (r *Router) Aliases() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.aliasTargets))
	for k, v := range r.aliasTargets {
		out[k] = v
	}
	return out
}

// CanonicalModelID 把一个"客户端写过的模型名"归一到对外标准 ID。
//
// 🔴 用途（2026-10-06 委托方反馈）：用量统计里同一个模型被记成三条 ——
//
//	"dsf"                          ← 别名
//	"deepseek-v4.1-flash"          ← 裸 ID（缺前缀）
//	"workbuddy/deepseek-v4.1-flash" ← 标准形态
//
// 归一后都变成 `workbuddy/deepseek-v4.1-flash`，聚合到同一行。
//
// 规则（按优先级）：
//  1. 命中别名表 → 取别名指向的目标（并继续按下面规则补前缀）
//  2. 已带前缀（含 "/"）→ 原样返回
//  3. 其余（裸 ID）→ 补上**唯一渠道**的前缀
//  4. 空串 → 原样返回（调用方自己决定怎么显示"未知模型"）
//
// ⚠️ 本函数【只用于展示口径】，不改变任何请求路由 ——
// 路由仍由 Resolve 负责，两者互不影响。
func (r *Router) CanonicalModelID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}

	r.mu.RLock()
	target, isAlias := r.aliasTargets[id]
	// 收集已注册的渠道名（用于给裸 ID 补前缀）
	var only string
	multi := false
	for name := range r.providers {
		if !multi && only != "" && name != only {
			multi = true
		}
		if only == "" {
			only = name
		}
	}
	// 🔴 同时收集"哪些渠道真的有这个模型"（见下面裸 ID 的处理）
	owners := make([]string, 0, 2)
	for name := range r.providers {
		if _, ok := r.models[name+"/"+id]; ok {
			owners = append(owners, name)
		}
	}
	r.mu.RUnlock()
	sort.Strings(owners)

	if isAlias {
		id = strings.TrimSpace(target)
		if id == "" {
			return "" // 别名指向空：脏数据，交给调用方兜底
		}
		// 别名目标也可能是不带前缀的裸 ID —— 递归一次把它补全。
		// （不再递归第二次：别名指向别名属于配置问题，不该在展示层无限展开。）
		if !strings.Contains(id, "/") {
			if p := r.prefixForBareID(id); p != "" {
				return p + "/" + id
			}
		}
	}

	// 已带前缀就直接用。
	//
	// 不做"是否已知前缀"的校验，也不替换成别的渠道前缀 ——
	// 多平台聚合时，带着别的平台前缀的 ID 不该被本渠道污染。
	if strings.Contains(id, "/") {
		return id
	}

	// ── 裸 ID：补平台前缀 ──
	//
	// 🔴 2026-10-06 修（委托方实测反馈）：
	//
	//	「图4中"deepseek-v4.1-flash"现在应该加上前缀了，因为以前没有
	//	  接入国际版可以这样写，现在接入了国际版应该做好区分」
	//
	//	原实现是"**唯一渠道**才补前缀，多渠道就不补" —— 那在接入国际版
	//	之后**全部失效**：两个渠道 ⇒ 一律不补 ⇒ 老记录永远显示裸 ID。
	//
	//	正确的判据不是"有几个渠道"，而是"**这个模型实际属于哪个渠道**"：
	//	  1. 先按 owners（真实注册表）判断 —— 唯一命中就补它
	//	  2. 命中多个渠道（同名模型两边都有，如 glm-5.3）⇒ **不补**：
	//	     补哪个都是猜，猜错会把统计归到错误的平台
	//	  3. 一个都没命中（模型已下架）⇒ 退回"唯一渠道"规则，
	//	     至少比什么都不做强，且不会碰多渠道歧义
	if len(owners) == 1 {
		return owners[0] + "/" + id
	}
	if len(owners) > 1 {
		// 两平台同名 —— 无法判定，保持原样（宁可显示裸 ID 也不要归错平台）
		return id
	}

	if only == "" || multi {
		return id
	}
	return only + "/" + id
}

// prefixForBareID 为裸 ID 找它所属的渠道前缀（找不到返回空串）。
//
// 判据与 CanonicalModelID 一致：**看真实注册表**，而不是"有几个渠道"。
// 命中多个渠道（同名模型）时返回空串 —— 不猜。
func (r *Router) prefixForBareID(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	found := ""
	for name := range r.providers {
		if _, ok := r.models[name+"/"+id]; !ok {
			continue
		}
		if found != "" && found != name {
			return "" // 多渠道命中 ⇒ 有歧义 ⇒ 不补
		}
		found = name
	}
	return found
}
