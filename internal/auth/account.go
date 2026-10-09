// Package auth 管理账号凭据与运行状态。
//
// 设计要点（经 DSH × Codex 第 8 轮确认）：
//   - 凭据（token）与状态（封号/限流/额度）分开存：状态要让面板读，凭据不能
//   - 状态持久化，否则重启就"忘了"某个号被封
//   - 冷却到期后可【惰性】恢复为正常，保留原因供面板显示
//   - 账号从磁盘加载时缺 status 字段 → 按正常处理，不能静默排除
//   - **多平台**（2026-10-06 加）：账号必须标明所属平台，
//     国内/国际的凭据与调度**绝不混用**（见 PlatformCN / PlatformIntl）
package auth

import (
	"sort"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/pool"
)

// 平台标识。与 provider/workbuddy 的 Platform 取值保持一致。
//
// ⚠️ 这里用字符串常量而不是 import provider 包：
// auth 是被 provider 层依赖的底层包，反向依赖会形成 import cycle。
const (
	// PlatformCN 国内版（copilot.tencent.com / www.codebuddy.cn）。
	PlatformCN = "cn"

	// PlatformIntl 国际版（www.workbuddy.ai）。
	PlatformIntl = "intl"
)

// NormalizePlatform 把平台字段归一。
//
// 空值 ⇒ **国内版**。理由：接入国际版之前的所有账号都是国内的，
// 老文件里没有这个字段。归一成国际版会让老账号被发到错误域名。
// 未知值也归一为国内版（同样出于"不改变既有行为"的考虑）。
func NormalizePlatform(p string) string {
	if p == PlatformIntl {
		return PlatformIntl
	}
	return PlatformCN
}

// Platform 返回该账号所属平台（已归一，可直接比较）。
func (a *Account) PlatformOf() string {
	if a == nil {
		return PlatformCN
	}
	return NormalizePlatform(a.Platform)
}

// IsIntl 报告该账号是否属于国际版。
func (a *Account) IsIntl() bool {
	return a.PlatformOf() == PlatformIntl
}

// Account 是一个账号的完整记录。
//
// 🔴 本结构含明文 token，【绝不】直接序列化到非加密文件，
// 也【绝不】写入日志或用量事件（见 Redact / Sanitized）。
type Account struct {
	// UID 账号唯一标识。
	UID string

	// Platform 该账号属于哪个上游平台（"cn" 国内版 / "intl" 国际版）。
	//
	// 🔴 为什么必须显式存（2026-10-06 接入国际版时加）：
	//
	//	两版是**两套独立的账号体系与凭据**，绝不能混用：
	//	  · 国内版 token 发到 www.workbuddy.ai 会被拒（甚至可能触发风控）
	//	  · 国际版 token 发到 copilot.tencent.com 同理
	//	  · 同名模型（glm-5.3）在两版的**倍率与能力都不同**，
	//	    混在一个池子里调度会让统计与费用口径失真
	//
	//	⇒ 调度、签到、refresh、用量归属都必须按平台分组。
	//
	// ⚠️ 老账号（本字段为空）**按国内版处理** —— 它们都是接入国际版之前
	//	导入的国内账号。这也是唯一安全的默认值（空值当国际版会让老账号
	//	被发到错误域名）。
	Platform string `json:"platform,omitempty"`

	// Nickname 显示名（通常是手机号）。
	Nickname string

	// EnterpriseID 企业 ID。
	EnterpriseID string

	// Domain 域。
	Domain string

	// AccessToken 访问令牌。
	AccessToken string

	// RefreshToken 刷新令牌。
	//
	// ⚠️ 只允许用于 refresh 端点，绝不进入 chat 请求。
	RefreshToken string

	// TokenExpiresAt 令牌过期时间（Unix 秒）；未知为 0。
	TokenExpiresAt int64

	// ManualDisabled 人工禁用，永不自动清除。
	ManualDisabled bool

	// Status 当前状态（上游观测 + 人工）。
	Status pool.Status

	// StatusReason 状态原因（面板展示，如"限流冷却中"）。
	StatusReason string

	// StatusUntil 冷却截止时间；零值表示无冷却。
	StatusUntil time.Time

	// LastObservedAt 最近一次观测到该账号状态的时间。
	LastObservedAt time.Time

	// LastError 最近一次错误摘要（已脱敏）。
	LastError string

	// Credit 额度快照。
	Credit CreditSnapshot

	// CheckinAt 最近一次签到时间。
	CheckinAt time.Time

	// CheckinDay 最近一次成功签到的日期（YYYY-MM-DD，UTC+8）。
	//
	// 用于 --due 判断今天是否已签，避免重复请求。
	CheckinDay string

	// Rev 运行时版本号，**每次 Mutate 成功就 +1**，用于结果时序校验
	// （见 Store.MutateIfRev）。
	//
	// 🔴 刻意**不落盘**（无 json tag ⇒ 不会被 diskAcct 编码）：
	//	它是"本进程内的变更序列"，重启后从 0 重新开始即可 ——
	//	持久化它反而会让"重启后的旧请求"被误判为有效。
	Rev uint64 `json:"-"`
}

// CreditSnapshot 是一个账号的额度快照。
type CreditSnapshot struct {
	// Known 是否成功查询过。
	//
	// ⚠️ 必须与 Remaining 分开：Remaining==0 可能是"真用完"或"没查成功"。
	Known bool

	// Remaining 剩余总额度。
	Remaining int64

	// Packages 各额度包明细。
	Packages []PackageSnapshot

	// At 快照时间。
	At time.Time
}

// PackageSnapshot 是单个额度包的快照。
type PackageSnapshot struct {
	Name string

	Remain int64

	// Size 该包的**总量**（周期口径，与 Remain/Used 同源）。
	//
	// 🔴 为什么必须带上它（2026-10-09 委托方实测反馈）：
	//
	//	面板要按「剩余 / 总量」画百分比条。此前这里**只有 Remain**，
	//	前端只能拿"本账号内最大的那个包"当分母（remain/maxRemain）——
	//	那是**相对长度**而不是占比，于是"只有 10 积分但从未使用"的包
	//	也画成 100% 满格，看着像"快用完了/很充足"，完全误导。
	//
	//	委托方原话：「这个积分包显示条的长度应该按百分比显示，
	//	              这个包只有 10 积分但是没使用过所以也是 100% 满长度，
	//	              而不是按积分量显示长度」。
	//
	//	⚠️ 0 表示**上游未下发总量**（老数据/字段缺失），
	//	  此时前端必须退回旧行为或显示"—"，**不得**当成"总量为 0"去算。
	Size int64

	// Used 已用量（= Size - Remain，上游直接给时以它为准）。
	//
	// 留它是因为上游有时只给 Used 而不给 Size（见 provider 的 bill()），
	// 有了两者就能互相校验，也能显示"已用多少"。
	Used int64

	// ExpireAt 到期日 YYYY-MM-DD（UTC+8）；空串表示上游未下发。
	ExpireAt string
}

// Percent 返回剩余百分比（0~100）。
//
// 第二个返回值为 false 表示**无总量数据**（Size<=0）——
// 调用方必须据此显示"—"或退回相对长度，不能拿 0 当分母。
//
// ⚠️ 与 EarliestExpiry 一样只做纯计算，不猜测上游意图。
func (p PackageSnapshot) Percent() (int, bool) {
	if p.Size <= 0 {
		return 0, false
	}
	pct := int(p.Remain * 100 / p.Size)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// EarliestExpiry 返回该账号最早的【有效】到期日；无则空串。
//
// 只考虑额度为正且日期合法的包，与 pool 的规则一致。
func (s CreditSnapshot) EarliestExpiry() string {
	best := ""
	var bestT time.Time
	for _, p := range s.Packages {
		if p.Remain <= 0 || p.ExpireAt == "" {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02", p.ExpireAt, zone)
		if err != nil {
			continue
		}
		if best == "" || t.Before(bestT) {
			best, bestT = p.ExpireAt, t
		}
	}
	return best
}

// ExpiringWithin 返回 horizon 内到期的额度小计（展示用）。
func (s CreditSnapshot) ExpiringWithin(horizon time.Duration, now time.Time) int64 {
	deadline := now.In(zone).Add(horizon)
	var sum int64
	for _, p := range s.Packages {
		if p.Remain <= 0 || p.ExpireAt == "" {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02", p.ExpireAt, zone)
		if err != nil {
			continue
		}
		if !t.After(deadline) {
			sum += p.Remain
		}
	}
	return sum
}

// zone 上游墙钟口径：固定 UTC+8（与 pool 一致）。
var zone = time.FixedZone("UTC+8", 8*60*60)

// EffectiveStatus 返回考虑冷却后的当前状态。
//
// 冷却已过时，"限流/临时故障/未知"可以【惰性】恢复为正常 ——
// 但保留 StatusReason 供面板显示"上次为何异常"。
//
// 注意：banned / disabled / auth_expired / no_credit 不会被时间治愈。
func (a Account) EffectiveStatus(now time.Time) pool.Status {
	st := a.Status.Normalize()
	if a.ManualDisabled {
		return pool.StatusDisabled
	}
	if !a.StatusUntil.IsZero() && now.Before(a.StatusUntil) {
		return st // 冷却中
	}
	switch st {
	case pool.StatusRateLimited, pool.StatusTransient:
		return pool.StatusNormal
	case pool.StatusUnknown:
		// 未知状态冷却过后也恢复 —— 保守不等于永久排斥。
		// 若真是封号，下次请求会再次观测到并重新标记。
		return pool.StatusNormal
	}
	return st
}

// ToPool 把账号转成调度层视图。
func (a Account) ToPool(now time.Time) pool.Account {
	pkgs := make([]pool.Package, 0, len(a.Credit.Packages))
	for _, p := range a.Credit.Packages {
		pkgs = append(pkgs, pool.Package{
			Name:     p.Name,
			Remain:   p.Remain,
			ExpireAt: p.ExpireAt,
		})
	}
	return pool.Account{
		ID:            a.UID,
		Platform:      a.PlatformOf(),
		Nickname:      a.Nickname,
		Credits:       a.Credit.Remaining,
		CreditsKnown:  a.Credit.Known,
		Status:        a.EffectiveStatus(now),
		CooldownUntil: a.StatusUntil,
		Disabled:      a.ManualDisabled,
		Packages:      pkgs,
	}
}

// Redact 返回 UID 的掩码形式，用于日志与面板。
//
// 手机号等昵称不在此列 —— 那是委托方自己的号，面板需要看到。
func (a Account) Redact() string {
	return MaskUID(a.UID)
}

// MaskUID 把 UID 掩码成前 8 位 + 省略号。
func MaskUID(uid string) string {
	if len(uid) <= 8 {
		return "***"
	}
	return uid[:8] + "…"
}

// Store 管理多个账号。
//
// 并发说明：Serve 时会有多个请求 goroutine 读写状态，
// 因此所有读写都过锁。账号数量是【个位数】，锁竞争可忽略。
//
// 锁保护的是 map 结构本身；Account 内字段的并发读写由同一把锁串行化 ——
// 面板操作（enable/disable/refresh/checkin）与换号调度会并发改同一账号，
// 没有这把锁就是数据竞争（面板上线后真实存在，见 api_accounts.go）。
type Store struct {
	mu       sync.Mutex
	accounts map[string]*Account

	// revSeq 是**进程内单调递增**的版本序列（只增不减、不复用）。
	//
	// 🔴 为什么不是"每个 Account 自己从 0 开始数"（Codex 第 53 轮指出的 ABA）：
	//
	//	若版本存在 Account 上、新对象从 0 开始，就会出现 ABA 碰撞：
	//	  ① 异步请求取到快照 + Rev(uid)=5
	//	  ② 该账号被删除
	//	  ③ 重新导入**同一个 UID**（新 Account 对象，Rev 从 0 开始）
	//	  ④ 它又被改 5 次 ⇒ Rev 又等于 5
	//	  ⑤ ① 的旧结果通过条件提交 ⇒ **污染了全新的账号**
	//
	//	⇒ 版本必须来自**仓库级、永不复用**的序列。
	//
	// ⚠️ 刻意**不落盘**：重启后新进程从 0 开始即可 ——
	//	旧进程的 goroutine 不会跨进程回来提交，不存在跨进程 ABA。
	revSeq uint64
}

// NewStore 创建空账号仓库。
func NewStore() *Store {
	return &Store{accounts: make(map[string]*Account)}
}

// nextRevLocked 取下一个版本号（**调用方必须已持锁**）。
func (s *Store) nextRevLocked() uint64 {
	s.revSeq++
	return s.revSeq
}

// Len 返回账号数。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

// Get 按 UID 取账号，返回**共享指针**。
//
// 🔴🔴 **不要用它做多字段读取**（2026-10-07 确认这是真实缺陷）：
//
//	共享指针会让调用方读到"写了一半"的字段组合。已用确定性测试复现：
//	读者拿到 (Status="rate_limited", StatusReason="") 这种生产上
//	不可能出现的组合（`race_demo_test.go`，一次运行 777 例）。
//
//	⇒ **只读请用 `Snapshot` / `ListSnapshots`**（锁内值拷贝，自洽）。
//	⇒ **要写请用 `Mutate` / `MutateIfRev`**（锁内回调）。
//	⇒ 本方法只剩两个合法用途：
//	    · 取 UID 是否存在（只要 ok，不看字段）
//	    · **正在被消灭的**旧代码路径（拿指针就地写）
//
// ⚠️ 那句"修改后调用 Mutate 以正确持锁"是**危险的误导**：
//
//	先改共享指针、再调 Mutate，**补救不了已经发生的无锁写入**
//	（字段已经写脏了）。必须在 Mutate 的**受锁回调内**修改。
func (s *Store) Get(uid string) (*Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[uid]
	return a, ok
}

// UIDs 返回全部账号 UID 的**快照**（按 UID 排序）。
//
// 🔴 用途（2026-10-07 修数据竞争）：并发路径**不应该**先 `List()` 拿
// 共享指针再进 goroutine。正确姿势是：
//
//	for _, uid := range st.UIDs() {                        // 只拿 ID（值）
//	    snap, _ := st.Snapshot(uid)                        // 锁内取值拷贝
//	    rev, _ := st.Rev(uid)                              // 版本基线
//	    go func() { ...; st.MutateIfRev(uid, rev, fn) }()  // 锁内条件提交
//	}
//
// 这样共享指针**从头到尾没有逃逸出锁**。
func (s *Store) UIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.accounts))
	for uid := range s.accounts {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

// Snapshot 返回账号的**值拷贝**（在锁内完成）。
//
// 🔴 为什么必须有它（2026-10-07 修既存数据竞争）：
//
//	`Get` 返回的是**共享指针**，调用方可以随时读到"写了一半"的字段组合。
//	这**不是理论风险**，已用确定性测试复现（`race_demo_test.go` 的
//	`TestTornReadAcrossTwoFields`：读者观测到 (Status="rate_limited",
//	StatusReason="") 这种生产上不可能出现的组合，一次运行 777 例）。
//
//	⇒ **读端一律用本方法**：拷贝在锁内完成，得到的一个自洽的快照，
//	  之后再怎么读都不会被并发写影响。`Get` 只保留给"必须拿指针去写"
//	  的场景，且那种场景正在被逐步消灭（见下面 Get 的说明）。
//
// ⚠️ 这是**浅拷贝**：字符串与值类型字段被复制；`Credit.Packages` 是
//
//	切片，拷贝的是 slice header（底层数组仍共享）。
//	⇒ 调用方**不得**修改返回值的切片内容；只读没问题。
//	  要改请走 Mutate。
func (s *Store) Snapshot(uid string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[uid]
	if !ok {
		return Account{}, false
	}
	return a.deepCopy(), true
}

// deepCopy 返回账号的**深拷贝**：连切片底层数组也复制。
//
// 🔴 为什么不能只做 `*a`（Codex 第 53 轮指出的残留缺口）：
//
//	`Account` 的**值拷贝只复制 slice header**（ptr/len/cap），
//	底层数组仍与 Store 里的那个**共享**。于是：
//	  · 写方 `a.Credit.Packages[:0]` + `append` 是**原地改写底层数组**
//	  · 读方拿到的"快照"会看到被改写后的内容 —— 快照并不自洽
//
//	⇒ 只值拷贝**不足以保证快照隔离**，必须把会被改写的切片复制一份。
//
// ⚠️ 只深拷贝**会被并发改写**的部分（`Credit.Packages`）：
//
//	其余字段都是值类型或 string（string 不可变，共享底层字节是安全的）。
//	按需复制比无脑 reflect 深拷贝更清晰、也更快。
//
// 🔴 字段审计（2026-10-07，Codex 第 53 轮要求"完成字段审计"）：
//
//	逐个核对 `Account` 及其嵌套结构，确认**唯一**的引用类型字段就是
//	`Credit.Packages`（切片）：
//	  · `Account`：全为 string / bool / int64 / uint64 / time.Time /
//	    pool.Status(字符串别名) / 三个嵌套**值**结构体
//	    —— **没有** 指针、map、chan、func、interface 字段
//	  · `CreditSnapshot`：`Known` bool、`Remaining` **int64（不是指针）**、
//	    `Packages` []PackageSnapshot、`At` time.Time
//	  · `PackageSnapshot`：`Name` string、`Remain` int64、`ExpireAt` string
//	    —— 纯值，**元素内部没有可变引用**
//	⇒ 复制 `Packages` 的底层数组即完成隔离；不需要无条件复制其它字段。
//	⚠️ 将来若给这些结构体加指针/map 字段，**必须同步更新本函数**。
func (a *Account) deepCopy() Account {
	cp := *a
	if a.Credit.Packages != nil {
		cp.Credit.Packages = make([]PackageSnapshot, len(a.Credit.Packages))
		copy(cp.Credit.Packages, a.Credit.Packages)
	}
	return cp
}

// ListSnapshots 返回全部账号的**深拷贝**，按 UID 排序。
//
// 供"只读遍历"使用（面板渲染、统计、列出账号）。
// 与 `List` 的区别：**共享指针与共享底层数组都不再逃逸**。
func (s *Store) ListSnapshots() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.deepCopy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// Mutate 持锁修改账号。
//
// 面板/调度对账号字段的写操作【必须】走这里而不是直接改 Get 返回的指针 ——
// 直接改没有锁保护，与并发请求形成数据竞争。
// fn 返回 true 表示有变化需要落盘。
func (s *Store) Mutate(uid string, fn func(*Account) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[uid]
	if !ok {
		return false
	}
	// 🔴 只有**真的发生了变更**才推进版本号（Codex 第 53 轮指出我第一版的错）：
	//
	//	第一版我写成无条件 `a.Rev++`，注释还错写成"每次成功变更"。
	//	后果：**无变化的 Mutate 也会推进版本**，从而无谓地让其它
	//	在途的条件提交（MutateIfRev）失败 —— 例如
	//	clearAccountFailure 在"本来就没有失败痕迹"时会走 return false，
	//	却仍然推进了版本，把一次正常的额度刷新结果丢掉。
	//
	//	现在：把 Rev++ 与"是否变更"绑定。
	//
	//	⚠️ 语义约定（调用方必须遵守）：
	//	  `fn` 返回 true ⇔ **确实改了字段**。
	//	  返回 false ⇔ 什么都没改（本函数不动 Rev，也不该落盘）。
	//	  这条约定是版本机制正确性的前提 —— 见 clearAccountFailure 的实现。
	changed := fn(a)
	if changed {
		a.Rev = s.nextRevLocked()
	}
	return changed
}

// MutateIfRev 只在**当前版本等于 want** 时才执行 fn（compare-and-set）。
//
// 🔴 为什么需要它（2026-10-07，Codex 第 52 轮指出的"结果时序"问题）：
//
//	**光加锁不够**。异步写者（如刷新额度）的典型流程是：
//	  ① 取快照（此刻 Rev=R）
//	  ② 发网络请求（可能耗时数秒）
//	  ③ 把结果写回
//	若在第 ② 步期间，另一个请求把该账号标成了异常（Rev 变成 R+1），
//	第 ③ 步仍会拿**过期的成功结果**把新异常抹掉 —— 这是**语义错误**，
//	互斥锁完全挡不住。
//
//	⇒ 用版本号做条件提交：版本被推进过就**丢弃**这次结果。
//
// 返回：
//
//	ok      账号存在**且**版本匹配**且** fn 返回 true（即确实变更了）
//	stale   版本不匹配（结果已过期，调用方应丢弃）
//
// ⚠️ 这两者必须分开：`stale=true` 不是错误，是"这次结果不该用"。
func (s *Store) MutateIfRev(uid string, want uint64, fn func(*Account) bool) (ok bool, stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, found := s.accounts[uid]
	if !found {
		return false, false
	}
	if a.Rev != want {
		// 期间有人改过 ⇒ 本次结果基于过期前提，丢弃
		return false, true
	}
	// 同 Mutate：只有真的改了才推进版本（避免无谓地作废其它在途提交）
	changed := fn(a)
	if changed {
		a.Rev = s.nextRevLocked()
	}
	return changed, false
}

// Rev 返回某账号的当前版本号（用于异步写者取基线）。
//
// 账号不存在时返回 (0,false)。
func (s *Store) Rev(uid string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[uid]
	if !ok {
		return 0, false
	}
	return a.Rev, true
}

// Put 插入或替换账号。
//
// 🔴 2026-10-07（Codex 第 53 轮要求）：**每次 Put 都给一个全新的版本号**。
//
//	为什么必须：新账号的 `Rev` 零值是 0，而"从没被改过"的旧账号也是 0
//	⇒ **重导入同一个 UID 后版本与旧基线相同** ⇒ ABA 碰撞：
//	  ① 旧请求取基线 Rev=0
//	  ② 账号被删除
//	  ③ 重导入同 UID（新对象，Rev 零值 = 0）
//	  ④ 旧结果回来，`MutateIfRev(uid, 0, ...)` **匹配成功** ⇒ 污染新账号
//
//	⚠️ 这个洞是**我第一版没堵住的**：我当时只在 Mutate 里发放版本，
//	  以为"仓库级序列不复用"就安全了 —— 但**零值不来自序列**，
//	  所以首次 Put 的账号仍可能与旧基线碰撞。
//	  Codex 要求"在重导入后、额外修改前就断言版本不同"，
//	  正是这条更严的断言把它抓出来的。
//
//	⇒ 现在 Put 也给号：新对象一定拿到序列里的新值，不可能等于任何旧基线。
func (s *Store) Put(a *Account) {
	if a == nil || a.UID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 无论新增还是替换，都分配新版本（旧对象上的 Rev 随之失效）
	a.Rev = s.nextRevLocked()
	s.accounts[a.UID] = a
}

// Remove 删除账号。
func (s *Store) Remove(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[uid]; !ok {
		return false
	}
	delete(s.accounts, uid)
	return true
}

// List 返回全部账号的**共享指针**，按 UID 排序保证稳定。
//
// 🔴 **"面板渲染可以接受"这句已被证伪**（2026-10-07）：
//
//	原注释写"单个账号字段仍会随后续写变化 —— 面板渲染可以接受"。
//	实测这**不可接受**：并发写会让读者看到撕裂的字段组合
//	（`race_demo_test.go` 确定性复现）。
//
//	⇒ **只读请用 `ListSnapshots`**。本方法只保留给：
//	    · 需要就地改写的遗留路径（正在被消灭）
//	    · 取 UID 集合
func (s *Store) List() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// PoolAccounts 返回供调度使用的账号视图（**全部平台**）。
//
// 值拷贝 —— 调度拿到的是此刻的快照，不会与并发写互相踩。
//
// ⚠️ 多平台下**不要直接用它做调度** —— 请用 PoolAccountsForPlatform，
// 否则国际账号会被拿去打国内端点（见下面那个函数的说明）。
func (s *Store) PoolAccounts(now time.Time) []pool.Account {
	return s.PoolAccountsForPlatform(now, "")
}

// PoolAccountsForPlatform 返回**指定平台**的可调度账号视图。
//
// 🔴 为什么要按平台过滤（2026-10-06 接入国际版时加）：
//
//	两版是**两套独立的账号体系**，凭据不能跨域名使用：
//	  · 把国内 token 发到 www.workbuddy.ai → 被拒（还可能触发风控）
//	  · 把国际 token 发到 copilot.tencent.com → 同理
//	且同名模型（glm-5.3）在两版的倍率/能力**都不同**，
//	混在一个池子里会让费用统计与调度决策失真。
//
// plat 为空 ⇒ 不过滤（返回全部），供"面板列出所有账号"这类场景使用。
// 调度路径**必须**传具体平台。
func (s *Store) PoolAccountsForPlatform(now time.Time, plat string) []pool.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pool.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		if plat != "" && a.PlatformOf() != NormalizePlatform(plat) {
			continue
		}
		out = append(out, a.ToPool(now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListByPlatform 返回指定平台的账号（面板按平台分组显示用）。
// plat 为空 ⇒ 返回全部。
func (s *Store) ListByPlatform(plat string) []*Account {
	all := s.List()
	if plat == "" {
		return all
	}
	want := NormalizePlatform(plat)
	out := make([]*Account, 0, len(all))
	for _, a := range all {
		if a.PlatformOf() == want {
			out = append(out, a)
		}
	}
	return out
}
