// Package app: 账号换号重试。
//
// 委托人要求：「账号异常了就换用另一个号的额度」。
//
// 🔴 核心约束（Codex 第 8 轮指出）：换号只在【还没有向客户端写出任何字节】时可行。
// 一旦写出 HTTP 头和第一个 SSE 帧，客户端已经收到 200，结果就无法再改。
//
// 因此本文件提供 TryChat：先确认上游真的开始产出（拿到第一个事件），
// 再把它交给调用方去写响应。首个事件到达前的任何失败都可以换号。
package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/provider"
)

// maxAccountAttempts 一次请求最多尝试的账号数。
//
// 取 2（原号 + 1 个备用）的理由：
//   - 一个坏请求不该打穿所有账号，那会白耗每个号的配额
const maxAccountAttempts = 2

// accountChatter 是"能按指定账号发起对话"的能力。
//
// 抽象出来是为了让换号逻辑不依赖具体渠道包（app 不许 import workbuddy）。
type accountChatter interface {
	ChatWithAccount(ctx context.Context, acct *auth.Account,
		req provider.ChatRequest, emit func(provider.Event) error) error
}

// chatSession 是一次已确认可用的对话会话。
//
// 只有在"上游确实产出了第一个事件"之后才会创建它 ——
// 调用方拿到它就可以安全地写 HTTP 头了。
type chatSession struct {
	// Account 实际使用的账号；退化路径（无账号仓库）时为 nil。
	Account *auth.Account

	// FirstEvent 首个上游事件。
	//
	// ⚠️ 调用方【必须】先处理它。它已经被从流里取出，
	// 不处理就会丢掉第一个 token。
	FirstEvent provider.Event

	// Rest 继续消费剩余事件；回调返回错误时应立即停止读取上游。
	Rest func(emit func(provider.Event) error) error

	// Close 释放上游连接。调用方【必须】defer 它。
	Close func()

	// usage 记录流里出现过的最后一个 usage 事件。
	//
	// 为什么由 session 持有（第 9 轮加）：用量记账需要它，而它只出现在
	// 流里最后一个事件（上游的 stream_options.include_usage）。调用方的
	// emit 回调是通用的"转发给客户端"，不该为了记账被迫解析每个事件。
	// 在这里顺手记下，调用方事后取走即可。
	//
	// 用锁：Rest 在读取 goroutine 里跑，调用方在它返回后才读，理论上有
	// happens-before；但加锁成本可忽略，且能防将来有人并发读。
	usageMu sync.Mutex
	usage   *openai.EventUsage
}

// AccountID 返回实际使用账号的短标识（脱敏），无账号时返回空串。
func (s *chatSession) AccountID() string {
	if s == nil || s.Account == nil {
		return ""
	}
	return s.Account.Redact()
}

// noteUsage 记录流里出现的用量事件（由事件转发路径调用）。
func (s *chatSession) noteUsage(u *openai.EventUsage) {
	if s == nil || u == nil {
		return
	}
	s.usageMu.Lock()
	s.usage = u
	s.usageMu.Unlock()
}

// Usage 返回流里最后出现的用量；从未出现时返回 nil。
//
// 🔴 返回 nil 有明确含义：上游没给 usage（例如客户端中途断开）。
// 调用方【不得】把 nil 当成 0 —— 见 usage.Event.UsageKnown 的说明。
func (s *chatSession) Usage() *openai.EventUsage {
	if s == nil {
		return nil
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.usage
}

// TryChat 选出账号并尝试建立对话，首个事件到达前失败会自动换号。
//
// 返回的 chatSession 已确认上游可用，调用方可放心写 HTTP 头。
func TryChat(ctx context.Context, deps Deps, chatter accountChatter,
	req provider.ChatRequest) (*chatSession, error) {

	store := deps.Accounts
	if store == nil || store.Len() == 0 {
		return tryChatWithoutAccounts(ctx, deps, req)
	}

	sel := deps.poolSelector()
	attempted := make(map[string]bool, maxAccountAttempts)
	var lastErr error

	// 🔴 按**模型所属平台**过滤账号（2026-10-06 接入国际版）。
	//
	//	平台从 ctx 取 —— 由 handleChat 在解析模型时判定并注入
	//	（那时才知道用户要的是哪个平台的模型）。
	//
	//	⚠️ 为什么必须过滤：国内 token 发到国际端点会被拒且可能触发风控；
	//	  同名模型（glm-5.3）在两版的倍率/能力也不同。
	//	  过滤放在选号这一步是唯一绕不过去的地方。
	wantPlatform := platformFromContext(ctx)

	for attempt := 1; attempt <= maxAccountAttempts; attempt++ {
		// 每次重新取快照：上一个账号失败可能改了状态
		now := nowFunc()
		picked, err := sel.PickExcludingForPlatform(
			store.PoolAccountsForPlatform(now, wantPlatform), attempted, wantPlatform)
		if err != nil {
			if lastErr != nil {
				break // 有更具体的错误，优先返回
			}
			return nil, fmt.Errorf("没有可用账号: %w", err)
		}
		attempted[picked.ID] = true

		// 🔴 2026-10-07：取**值拷贝快照**而不是共享指针。
		//
		//	原来用 store.Get() 拿共享指针，然后
		//	  · 传给 probeOnce → ChatWithAccount（读凭据）
		//	  · 存进 sess.Account（逃逸到长生命周期结构体）
		//	  · 交给 record/clearAccountFailure 就地写（无锁！）
		//	三个用途里只有第一个是读，后两个都危险。
		//
		//	现在：读用快照（自洽、不逃逸锁），写只传 UID。
		snap, ok := store.Snapshot(picked.ID)
		if !ok {
			lastErr = fmt.Errorf("账号 %s 在调度后消失", auth.MaskUID(picked.ID))
			continue
		}

		// 传快照的地址给 probeOnce：它在整次尝试内只读，不写回仓库。
		acct := &snap
		sess, err := probeOnce(ctx, chatter, acct, req)
		if err == nil {
			// 成功：清掉上次的失败痕迹（只传 UID，锁内改）
			clearAccountFailure(deps, picked.ID)
			if attempt > 1 {
				deps.logf("已换用账号 %s 成功", auth.MaskUID(acct.UID))
			}
			return sess, nil
		}
		lastErr = err

		if !recordAccountFailure(deps, picked.ID, err) {
			// 该错误换号也解决不了（如档位不支持）——立即返回
			return nil, err
		}

		deps.logf("账号 %s 失败（%v），准备换号（已试 %d/%d）",
			auth.MaskUID(acct.UID), err, attempt, maxAccountAttempts)
	}

	if lastErr == nil {
		lastErr = pool.ErrNoAvailable
	}
	return nil, lastErr
}

// probeOnce 用单个账号尝试，直到拿到第一个事件为止。
//
// 时序与内存特性见 probeStream 的说明。
func probeOnce(ctx context.Context, chatter accountChatter,
	acct *auth.Account, req provider.ChatRequest) (*chatSession, error) {

	sess, err := probeStream(ctx, func(ctx context.Context,
		emit func(provider.Event) error) error {
		return chatter.ChatWithAccount(ctx, acct, req, emit)
	})
	if err != nil {
		return nil, err
	}
	sess.Account = acct
	return sess, nil
}

// tryChatWithoutAccounts 是没有账号仓库时的退化路径。
//
// 用于单元测试与"未配置账号"场景，行为等同旧的单 provider 实现。
func tryChatWithoutAccounts(ctx context.Context, deps Deps,
	req provider.ChatRequest) (*chatSession, error) {

	if deps.Router == nil {
		return nil, errors.New("尚未配置任何渠道")
	}
	p, upstreamID, _, err := deps.Router.Resolve(req.Model)
	if err != nil {
		return nil, err
	}
	req.Model = upstreamID

	return probeStream(ctx, func(ctx context.Context,
		emit func(provider.Event) error) error {
		return p.Chat(ctx, req, emit)
	})
}

// probeStream 是 probeOnce 的公共实现，只依赖"怎么跑一条流"这个函数。
//
// 抽出来是因为 probeOnce 与 tryChatWithoutAccounts 的时序完全一致，
// 而这段时序【很容易写错】（漏掉 sent 标志的复位就会丢掉除首个之外的所有事件）。
// 只有一份实现就不会出现"修了一处漏了另一处"。
func probeStream(ctx context.Context,
	run func(context.Context, func(provider.Event) error) error) (*chatSession, error) {

	ctx, cancel := context.WithCancel(ctx)

	first := make(chan provider.Event, 1)
	handoff := make(chan func(provider.Event) error, 1)
	done := make(chan struct{})
	var upstreamErr error

	go func() {
		defer close(done)

		var handedOff bool
		var emitFn func(provider.Event) error

		upstreamErr = run(ctx, func(ev provider.Event) error {
			if !handedOff {
				select {
				case first <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case emitFn = <-handoff:
					handedOff = true
					return nil // ev1 已通过 first 交付
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return emitFn(ev)
		})
	}()

	select {
	case ev := <-first:
		return &chatSession{
			FirstEvent: ev,
			Close:      cancel,
			Rest: func(emit func(provider.Event) error) error {
				select {
				case handoff <- emit:
				case <-done:
					return upstreamErr
				case <-ctx.Done():
					return ctx.Err()
				}
				<-done
				return upstreamErr
			},
		}, nil

	case <-done:
		cancel()
		if upstreamErr != nil {
			return nil, upstreamErr
		}
		return nil, errors.New("上游未返回任何数据就结束了")

	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// ─────────────────────────────────────────────────────────────
// 状态记录
// ─────────────────────────────────────────────────────────────

// recordAccountFailure 记录一次账号失败，返回"是否应该换号重试"。
//
// 🔴 2026-10-07 修既存数据竞争（Codex 第 52 轮指出的高优先级待办）：
//
//	原实现在**无锁**下就地写 `acct.Status / StatusReason / LastError /
//	StatusUntil`。这是**调用频次最高的写点**（每个失败的 chat 请求都命中），
//	与刷新额度（goroutine 池）并发时会互相踩。
//
//	现在整块写入放进 `Mutate` 的**受锁回调**内。
//
// ⚠️ 只传 `uid` 而不是 `*auth.Account`：
//
//	传指针就要求调用方先 `Get()`，那正是"共享指针逃逸"的来源。
//	改成只传 UID，回调里自己查 —— 调用方拿不到指针，也就没法绕开锁。
func recordAccountFailure(deps Deps, uid string, err error) bool {
	// 换号也解决不了的错误：不改账号状态（不是账号的错），直接返回
	if isNonRetryable(err) {
		return false
	}

	status, reason, cooldown := classifyFailure(err)
	now := nowFunc()

	if deps.Accounts != nil {
		deps.Accounts.Mutate(uid, func(a *auth.Account) bool {
			a.Status = status
			a.StatusReason = reason
			a.LastError = reason
			a.LastObservedAt = now
			if cooldown > 0 {
				a.StatusUntil = now.Add(cooldown)
			}
			return true
		})
	}

	persistAccounts(deps)
	return shouldFailover(status)
}

// clearAccountFailure 在一次成功后清掉失败痕迹。
//
// ⚠️ 不清 ManualDisabled —— 那是人工设置，不能被自动逻辑覆盖。
//
// 🔴 2026-10-07 修数据竞争：原来先**无锁读 3 个字段**判断 changed、
//
//	再**无锁写 5 个字段** —— 一个完全无锁的 read-modify-write。
//	现在整个"判断 + 写入"都在同一个受锁回调内完成（原子）。
func clearAccountFailure(deps Deps, uid string) {
	if deps.Accounts == nil {
		return
	}
	now := nowFunc()
	changed := deps.Accounts.Mutate(uid, func(a *auth.Account) bool {
		// 判断与写入在**同一次持锁**内 ⇒ 不会与并发写交错
		if a.Status.Normalize() == pool.StatusNormal &&
			a.StatusReason == "" && a.LastError == "" {
			return false // 无变化，不落盘
		}
		a.Status = pool.StatusNormal
		a.StatusReason = ""
		a.StatusUntil = time.Time{}
		a.LastError = ""
		a.LastObservedAt = now
		return true
	})
	if changed {
		persistAccounts(deps)
	}
}

// persistAccounts 把账号状态落盘；失败只记日志，不阻断请求。
func persistAccounts(deps Deps) {
	if deps.Accounts == nil || deps.Persister == nil {
		return
	}
	if err := deps.Persister.Save(deps.Accounts); err != nil {
		deps.logf("保存账号状态失败: %v", err)
	}
}

// shouldFailover 报告某状态是否应该触发换号。
//
// 【不】换号的是 disabled —— 那是人工禁用，换号逻辑不该碰它。
func shouldFailover(st pool.Status) bool {
	switch st {
	case pool.StatusRateLimited, pool.StatusNoCredit,
		pool.StatusBanned, pool.StatusAuthExpired,
		pool.StatusTransient, pool.StatusUnknown:
		return true
	}
	return false
}

// ErrNonRetryable 表示该错误换号也解决不了，应立即返回给客户端。
//
// 用于把"账号问题"（该换号）与"请求问题"（换号无用）区分开。
// 典型：档位不支持、客户端取消 —— 换个号还是同样的结果，
// 白白消耗另一个账号的配额。
var ErrNonRetryable = errors.New("app: 该错误不可通过换号解决")

// classifyFailure 把错误映射为 (状态, 原因, 冷却时长)。
//
// 规则（证据优先，未知保守）：
//
//	限流              → rate_limited，冷却 60s
//	401/403           → auth_expired（⚠️ 不等于封号）
//	超时              → transient，冷却 30s
//	档位不支持/取消    → 无冷却，且【不换号】（见 ErrNonRetryable）
//	其他未知           → transient，冷却 30s（【不】标 banned）
func classifyFailure(err error) (pool.Status, string, time.Duration) {
	if err == nil {
		return pool.StatusUnknown, "未知错误", 30 * time.Second
	}

	// 明确不可重试的：不是账号问题，换号无用
	if errors.Is(err, provider.ErrUnsupportedReasoning) {
		return pool.StatusNormal, "档位不支持", 0
	}
	if errors.Is(err, context.Canceled) {
		return pool.StatusNormal, "客户端取消", 0
	}

	// provider 层用结构化接口暴露分类，避免 app 依赖具体渠道包
	var cls interface {
		IsAuthFailure() bool
		IsRateLimited() bool
	}
	if errors.As(err, &cls) {
		switch {
		case cls.IsRateLimited():
			return pool.StatusRateLimited, "上游限流", 60 * time.Second
		case cls.IsAuthFailure():
			// ⚠️ 401/403 不等于封号。标 auth_expired 表示"需要重新授权"。
			//
			// 🔴 2026-10-05 更正（Codex 第 33 轮要求）：
			// 这里原先写着「标 auth_expired **以便尝试 refresh**；
			// 只有 refresh 也失败才算真失效」—— **那句话是错的**：
			// 自动续期**从未实现**（`Client.TokenRefreshURL()` 全项目零调用方）。
			//
			// 留着那句注释会误导后来者（包括 AI）以为"已经有续期兜底了"，
			// 从而不去处理真正的失效场景。实测已确认续期端点可用
			// （见 codex-consult/33c-…），但**尚未接入生产**。
			//
			// ⇒ 当前行为：401/403 直接标 auth_expired，需**人工重新授权**。
			return pool.StatusAuthExpired, "凭证失效: " + err.Error(), 0
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return pool.StatusTransient, "超时", 30 * time.Second
	}

	// 兜底：临时故障，短期冷却后自动重试。
	// 【故意】不标 banned —— 一次未知错误不该废掉一个账号。
	return pool.StatusTransient, "上游异常: " + err.Error(), 30 * time.Second
}

// isNonRetryable 报告该错误是否属于"换号也没用"的一类。
//
// 与 classifyFailure 分开判断：状态描述的是"账号怎么了"，
// 而能否重试描述的是"换个号有帮助吗"，两者不完全重合。
func isNonRetryable(err error) bool {
	return errors.Is(err, provider.ErrUnsupportedReasoning) ||
		errors.Is(err, context.Canceled)
}
