// Package pool 决定"这次请求用哪个账号"。
//
// 核心策略（委托人明确要求，第 8 轮修正）：
//
//	只看【每个账号下最早到期的那个包】—— 谁最早到期，谁优先。
//
// ⚠️ 这里【故意不采用】"未来 N 天内的临期额度总量"这类中间量
// （参考项目 wild-work 的做法）。委托人原话：
//
//	"他是设置几天内的临期额度量排序，我是要求只看各个账号下哪个包过期就优先哪个账号"
//
// 因此本包没有参与调度的 Horizon 概念。面板上的"未来 7 天到期"只是展示统计。
//
// 排序规则（严格按此顺序）：
//  1. 有效的最早到期日升序（越早越优先）；无有效到期日的排最后
//  2. 同到期日：该日期的包剩余量之和降序
//  3. 账号总剩余额度降序
//  4. ID 升序（保证稳定）
//
// 排除：人工禁用 / 冷却中 / 额度未知 / 额度为零。
package pool

import (
	"errors"
	"sort"
	"time"
)

// ErrNoAvailable 没有可用账号。
var ErrNoAvailable = errors.New("pool: 没有可用账号")

// Status 是账号的调度状态。
type Status string

const (
	// StatusNormal 正常，可调度。
	StatusNormal Status = "normal"

	// StatusBanned 明确封禁（必须有上游业务证据）。
	//
	// ⚠️ 绝不能因为一次未知的 403 就永久标记为 banned。
	StatusBanned Status = "banned"

	// StatusRateLimited 限流，带冷却截止时间，到期自动恢复。
	StatusRateLimited Status = "rate_limited"

	// StatusNoCredit 额度耗尽。
	StatusNoCredit Status = "no_credit"

	// StatusAuthExpired 凭证失效，需要**人工重新授权**。
	//
	// 🔴 2026-10-05 更正（Codex 第 33 轮要求）：原先写的是
	// "凭证失效且 refresh 也失败" —— **那是错的**，因为
	// **自动续期从未实现**（`Client.TokenRefreshURL()` 全项目零调用方）。
	//
	// 留着原措辞会让人以为"已经有续期兜底、这里只是兜底也失败了"，
	// 从而低估影响。实测已确认续期端点可用，但**尚未接入生产**。
	StatusAuthExpired Status = "auth_expired"

	// StatusDisabled 人工禁用，永不自动清除。
	StatusDisabled Status = "disabled"

	// StatusTransient 临时故障（5xx / 网络错误 / 超时），短期隔离。
	StatusTransient Status = "transient"

	// StatusUnknown 有异常但证据不足，无法定性。
	//
	// 保守取此值而不是猜成 banned —— 一次未知 403 不该永久废掉一个账号。
	StatusUnknown Status = "unknown"
)

// Schedulable 报告该状态是否"时间能解决"（还需检查冷却时间）。
//
// ⚠️ 空串（Status 的零值）按【正常】处理。
// 理由：账号从磁盘加载时若缺 status 字段，零值就是 ""。
// 若把零值判为不可调度，则"没写 status 的账号"会全部被静默排除 ——
// 这种失败很隐蔽（服务能启动，但一个号都不用）。
func (s Status) Schedulable() bool {
	switch s {
	case "", StatusNormal, StatusRateLimited, StatusTransient, StatusUnknown:
		return true
	}
	// banned / disabled / auth_expired / no_credit：需人工或外部条件恢复
	return false
}

// Normalize 把零值归一为 StatusNormal，便于持久化与展示。
func (s Status) Normalize() Status {
	if s == "" {
		return StatusNormal
	}
	return s
}

// Label 返回状态的中文显示名（面板用）。
func (s Status) Label() string {
	switch s {
	case StatusNormal:
		return "正常"
	case StatusBanned:
		return "封号"
	case StatusRateLimited:
		return "限流"
	case StatusNoCredit:
		return "额度耗尽"
	case StatusAuthExpired:
		return "凭证失效"
	case StatusDisabled:
		return "已禁用"
	case StatusTransient:
		return "临时故障"
	case StatusUnknown:
		return "状态未知"
	}
	return "正常"
}

// Account 是一个账号的调度状态。
type Account struct {
	// ID 账号标识（uid）。
	ID string

	// Platform 所属平台（"cn" 国内版 / "intl" 国际版）。
	//
	// 🔴 调度必须按平台分组：两版是**独立的账号体系**，
	// 凭据不能跨域名使用，同名模型的能力/倍率也不同。
	// 混在一个池子里会把请求发到错误的上游。
	Platform string

	// Nickname 显示名（可能是手机号）。
	Nickname string

	// Credits 剩余总额度。
	Credits int64

	// CreditsKnown 额度是否已成功查询过。
	//
	// ⚠️ 必须与 Credits 分开：Credits==0 既可能是"真的用完了"，
	// 也可能是"从来没查成功过"。前者该跳过，后者不该当成没额度。
	CreditsKnown bool

	// Status 账号状态。
	Status Status

	// CooldownUntil 冷却截止时间；零值表示无冷却。
	//
	// 语义：在此时间之前不参与调度。StatusRateLimited / StatusTransient
	// 靠它自动恢复。
	CooldownUntil time.Time

	// Disabled 人工禁用。
	//
	// 与 Status 的自动状态分离，避免自动逻辑清掉人工设置。
	Disabled bool

	// ModelCooldowns 按模型的冷却截止时间（模型 ID → 截止时刻）。
	//
	// ═══════════════════════════════════════════════════════════════════
	// 🔴 为什么需要它（2026-10-09 委托方实测要求）
	// ═══════════════════════════════════════════════════════════════════
	//
	// 上游的 429 是**按模型**限流的。委托方原话：
	//
	//	「上游的限流我实测之前使用DeepSeek过多导致限制，
	//	  我切换glm后能正常使用」
	//
	// 这与上游 429 响应体里那句「您也可以切换其他模型继续使用」完全吻合。
	//
	// ⇒ 若只用账号级 CooldownUntil，一个模型被限流会把该账号上
	//   **其他仍然可用的模型**一起冻住 —— 恰好丢掉上游给的退路。
	//   所以限流冷却必须按 (账号, 模型) 记录。
	//
	// ⚠️ 与 CooldownUntil 的分工：
	//	· CooldownUntil        —— **账号级**问题（超时/未知故障/凭证等）
	//	· ModelCooldowns[模型] —— **模型级**限流
	//	两者任一未过期，该账号在对应场景下都不可调度。
	//
	// ⚠️ 为空 map 与 nil 等价（都表示"没有任何模型在冷却"），
	//	不要用 len()==0 判断"该账号被限流"。
	ModelCooldowns map[string]time.Time

	// Packages 各额度包明细，用于算最早到期日。
	Packages []Package
}

// Package 是一个额度包。
type Package struct {
	Name string

	// Remain 剩余额度。<=0 的包不参与到期日计算。
	Remain int64

	// ExpireAt 到期日 YYYY-MM-DD（UTC+8 墙钟）。
	//
	// 空串表示上游未下发 —— 不参与"谁先过期"的比较，
	// 【不得】用零值时间冒充"永不过期"。
	ExpireAt string

	// ExpiredButReported 本地日期看已过期，但上游仍返回它且额度为正。
	//
	// 此时【仍然算数】：上游快照优先，本地日期只负责告警。
	ExpiredButReported bool
}

// zone 上游墙钟口径：固定 UTC+8。
//
// ⚠️ 不能用 time.Local —— 上游时间串是北京时间，换时区会算错一天。
var zone = time.FixedZone("UTC+8", 8*60*60)

// parseExpire 解析到期日；空串或非法格式返回 false。
func parseExpire(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, zone)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// usable 报告该包是否参与到期日计算。
//
// 条件：额度为正【且】到期日合法。
// 零/负额度的包没有意义（用不掉），非法日期无法比较。
func (p Package) usable() (time.Time, bool) {
	if p.Remain <= 0 {
		return time.Time{}, false
	}
	return parseExpire(p.ExpireAt)
}

// EarliestExpiry 返回该账号最早的【有效】到期日（YYYY-MM-DD）；无则空串。
//
// 只考虑额度为正且日期合法的包 —— 否则一个 0 额度的旧包会把整个账号
// 排到最前面，白白浪费一次调度。
func (a Account) EarliestExpiry() string {
	best := ""
	var bestT time.Time
	for _, p := range a.Packages {
		t, ok := p.usable()
		if !ok {
			continue
		}
		if best == "" || t.Before(bestT) {
			best, bestT = p.ExpireAt, t
		}
	}
	return best
}

// RemainOn 返回该账号在指定到期日上的剩余额度之和。
//
// 用于同到期日时的 tie-break。⚠️ 只统计【恰好等于该日期】的包，
// 不能把其他日期的额度也算进来（那正是被否定的"总量排序"）。
func (a Account) RemainOn(expireAt string) int64 {
	if expireAt == "" {
		return 0
	}
	var sum int64
	for _, p := range a.Packages {
		if p.ExpireAt != expireAt || p.Remain <= 0 {
			continue
		}
		sum += p.Remain
	}
	return sum
}

// ExpiringWithin 返回在 horizon 内到期的额度小计。
//
// ⚠️ 这是【展示统计】，供面板显示"未来 N 天要过期多少"。
// 【绝不参与】账号选择 —— 见包注释。
func (a Account) ExpiringWithin(horizon time.Duration, now time.Time) int64 {
	deadline := now.In(zone).Add(horizon)
	var sum int64
	for _, p := range a.Packages {
		t, ok := p.usable()
		if !ok {
			continue
		}
		if !t.After(deadline) {
			sum += p.Remain
		}
	}
	return sum
}

// DisplayHorizon 面板展示用的"临期"窗口：7 天。与调度无关。
const DisplayHorizon = 7 * 24 * time.Hour

// Selector 按策略选择账号。
type Selector struct {
	// now 便于测试注入时间。
	now func() time.Time
}

// NewSelector 创建选择器。
func NewSelector() *Selector {
	return &Selector{now: time.Now}
}

// SetClock 注入时间源（测试用）。
func (s *Selector) SetClock(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

// ModelCooling 报告该账号在**指定模型**上是否仍在冷却。
//
// model 为空 ⇒ 只判账号级冷却，忽略模型级（用于"不关心具体模型"的调用方，
// 如面板展示）。
func (a Account) ModelCooling(model string, now time.Time) bool {
	if model == "" || len(a.ModelCooldowns) == 0 {
		return false
	}
	until, ok := a.ModelCooldowns[model]
	return ok && until.After(now)
}

// Scheduled 报告该账号当前是否可参与调度。
//
// ⚠️ 这是"账号级"判定（不看具体模型）。带模型的调度请用
// ScheduledForModel —— 直接用这个会把"仅某模型被限流"的账号
// 误判成完全不可用。
func (s *Selector) Scheduled(a Account, now time.Time) bool {
	return s.ScheduledForModel(a, "", now)
}

// ScheduledForModel 报告该账号在**指定模型**上是否可参与调度。
//
// model 为空 ⇒ 忽略模型级冷却（退回账号级判定）。
func (s *Selector) ScheduledForModel(a Account, model string, now time.Time) bool {
	if a.Disabled || a.Status.Normalize() == StatusDisabled {
		return false
	}
	if !a.Status.Schedulable() {
		return false
	}
	// 账号级冷却
	if !a.CooldownUntil.IsZero() && a.CooldownUntil.After(now) {
		return false
	}
	// 模型级冷却（2026-10-09 加账号+模型限流）
	if a.ModelCooling(model, now) {
		return false
	}
	// 额度：必须"已知"且为正
	if !a.CreditsKnown {
		return false
	}
	return a.Credits > 0
}

// Pick 选出一个可用账号。
//
// 排序见包注释。返回 ErrNoAvailable 表示当前无号可用。
//
// ⚠️ 多平台下**不要直接用它** —— 请用 PickForPlatform。
// 直接用它会把国际账号选给国内请求（反之亦然）。
func (s *Selector) Pick(accounts []Account) (Account, error) {
	return s.PickForPlatform(accounts, "")
}

// PickForPlatform 在**指定平台**的账号里选一个。
//
// plat 为空 ⇒ 不过滤（兼容既有调用与单平台场景）。
//
// 🔴 为什么过滤放在调度层而不是调用方：
//
//	调用方（TryChat / 面板）拿到的账号列表可能含多平台，
//	只要有一处忘记过滤，请求就会被发到**错误的上游域名** ——
//	那是凭据跨站泄露级别的问题，不能靠"每个调用方都记得"来保证。
//	放在选号这一步，是唯一无法绕过的地方。
func (s *Selector) PickForPlatform(accounts []Account, plat string) (Account, error) {
	return s.PickForModel(accounts, plat, "")
}

// PickForModel 在**指定平台 + 指定模型**下选一个账号。
//
// 🔴 为什么必须带 model（2026-10-09 账号+模型限流）：
//
//	上游限流是**按模型**的（委托方实测：DeepSeek 被限后切 glm 可用）。
//	只按平台筛会把"仅该模型被限流"的账号也选出来，于是每次请求都
//	撞一次 429 再换号 —— 白白消耗上游请求，面板还会闪"限流"。
//
// model 为空 ⇒ 忽略模型级冷却（等价于旧的按平台行为）。
func (s *Selector) PickForModel(accounts []Account, plat, model string) (Account, error) {
	now := s.now()

	avail := make([]candidate, 0, len(accounts))
	for _, a := range accounts {
		if plat != "" && a.Platform != plat {
			continue
		}
		if !s.ScheduledForModel(a, model, now) {
			continue
		}
		earliest := a.EarliestExpiry()
		avail = append(avail, candidate{
			acct:     a,
			earliest: earliest,
			onDate:   a.RemainOn(earliest),
		})
	}
	if len(avail) == 0 {
		return Account{}, ErrNoAvailable
	}

	sort.SliceStable(avail, func(i, j int) bool {
		x, y := avail[i], avail[j]

		// 1) 有有效到期日的排前面，且越早越前
		if (x.earliest == "") != (y.earliest == "") {
			return y.earliest == ""
		}
		if x.earliest != y.earliest {
			return x.earliest < y.earliest
		}

		// 2) 同到期日（或都无到期日）：该日额度多的优先
		if x.onDate != y.onDate {
			return x.onDate > y.onDate
		}

		// 3) 账号总剩余额度多的优先
		if x.acct.Credits != y.acct.Credits {
			return x.acct.Credits > y.acct.Credits
		}

		// 4) 稳定：按 ID
		return x.acct.ID < y.acct.ID
	})

	return avail[0].acct, nil
}

// PickExcluding 与 Pick 相同，但跳过 exclude 里的账号 ID。
//
// 用于换号重试：避免重新选回刚失败的那个号。
func (s *Selector) PickExcluding(accounts []Account, exclude map[string]bool) (Account, error) {
	return s.PickExcludingForPlatform(accounts, exclude, "")
}

// PickExcludingForPlatform 同上，但**同时限定平台**。
//
// 🔴 换号重试最容易出跨平台问题：某平台账号全失败后，
//
//	若不限定平台，重试就会"借"另一个平台的账号 ——
//	那把凭据发到了错误的上游域名。所以平台过滤必须与 exclude 一起生效。
func (s *Selector) PickExcludingForPlatform(accounts []Account,
	exclude map[string]bool, plat string) (Account, error) {

	return s.PickExcludingForModel(accounts, exclude, plat, "")
}

// PickExcludingForModel 同上，但**同时限定平台与模型**。
//
// 换号重试时也必须带模型：否则会重新选中"该模型正在冷却"的账号，
// 而那正是刚刚失败的那个原因。
func (s *Selector) PickExcludingForModel(accounts []Account,
	exclude map[string]bool, plat, model string) (Account, error) {

	if len(exclude) == 0 {
		return s.PickForModel(accounts, plat, model)
	}
	filtered := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		if exclude[a.ID] {
			continue
		}
		filtered = append(filtered, a)
	}
	return s.PickForModel(filtered, plat, model)
}

type candidate struct {
	acct     Account
	earliest string
	onDate   int64
}
