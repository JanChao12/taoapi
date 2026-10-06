// checkin_daemon.go：内置每日自动签到。
//
// 委托人的原始要求（面向 GitHub 发布）："要求别人下载我的软件也能有自动签到功能"
// —— 所以签到不能依赖本机计划任务，serve 自己就要能签。
//
// 但 2026-10-05 委托人明确调整了两点：
//  1. **默认关闭**（不是默认开启）。用户要在设置页主动打开。
//  2. **可在设置里开关**，且**关掉要立即停止调度**，不是等下次启动。
//
// 第 9 轮 Codex 评审又改掉一处设计缺陷（已采纳）：
//
//	原先用"启动后固定 15 秒"作为首签触发点。问题是：
//	  - 15 秒是个猜出来的数字，账号多/额度刷新慢时可能撞上装配未完成；
//	  - 一个账号都没有时也会白等 15 秒，然后什么都不做。
//	改为**等待"账号已就绪"信号**：装配完成即触发，且无账号时不发任何请求。
//
// 语义（委托人确认）：启动时调度全部账号，**当天已签的跳过**（本地判断，
// 不发请求）；失败的允许本次重试一次。
package app

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
)

// 自动签到节奏。
const (
	// checkinDaemonInterval 唤醒间隔。30 分钟足够及时（最坏晚 30 分钟签上），
	// 且唤醒本身开销为零（不请求上游，只比较日期字符串）。
	checkinDaemonInterval = 30 * time.Minute

	// checkinReadyTimeout 等待"账号就绪"的上限。
	//
	// 就绪信号正常在装配完成时立刻发出（毫秒级）。这个上限是兜底：
	// 万一信号因故没来，也不能让签到永远不跑 —— 到点就照常开始，
	// 无账号时 runDueCheckins 自身会安全地什么都不做。
	checkinReadyTimeout = 30 * time.Second

	// checkinJitterMax 多账号之间的随机抖动上限。
	//
	// Codex 建议：多账号同时启动会造成请求突刺，加小抖动错开。
	// 只在账号之间插入，不改变整体时机。
	checkinJitterMax = 400 * time.Millisecond
)

// CheckinStatus 是自动签到的运行状态，供面板展示。
//
// Codex 要求面板能看到"已启用/已停用、上次触发时间、结果"，
// 而不是只有一个开关却不知道它到底跑没跑。
type CheckinStatus struct {
	// Enabled 当前是否启用。
	Enabled bool `json:"enabled"`

	// Running 是否正在执行一轮签到。
	Running bool `json:"running"`

	// LastRunAt 上次触发时间（零值表示还没触发过）。
	LastRunAt time.Time `json:"lastRunAt,omitempty"`

	// LastResult 上次结果的简短描述（如 "3 签 1 失败"）。
	LastResult string `json:"lastResult,omitempty"`

	// SkippedNoAccounts 上次因"没有账号"而未执行。
	SkippedNoAccounts bool `json:"skippedNoAccounts"`
}

// checkinDaemon 是可开关的签到守护。
//
// 为什么不是一堆裸 goroutine + channel：开关要在运行期切换（设置页），
// 所以需要一个能"起一个、停一个、且知道自己在不在跑"的对象。
type checkinDaemon struct {
	deps Deps

	mu      sync.Mutex
	stop    chan struct{} // 非 nil 表示正在运行
	ready   chan struct{} // 账号就绪信号（由装配方 close）
	status  CheckinStatus
	stopped bool // 服务正在退出，不要再起新的一轮
}

// readySignal 在账号装配完成后由装配方 close，触发首轮签到。
//
// 单独一个类型是为了让"就绪"这个概念显式化 —— 早期版本用 sleep 15 秒
// 猜时机，那个数字没有任何依据，且无账号时会白发一次请求。
func newCheckinDaemon(deps Deps) (*checkinDaemon, chan struct{}) {
	ready := make(chan struct{})
	d := &checkinDaemon{deps: deps, ready: ready}
	return d, ready
}

// Start 启用自动签到（幂等：已在跑就什么都不做）。
func (d *checkinDaemon) Start() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.stopped || d.stop != nil {
		return // 已在跑，或服务正在退出
	}
	stop := make(chan struct{})
	d.stop = stop
	d.status.Enabled = true

	go d.loop(stop)
}

// Stop 停用自动签到（幂等：没在跑也返回）。
//
// 关掉后**立即停止后续调度** —— 这是委托人明确要求的，
// 不是"改个配置等下次启动"。
func (d *checkinDaemon) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.stop != nil {
		close(d.stop)
		d.stop = nil
	}
	d.status.Enabled = false
}

// Shutdown 服务退出时调用：停止调度并阻止再启动。
func (d *checkinDaemon) Shutdown() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.Stop()
}

// Status 返回状态快照。
func (d *checkinDaemon) Status() CheckinStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// loop 是调度循环。
func (d *checkinDaemon) loop(stop chan struct{}) {
	// 先等"账号就绪"，再跑首轮。
	// 这样无账号时不会白等 15 秒、也不会发无用请求。
	select {
	case <-d.ready:
	case <-time.After(checkinReadyTimeout):
		// 兜底：就绪信号没来也照常开始（无账号时 runDueCheckins 是空操作）
	case <-stop:
		return
	}

	d.runOnce(stop)

	t := time.NewTicker(checkinDaemonInterval)
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

// runOnce 执行一轮签到并更新状态。
func (d *checkinDaemon) runOnce(stop chan struct{}) {
	d.mu.Lock()
	d.status.Running = true
	d.mu.Unlock()

	ok, failed, skipped := runDueCheckins(d.deps, stop)

	d.mu.Lock()
	d.status.Running = false
	d.status.LastRunAt = time.Now()
	d.status.SkippedNoAccounts = skipped
	switch {
	case skipped:
		d.status.LastResult = "未执行（无账号）"
	case failed == 0:
		d.status.LastResult = itoa(ok) + " 个成功"
	default:
		d.status.LastResult = itoa(ok) + " 个成功，" + itoa(failed) + " 个失败"
	}
	d.mu.Unlock()
}

// runDueCheckins 给所有"今天还没签"的未禁用账号签到。
//
// 返回值：(成功数, 失败数, 是否因无账号而跳过)。
//
// serve 与 CLI（checkin --due）共用同一套判断口径（todayCN），
// 保证计划任务和内置守护不会互相重复签。
func runDueCheckins(deps Deps, stop <-chan struct{}) (ok, failed int, skippedNoAccounts bool) {
	if deps.Accounts == nil || deps.WBClient == nil {
		return 0, 0, true
	}
	today := todayCN()

	any := false
	// 🔴 2026-10-07 修数据竞争（Codex 第 52 轮审计特别点名本文件）：
	//
	//	本文件原本**三个写点全部正确走 Mutate**（:250/:258/:266），
	//	但读端仍用 `List()` 的**共享指针**读 ManualDisabled / CheckinDay /
	//	Platform。⇒ 它证明了"**只修写方不够**"：读端拿不到锁，
	//	照样会与别处的写（如刷新额度）形成撕裂读。
	//
	//	现在读端也改为快照。
	snaps := deps.Accounts.ListSnapshots()
	for i := range snaps {
		a := &snaps[i]
		if a.ManualDisabled {
			continue
		}
		// 🔴 国际版没有签到活动（实测 2026-10-06）——
		//	不计入 any、不调用、也不计成功/失败。
		//
		//	⚠️ 不能把它算进 any：否则"只有国际版账号"时
		//	any=true 会走完整流程却什么都不做，
		//	调用方据此判断"有账号可签"，语义就错了。
		if !platformHasCheckin(a) {
			continue
		}
		any = true

		// 今天已签 → 跳过（无网络开销）。
		// 这正是委托人确认的语义："启动时调度全部账号，当天已签的跳过"。
		if a.CheckinDay == today {
			ok++
			continue
		}

		// 关掉开关或服务退出时，立刻停止后续账号的处理
		select {
		case <-stop:
			persistAccounts(deps)
			return ok, failed, false
		default:
		}

		// 账号之间加小抖动，避免请求突刺（Codex 建议）
		if ok+failed > 0 {
			sleepJitter(stop)
		}

		p := providerForAccount(deps, a)
		if p == nil {
			continue // 无上游客户端（异常装配）：跳过而不是 panic
		}

		ctx, cancel := context.WithTimeout(context.Background(), checkinTimeout)
		res, err := p.Checkin(ctx, "")
		cancel()

		switch {
		case err != nil:
			failed++
			deps.logf("[auto-checkin] %s 失败: %v", a.Redact(), err)
			deps.Accounts.Mutate(a.UID, func(x *auth.Account) bool {
				x.LastError = "自动签到失败: " + err.Error()
				x.LastObservedAt = time.Now()
				return true
			})
		case res.AlreadyCheckedIn:
			// 幂等成功，记录签到日
			ok++
			deps.Accounts.Mutate(a.UID, func(x *auth.Account) bool {
				x.CheckinDay = today
				x.CheckinAt = time.Now()
				return true
			})
			deps.logf("[auto-checkin] %s 今日已签（幂等）", a.Redact())
		default:
			ok++
			deps.Accounts.Mutate(a.UID, func(x *auth.Account) bool {
				x.CheckinDay = today
				x.CheckinAt = time.Now()
				x.LastError = ""
				return true
			})
			deps.logf("[auto-checkin] %s 签到成功", a.Redact())
		}
	}

	if !any {
		return 0, 0, true
	}

	// 一轮结束统一落盘一次（而非每账号一次），减少磁盘写入
	persistAccounts(deps)
	return ok, failed, false
}

// sleepJitter 在 [0, checkinJitterMax) 内随机等待，可被 stop 打断。
func sleepJitter(stop <-chan struct{}) {
	select {
	case <-time.After(jitterDuration()):
	case <-stop:
	}
}

// jitterDuration 返回一个 [0, checkinJitterMax) 的随机时长。
//
// 用 math/rand 而非 crypto/rand：这里只是为了避免请求突刺，
// 不涉及安全性，且要快。
func jitterDuration() time.Duration {
	if checkinJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(checkinJitterMax)))
}

// itoa 避免为两个数字引入 strconv 的可读性噪音（保持本文件依赖最小）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
