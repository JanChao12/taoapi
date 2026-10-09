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

// rateLimitCooldown 429「上游限流」的冷却时长。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么是**写死的 10 分钟**，而不是解析上游给的重置时间
//
//	（2026-10-09 委托方拍板，理由如下，不要再"优化"回去）
//
// ═══════════════════════════════════════════════════════════════════
//
// 上游 429 的响应体里确实带着一个重置时间，实测长这样（两处措辞并存）：
//
//	{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-09 21:24:55
//	 UTC+8 重置，您也可以切换其他模型继续使用。"}
//	{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your
//	 usage will reset at 2026-10-10 00:08:52 UTC+8, alternatively, you can
//	 switch to the other models to continue using it."}
//
// 委托方原话（2026-10-09）：
//
//	「我建议先改成写死10分钟，**因为他确实是按模型限制的，
//	  切换其他模型可以继续用**」
//
// ⇒ **不能**照抄那个重置时间：它只对**被限流的那一个模型**成立
//
//	（实测窗口 9~17 小时，且随请求变化）。而本项目的冷却状态是
//	**账号级**的（pool.Account.CooldownUntil）—— 照抄 9 小时等于把
//	这个账号上**其他仍然可用的模型**一起冻住 9 小时，
//	恰恰丢掉了上游明确提示的"切换其他模型继续用"这条退路。
//
// 所以取一个**短而固定**的值：足够让反复重试停下来（实测 60 秒会让
// 同一个已限流账号一天被打 100+ 次），又不会长时间浪费其他模型。
//
// ⚠️ 已知的**有意简化**：冷却仍是**账号级**而非"账号+模型"级，
//
//	即这 10 分钟内该账号的其它模型也选不中。要做成 per-model 需要
//	给 pool.Account 加一张状态表（新状态、新持久化、新调度判定），
//	而当前证据只支持"上游这么说"，没有实测到"限流后换模型确实可用"。
//	// ponytail: 账号级冷却，出现"某账号仅个别模型被限流且其余模型
//	// 仍被调度失败"的实测证据时，再升级为 per-(账号,模型) 冷却。
const rateLimitCooldown = 10 * time.Minute

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

	// alreadyRefreshed 记录本轮**已经尝试过续期**的账号。
	//
	// 🔴 为什么必须防重（2026-10-09）：
	//
	//	续期失败时账号仍是 auth_expired，若没有这个标记，
	//	下一轮换号逻辑可能又选中同一个账号（例如只有它可用），
	//	于是**反复提交同一个 refresh token** —— 而 Codex 第 28 轮警告过，
	//	那可能触发整个 token family 被撤销，让用户**彻底无法登录**。
	//	⇒ 宁可这一次请求失败，也不能把用户的凭据搞废。
	alreadyRefreshed := make(map[string]bool, maxAccountAttempts)

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
		// 🔴 按**模型**选号（2026-10-09 加账号+模型限流）。
		//
		//	req.Model 是上游模型 ID（chat.go 已把客户端名改写成上游 ID）。
		//	带它才能跳过"仅这个模型被限流"的账号 —— 否则每次请求都要
		//	先撞一次 429 再换号，白耗上游请求。
		picked, err := sel.PickExcludingForModel(
			store.PoolAccountsForPlatform(now, wantPlatform), attempted, wantPlatform, req.Model)
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

		// 🔴 R1（2026-10-07 修）：**在发请求之前**取版本基线。
		//
		//	snap 是快照，其 Rev 正是"取快照那一刻"的仓库版本。
		//	下面所有回写都用 `MutateIfRev(uid, baseRev, …)` ——
		//	期间账号被改过/删除后重导入，版本就对不上，过期结果被丢弃。
		//
		//	🔴 **绝不能**在响应回来后再读 Rev 当基线：
		//	   那等于把"现在的版本"冒充成"发请求时的版本"，
		//	   无论期间发生过什么都会匹配成功 —— 保护等于没有。
		baseRev := snap.Rev

		sess, err := probeOnce(ctx, chatter, acct, req)
		if err == nil {
			// 成功：清掉上次的失败痕迹（条件提交，见 R1 说明）
			clearAccountFailure(deps, picked.ID, baseRev, req.Model)
			if attempt > 1 {
				deps.logf("已换用账号 %s 成功", auth.MaskUID(acct.UID))
			}
			return sess, nil
		}

		// ── 按需续期（2026-10-09 凭证保活）──
		//
		// 🔴 只有在**判定为鉴权过期**时才试，且**每个请求对每个账号只试一次**。
		//
		//	不能把所有 401/403 都当"token 过期"（Codex 第 33 轮明确要求）：
		//	403 可能是权限/风控问题，续期解决不了，反而白发一个请求
		//	（而重复提交 refresh token 有触发 token family 撤销的风险）。
		//	所以这里**复用 classifyFailure 的判定结果**，不自作一套。
		//
		// ⚠️ 这里**不需要**检查"流式是否已开始"：probeOnce 只在
		//	**第一个事件到达前**失败才返回 error；一旦拿到首帧，
		//	调用方已经写出了 200，根本走不到这条路径
		//	（见 failover.go 包注释与 TestFailoverAfterStreamStartedIsImpossible）。
		if alreadyRefreshed[picked.ID] {
			// 这个账号本轮已经续过期了，不再重复（防死循环）
		} else if status, _, _ := classifyFailure(err); status == pool.StatusAuthExpired &&
			refreshAccountOnDemand(deps, deps.Keepalive, picked.ID) {
			alreadyRefreshed[picked.ID] = true
			deps.logf("账号 %s 的凭据已自动续期，重试本次请求", auth.MaskUID(picked.ID))

			// 续期成功后用**新凭据**再试一次同一个账号。
			//
			// ⚠️ 必须重新取快照：续期是在锁内写进去的，
			//	原来的 snap 里的 token 已经旧了。
			if snap2, ok := store.Snapshot(picked.ID); ok {
				acct2 := &snap2
				if sess2, err2 := probeOnce(ctx, chatter, acct2, req); err2 == nil {
					clearAccountFailure(deps, picked.ID, snap2.Rev, req.Model)
					deps.logf("账号 %s 续期后重试成功", auth.MaskUID(picked.ID))
					return sess2, nil
				} else {
					err = err2
				}
			}
		}

		lastErr = err

		if !recordAccountFailure(deps, picked.ID, baseRev, req.Model, err) {
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
//
// 🔴 R1（2026-10-07 修）：`baseRev` 是**发请求之前**取的版本基线，
// 回写走 `MutateIfRev` 条件提交。原来的 `Mutate` 是无条件的，会有两个洞：
//
//	① **同 UID 重导入**：请求在途时账号被删除、又导入了同一个 UID
//	   （新对象、新版本），旧的失败记录会写到**全新的账号**上。
//	② **覆盖更新的状态**：请求在途时另一个来源（如额度刷新）
//	   已经把账号标成了新状态，旧的失败记录仍会覆盖它。
//
//	返回"是否应该换号重试"的语义**不因版本不匹配而改变**：
//	换号决策依赖的是**本次错误本身**，与"回写是否被接受"无关 ——
//	否则一次版本抖动会让本该换号的请求直接失败。
//
// 🔴 model（2026-10-09 加账号+模型限流）：本次请求用的**上游模型 ID**。
//
//	上游限流是按模型的（委托方实测：DeepSeek 被限后切 glm 可用），
//	所以限流必须记到"这个模型"名下；只记账号级会把该账号上其他
//	仍然可用的模型一起冻住。
func recordAccountFailure(deps Deps, uid string, baseRev uint64, model string, err error) bool {
	// 换号也解决不了的错误：不改账号状态（不是账号的错），直接返回
	if isNonRetryable(err) {
		return false
	}

	status, reason, cooldown := classifyFailure(err)
	now := nowFunc()

	if deps.Accounts != nil {
		deps.Accounts.MutateIfRev(uid, baseRev, func(a *auth.Account) bool {
			// ── 限流：按**模型**记冷却，不冻整个账号 ──
			//
			// 🔴 这一段必须放在"写账号级字段"之前并提前 return：
			//	限流不是**账号**的问题，而是"这个账号的这个模型"的问题。
			//	把账号级 Status 也写成 rate_limited 会让面板显示"限流"，
			//	而实际该账号对其他模型完全正常 —— 那正是本改动要消除的误导。
			if status == pool.StatusRateLimited && model != "" {
				until := now.Add(cooldown) // 默认 10 分钟（见 rateLimitCooldown）
				// 优先用上游自述的重置时刻（受限时才有）
				if resetAt, ok := rateLimitResetOf(err); ok {
					until = clampRateLimitReset(resetAt, now)
				}
				if a.ModelCooldowns == nil {
					a.ModelCooldowns = make(map[string]time.Time, 1)
				}
				a.ModelCooldowns[model] = until

				// 账号**保持正常**：它只是"这个模型"暂时受限。
				a.Status = pool.StatusNormal
				// 冷却时间留空 —— 账号本身没有冷却。
				a.StatusUntil = time.Time{}
				// 但**要把事实说清楚**（面板要显示"哪个模型被限到几点"）。
				a.StatusReason = "模型限流: " + model
				a.LastError = a.StatusReason
				a.LastObservedAt = now
				return true
			}

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

// rateLimitResetOf 取出上游自述的重置时刻（可选接口，解析不到就 false）。
//
// 用**独立**的可选接口而不是往 IsRateLimited 那个接口里加方法：
//
//	那个接口有 4 处实现（failover / protocol_codec / anthropic / responses），
//	加方法会逼着每个实现都补一个它并不关心的能力。这里只需一处能提供即可。
func rateLimitResetOf(err error) (time.Time, bool) {
	var r interface{ RateLimitReset() (time.Time, bool) }
	if errors.As(err, &r) {
		return r.RateLimitReset()
	}
	return time.Time{}, false
}

// rateLimitResetFloor / Ceiling 是采信上游重置时刻的**夹取区间**。
//
//	下界 1 分钟：上游偶尔会给出"已经过去"或"马上就到"的时刻，
//	直接用会让账号立刻回到可用、随即又撞一次 429（抖动）。
//	上界 7 天：防解析出垃圾（如年份写错）导致账号被冻到遥遥无期。
//	实测真实值在 9~17 小时区间内，两个界都不影响正常情况。
const (
	rateLimitResetFloor   = time.Minute
	rateLimitResetCeiling = 7 * 24 * time.Hour
)

// clampRateLimitReset 把上游给的重置时刻夹到合理区间（相对 now）。
func clampRateLimitReset(resetAt, now time.Time) time.Time {
	d := resetAt.Sub(now)
	if d < rateLimitResetFloor {
		d = rateLimitResetFloor
	}
	if d > rateLimitResetCeiling {
		d = rateLimitResetCeiling
	}
	return now.Add(d)
}

// clearAccountFailure 在一次成功后清掉失败痕迹。
//
// ⚠️ 不清 ManualDisabled —— 那是人工设置，不能被自动逻辑覆盖。
//
// 🔴 2026-10-07 修数据竞争：原来先**无锁读 3 个字段**判断 changed、
//
//	再**无锁写 5 个字段** —— 一个完全无锁的 read-modify-write。
//	现在整个"判断 + 写入"都在同一个受锁回调内完成（原子）。
//
// 🔴 R1（2026-10-07 修）：改用 `MutateIfRev` 条件提交。
//
//	这是 R1 的第二个洞，也是最隐蔽的一个：
//	一次 chat **成功**返回时，如果期间账号已被别的来源标成
//	`rate_limited`（例如并发的额度刷新），原来的无条件清除会把
//	**更新的失败状态抹掉** —— 用一次"基于旧前提的成功"覆盖了新事实。
//	版本不匹配就丢弃这次清除。
//
//	`baseRev` 必须是**发请求之前**取的基线，不能响应回来再读（见调用方说明）。
//
// 🔴 model（2026-10-09）：本次成功的**上游模型 ID**。
//
//	一次成功证明"这个模型对这个账号是可用的"，所以只清**这个模型**的
//	冷却；其他模型的限流事实不受影响（它们可能确实还在被限流）。
//	⚠️ 这也是为什么不能清整张表：那会把别的模型的真实限流抹掉。
func clearAccountFailure(deps Deps, uid string, baseRev uint64, model string) {
	if deps.Accounts == nil {
		return
	}
	now := nowFunc()
	changed, _ := deps.Accounts.MutateIfRev(uid, baseRev, func(a *auth.Account) bool {
		dirty := false

		// ── 该模型的冷却：一次成功即证明它可用 ──
		if model != "" && len(a.ModelCooldowns) > 0 {
			if _, ok := a.ModelCooldowns[model]; ok {
				delete(a.ModelCooldowns, model)
				if len(a.ModelCooldowns) == 0 {
					a.ModelCooldowns = nil
				}
				dirty = true
			}
		}

		// ── 账号级失败痕迹 ──
		//
		// 判断与写入在**同一次持锁**内 ⇒ 不会与并发写交错
		if !(a.Status.Normalize() == pool.StatusNormal &&
			a.StatusReason == "" && a.LastError == "") {
			a.Status = pool.StatusNormal
			a.StatusReason = ""
			a.StatusUntil = time.Time{}
			a.LastError = ""
			a.LastObservedAt = now
			dirty = true
		}
		return dirty
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
//	限流              → rate_limited，冷却 10 分钟（见 rateLimitCooldown）
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
			return pool.StatusRateLimited, "上游限流", rateLimitCooldown
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
