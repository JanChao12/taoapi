package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// ═══════════════════════════════════════════════════════════════════════
// 凭证保活（token 续期守护）
// ═══════════════════════════════════════════════════════════════════════
//
// 目标（委托方 2026-10-09 要求）：
//
//	「实现凭证保活功能，让账号不用重复登录。」
//
// 做什么：定期检查各账号的 access token 是否临近过期，到期前用
// refresh token 换新凭据，**原子替换**并落盘。chat 遇鉴权失败时
// 也按需续一次（"定时 + 按需"两种触发，见 refreshOnDemand）。
//
// 🔴 必须如实记住的限制（Codex 第 33 轮给定措辞，不得夸大）：
//
//	「**到期后的续期从未验证**」—— 已实测的续期都是 access token
//	**未过期**时调的，服务端返回的还是同一个 token（未轮换、
//	有效期未延长）。所以本功能的目标是**减少重复登录**，
//	**不能承诺"永不过期"**。
//
//	样本极小（一个账号、两次请求、同一天）：
//	跨天 / 到期 / 闲置超时 / 服务端撤销 **均未验证**。
//
// 🔴 三条实现约束（来自 Codex 第 33 轮，必须守）：
//
//  1. **同账号并发续期去重** —— 定时任务与 chat 按需触发可能同时
//     对同一账号发起续期，而重复提交 refresh token 有触发
//     **token family 撤销**的风险（Codex 第 28 轮警告）。
//     ⇒ 用 per-UID 的 in-flight 标记，同一账号同一时刻只有一个在飞。
//  2. **新凭据原子替换** —— 必须在 `Store.Mutate` 的受锁回调内写入
//     （并发红线第 4 条），然后 Save。
//  3. **不能把所有 401/403 都当过期** —— 403 有专门的分类规则
//     （见 classifyFailure：403 不判封号）。本文件只在
//     failover 已判定为 auth_expired 时才按需续期。
//
// ⚠️ 不依赖"refresh token 不轮换"：收到新值就无条件替换。
//    那只是本次两次请求的观察，将来上游轮换时会立刻失效。

// keepaliveInterval 是定时检查的间隔。
//
// ponytail: 固定 30 分钟轮询，够用且开销可忽略（一次检查只是本地
// 解码 JWT 比较时间，不发网络请求）。升级触发条件：若将来账号数
// 变成几百个、或上游对续期有频率限制，再改成按"最近到期时间"排序的
// 最小堆调度。
const keepaliveInterval = 30 * time.Minute

// refreshHorizon 是"提前多久续期"。
//
// 取 24 小时：access token 实测有效期 40 天左右，提前一天续期
// 既能避开"刚好过期"的边界，也不会频繁打扰上游。
//
// ⚠️ 这只是**调度窗口**，不是"上游保证续期成功"的承诺 ——
// 见文件头的限制说明。
const refreshHorizon = 24 * time.Hour

// keepaliveRefreshTimeout 是单次续期请求的超时。
//
// ⚠️ 与 api_accounts.go 的 refreshTimeout（额度刷新）刻意区分开：
// 两者是不同动作，超时口径未必该一样。
const keepaliveRefreshTimeout = 30 * time.Second

// keepaliveReadyTimeout 是等待"账号就绪"的上限。
const keepaliveReadyTimeout = 15 * time.Second

// keepaliveStatus 是保活守护的运行状态（面板展示用，不含任何凭据）。
type keepaliveStatus struct {
	// Enabled 是否已启动。
	Enabled bool

	// Running 当前是否正在跑一轮。
	Running bool

	// LastRunAt 最近一轮完成时间。
	LastRunAt time.Time

	// LastResult 最近一轮结果的简述。
	LastResult string

	// LastError 最近一次失败原因（已脱敏，不含 token）。
	LastError string
}

// keepaliveDaemon 是凭证保活守护。
type keepaliveDaemon struct {
	deps Deps

	mu      sync.Mutex
	status  keepaliveStatus
	stop    chan struct{}
	stopped bool
	ready   chan struct{}

	// inflight 记录"正在续期的账号"，用于**并发去重**。
	//
	// 🔴 为什么必须有它（Codex 第 33 轮点名）：
	//
	//	定时轮询与 chat 按需触发可能**同时**对同一账号发起续期。
	//	同一个 refresh token 并发提交两次，有触发 token family
	//	撤销的风险 —— 那会让用户**彻底无法登录**，比不续期严重得多。
	//
	//	键是 uid，值是"这轮续期的完成信号"。后到的调用者**等待**
	//	前一个完成，而不是自己再发一个请求。
	inflight map[string]chan struct{}

	// lastRefresh 记录每个账号最近一次续期**尝试**的时间。
	//
	// 用途：当 access token 解不出 exp（非 JWT / 格式变化）时，
	// 用时间节流代替 exp 判断，避免"每次检查都发请求"。
	lastRefresh map[string]time.Time
}

// newKeepaliveDaemon 构造保活守护。
func newKeepaliveDaemon(deps Deps) *keepaliveDaemon {
	return &keepaliveDaemon{
		deps:        deps,
		stop:        make(chan struct{}),
		ready:       make(chan struct{}),
		inflight:    make(map[string]chan struct{}),
		lastRefresh: make(map[string]time.Time),
	}
}

// MarkReady 通知守护"账号已加载完毕"。
//
// 与签到守护同款语义（见 checkin_daemon.go 的 ready）。
func (d *keepaliveDaemon) MarkReady() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.ready:
		// 已经关过，避免重复 close panic。
	default:
		close(d.ready)
	}
}

// Start 启动守护（幂等）。
func (d *keepaliveDaemon) Start() {
	d.mu.Lock()
	if d.stopped || d.status.Enabled {
		d.mu.Unlock()
		return
	}
	d.status.Enabled = true
	stop := d.stop
	d.mu.Unlock()

	go d.loop(stop)
}

// Stop 停止调度（可再次 Start）。
func (d *keepaliveDaemon) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop != nil {
		close(d.stop)
		d.stop = nil
	}
	d.status.Enabled = false
}

// Shutdown 服务退出时调用：停止调度并阻止再启动。
func (d *keepaliveDaemon) Shutdown() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.Stop()
}

// Status 返回状态快照。
func (d *keepaliveDaemon) Status() keepaliveStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// loop 是调度循环。
func (d *keepaliveDaemon) loop(stop chan struct{}) {
	// 先等"账号就绪"，再跑首轮 —— 无账号时不发无用请求。
	select {
	case <-d.ready:
	case <-time.After(keepaliveReadyTimeout):
		// 兜底：就绪信号没来也照常开始（无账号时是空操作）
	case <-stop:
		return
	}

	d.runOnce(stop)

	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.runOnce(stop)
		case <-stop:
			return
		}
	}
}

// runOnce 检查全部账号，给需要的续期。
func (d *keepaliveDaemon) runOnce(stop <-chan struct{}) {
	d.mu.Lock()
	d.status.Running = true
	d.mu.Unlock()

	ok, failed, skipped, lastErr := renewExpiringAccounts(d.deps, d, stop)

	d.mu.Lock()
	d.status.Running = false
	d.status.LastRunAt = time.Now()
	d.status.LastError = lastErr
	switch {
	case skipped:
		d.status.LastResult = "未执行（无账号）"
	case ok == 0 && failed == 0:
		d.status.LastResult = "无需续期"
	case failed == 0:
		d.status.LastResult = itoa(ok) + " 个已续期"
	default:
		d.status.LastResult = itoa(ok) + " 个已续期，" + itoa(failed) + " 个失败"
	}
	d.mu.Unlock()
}

// beginRefresh 尝试为 uid 取得"独家续期权"。
//
// 返回 (isOwner, wait)：
//   - isOwner=true  ⇒ 调用者负责真正发请求，完成后必须调 endRefresh
//   - isOwner=false ⇒ 已有别人在续，调用者应等待 wait 关闭后再读结果
//
// 🔴 这是**并发去重**的实现，不是优化 —— 见 inflight 的说明。
func (d *keepaliveDaemon) beginRefresh(uid string) (bool, chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.inflight[uid]; ok {
		return false, ch
	}
	ch := make(chan struct{})
	d.inflight[uid] = ch
	return true, ch
}

// endRefresh 释放独家续期权并唤醒等待者。
func (d *keepaliveDaemon) endRefresh(uid string) {
	d.mu.Lock()
	ch, ok := d.inflight[uid]
	if ok {
		delete(d.inflight, uid)
	}
	d.mu.Unlock()
	if ok {
		close(ch)
	}
}

// markRefreshed 记录一次续期尝试的时间（供无 exp 时的时间节流）。
func (d *keepaliveDaemon) markRefreshed(uid string, at time.Time) {
	d.mu.Lock()
	d.lastRefresh[uid] = at
	d.mu.Unlock()
}

// lastRefreshAt 读取某账号的最近续期尝试时间。
func (d *keepaliveDaemon) lastRefreshAt(uid string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastRefresh[uid]
}

// renewExpiringAccounts 给所有"到了该续期"的账号续期。
//
// 返回值：(成功数, 失败数, 是否因无账号而跳过, 最近一条错误)。
//
// 🔴 读账号必须用快照（并发红线第 5 条）—— 不用 Get/List 的共享指针。
func renewExpiringAccounts(deps Deps, d *keepaliveDaemon, stop <-chan struct{}) (ok, failed int, skipped bool, lastErr string) {
	if deps.Accounts == nil || deps.WBClient == nil {
		return 0, 0, true, ""
	}

	now := time.Now()
	snaps := deps.Accounts.ListSnapshots()
	if len(snaps) == 0 {
		return 0, 0, true, ""
	}

	any := false
	for i := range snaps {
		select {
		case <-stop:
			return ok, failed, false, lastErr
		default:
		}

		s := snaps[i]

		// 人工禁用的账号不保活：用户明确不要它，就不该替它续期。
		if s.ManualDisabled {
			continue
		}
		// 没有 refresh token 的账号没法续期（例如老的手工导入凭据）。
		if strings.TrimSpace(s.RefreshToken) == "" {
			continue
		}
		if !workbuddy.NeedsRefresh(s.AccessToken, d.lastRefreshAt(s.UID), now, refreshHorizon) {
			continue
		}

		any = true
		if renewOneAccount(deps, d, s.UID, stop) {
			ok++
		} else {
			failed++
			lastErr = "部分账号续期失败（详见日志）"
		}
	}
	if !any {
		return 0, 0, false, ""
	}
	return ok, failed, false, lastErr
}

// renewOneAccount 给单个账号续期（含并发去重与原子落盘）。
//
// 🔴 完整链路（每一步都有理由）：
//
//  1. 并发去重：同一账号同时只有一个续期在飞
//  2. 取快照（**锁内深拷贝**，不是共享指针）
//  3. 发请求（**锁外** —— 网络调用绝不能持锁）
//  4. 条件提交：只在账号没被改过时写入（防"过期结果覆盖新状态"）
//  5. 落盘
func renewOneAccount(deps Deps, d *keepaliveDaemon, uid string, stop <-chan struct{}) bool {
	isOwner, wait := d.beginRefresh(uid)
	if !isOwner {
		// 已有别人在续：等它结束，然后按它的结果读新状态。
		//
		// ⚠️ 这里**不做超时等待的推断** —— 等待者只需知道"那一轮结束了"，
		// 自己不必再发请求（重复提交 refresh token 是本项目明确要防的）。
		select {
		case <-wait:
			return accountLooksRefreshed(deps, uid)
		case <-stop:
			return false
		}
	}
	defer d.endRefresh(uid)

	// ── 取快照（锁内深拷贝）──
	snap, ok := deps.Accounts.Snapshot(uid)
	if !ok {
		// 账号已被删除：不是错误，直接跳过。
		return false
	}
	if strings.TrimSpace(snap.RefreshToken) == "" {
		return false
	}

	// ── 发请求（锁外）──
	ctx, cancel := context.WithTimeout(context.Background(), keepaliveRefreshTimeout)
	defer cancel()

	cred := credentialOf(&snap)
	res, err := deps.WBClient.RefreshToken(ctx, cred)

	// 无论成功失败都记下"这次尝试的时间"，供无 exp 时的时间节流。
	d.markRefreshed(uid, time.Now())

	if err != nil {
		// ⚠️ 错误信息可能带响应体片段；由 UpstreamError 保证不含 token。
		deps.logf("凭证保活：账号 %s 续期失败: %v", auth.MaskUID(uid), err)
		return false
	}

	// ── 条件提交（锁内）──
	//
	// 🔴 必须在 Mutate 的**受锁回调内**改字段（并发红线第 4 条）：
	//	先改共享指针再调 Mutate 补救不了已发生的无锁写入。
	//
	// 用 MutateIfRev 而不是 Mutate：期间账号若被别处改过（如重新登录、
	// 额度刷新），本次结果就是**过期的**，必须丢弃 —— 否则会用旧 token
	// 覆盖刚登录拿到的新凭据（这正是 R1 那类缺陷）。
	committed, stale := deps.Accounts.MutateIfRev(uid, snap.Rev, func(a *auth.Account) bool {
		// 双保险：确认这仍是同一个账号（UID 相同）且没换过 refresh token。
		if a.RefreshToken != snap.RefreshToken {
			return false
		}
		a.AccessToken = res.AccessToken
		// 无条件覆盖（收到新值就存）—— 见文件头"不依赖不轮换"。
		a.RefreshToken = res.RefreshToken
		a.TokenExpiresAt = expiryFromRefresh(res, time.Now())
		a.LastError = ""
		return true
	})

	if !committed {
		reason := "账号已被改动"
		if stale {
			reason = "结果过期（期间账号有更新）"
		}
		deps.logf("凭证保活：账号 %s 续期结果未提交（%s），已丢弃", auth.MaskUID(uid), reason)
		return false
	}

	if err := deps.Persister.Save(deps.Accounts); err != nil {
		// ⚠️ 内存已更新但磁盘失败：不回滚内存（那会让本进程用旧 token），
		// 但要明确告警 —— 重启后会丢掉这次续期。
		deps.logf("凭证保活：账号 %s 续期成功但**落盘失败**（重启后会丢）: %v",
			auth.MaskUID(uid), err)
		return true
	}

	if res.Rotated {
		deps.logf("凭证保活：账号 %s 已续期（refresh token 已轮换）", auth.MaskUID(uid))
	} else {
		deps.logf("凭证保活：账号 %s 已续期", auth.MaskUID(uid))
	}
	return true
}

// accountLooksRefreshed 判断等待者看到的账号是否已被成功续期。
//
// 判据用**确定性事实**：快照里 access token 与"等待前"不同。
// 但等待者并没有"等待前"的基线，所以退而求其次：只要账号当前
// 不在"该续期"状态，就认为这一轮成功了（否则算失败）。
func accountLooksRefreshed(deps Deps, uid string) bool {
	snap, ok := deps.Accounts.Snapshot(uid)
	if !ok {
		return false
	}
	// 续期成功后 access token 通常不再进入 horizon 窗口。
	exp := workbuddy.AccessTokenExpiry(snap.AccessToken)
	if exp == 0 {
		// 解不出 exp：无法判断，保守返回 true（避免把一个其实
		// 成功的续期报成失败，那会让面板显示假故障）。
		return true
	}
	return time.Now().Add(refreshHorizon).Before(time.Unix(exp, 0))
}

// expiryFromRefresh 根据续期响应推算 access token 到期时间。
//
// 优先用 JWT 里的 exp（最准）；解不出时退回 expiresIn 推算。
// 两者都没有则保留原值（不臆造）。
func expiryFromRefresh(res workbuddy.RefreshResult, now time.Time) int64 {
	if exp := workbuddy.AccessTokenExpiry(res.AccessToken); exp > 0 {
		return exp
	}
	if res.ExpiresIn > 0 {
		return now.Add(time.Duration(res.ExpiresIn) * time.Second).Unix()
	}
	return 0
}

// refreshAccountOnDemand 是**按需续期**：chat 链路判定鉴权过期时调用。
//
// 与定时续期的区别：它**无视 horizon**，只要当前 token 已失效就立即试一次。
// 仍然复用同一套去重与条件提交逻辑。
//
// 返回是否**成功换到了可用凭据**（调用方据此决定要不要重试请求）。
//
// ⚠️ 调用方必须已经排除了"流式响应已开始"的情况 ——
//
//	那时代理层已经写出 200 与首帧，重放对话在协议上不可能
//	（见 failover.go 包注释与 TestFailoverAfterStreamStartedIsImpossible）。
func refreshAccountOnDemand(deps Deps, d *keepaliveDaemon, uid string) bool {
	if deps.Accounts == nil || deps.WBClient == nil {
		return false
	}
	snap, ok := deps.Accounts.Snapshot(uid)
	if !ok || strings.TrimSpace(snap.RefreshToken) == "" {
		return false
	}
	return renewOneAccount(deps, d, uid, make(chan struct{}))
}
