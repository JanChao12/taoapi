package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
)

// scriptedChatter 是一个可编排的假账号对话器。
//
// 每个账号可以配置：失败、或不失败并产出若干事件。
type scriptedChatter struct {
	mu sync.Mutex

	// behavior 按账号 UID 指定行为。
	behavior map[string]*acctBehavior

	// calls 记录每个账号被调用了多少次。
	calls map[string]int

	// order 记录调用顺序。
	order []string
}

type acctBehavior struct {
	// failWith 非 nil 时立即返回该错误（模拟上游拒绝）。
	failWith error

	// events 成功时要产出的事件。
	events []provider.Event

	// failAfter 在产出第 N 个事件后失败（用于测试"已开始流之后不能换号"）。
	// 0 表示不中途失败。
	failAfter int

	// onCall 在"调用已开始、结果尚未返回"的窗口内执行一次。
	//
	// 🔴 用途（2026-10-07 加，用于 R1 的**端到端**验证）：
	//
	//	R1 的窗口正是"请求在途期间"。只在 store 层构造窗口
	//	（`failover_stale_test.go` 的前几条）能验证条件提交本身，
	//	但**验不到"调用方确实传了发请求前的基线"** ——
	//	而那恰恰是 Codex 特别点名的约束
	//	（"不能在响应回来后才读 Rev 再提交"）。
	//
	//	有了它就能在**真实 TryChat 调用链**里插入
	//	"删除账号 + 重导入同 UID"，从而端到端证明旧结果不会污染新账号。
	onCall func()
}

func newScriptedChatter() *scriptedChatter {
	return &scriptedChatter{
		behavior: make(map[string]*acctBehavior),
		calls:    make(map[string]int),
	}
}

func (c *scriptedChatter) set(uid string, b *acctBehavior) *scriptedChatter {
	c.behavior[uid] = b
	return c
}

func (c *scriptedChatter) ChatWithAccount(ctx context.Context, acct *auth.Account,
	req provider.ChatRequest, emit func(provider.Event) error) error {

	c.mu.Lock()
	c.calls[acct.UID]++
	c.order = append(c.order, acct.UID)
	b := c.behavior[acct.UID]
	c.mu.Unlock()

	if b == nil {
		return fmt.Errorf("测试未给账号 %s 配置行为", acct.UID)
	}
	// 在"请求在途"窗口内执行副作用（模拟并发的删除/重导入等）
	if b.onCall != nil {
		b.onCall()
	}
	if b.failWith != nil {
		return b.failWith
	}

	for i, ev := range b.events {
		// failAfter=N 表示"N 个事件成功发出后"才失败，
		// 所以检查放在 emit 之后。
		if err := emit(ev); err != nil {
			return err
		}
		if b.failAfter > 0 && i+1 >= b.failAfter {
			return errors.New("流中途失败")
		}
	}
	return nil
}

func (c *scriptedChatter) callCount(uid string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[uid]
}

func (c *scriptedChatter) callOrder() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order...)
}

// textEvents 构造一个"正常"的事件序列。
func textEvents(texts ...string) []provider.Event {
	evs := []provider.Event{{Type: provider.EventContent, Text: "role"}}
	for _, t := range texts {
		evs = append(evs, provider.Event{Type: provider.EventContent, Text: t})
	}
	evs = append(evs, provider.Event{Type: provider.EventDone, FinishReason: "stop"})
	return evs
}

// newFailoverDeps 构造一个带 N 个账号的 Deps。
func newFailoverDeps(t *testing.T, uids ...string) (Deps, *auth.Store) {
	t.Helper()

	store := auth.NewStore()
	for i, uid := range uids {
		store.Put(&auth.Account{
			UID:            uid,
			Nickname:       fmt.Sprintf("账号%d", i),
			AccessToken:    "token-" + uid,
			Status:         pool.StatusNormal,
			ManualDisabled: false,
			Credit: auth.CreditSnapshot{
				Known:     true,
				Remaining: int64(1000 - i),
				Packages: []auth.PackageSnapshot{
					{Name: "包", Remain: int64(1000 - i), ExpireAt: "2027-01-01"},
				},
			},
		})
	}

	return Deps{
		Accounts: store,
		Logger:   testLogger(t),
	}, store
}

// testLogger 返回丢弃输出的 logger（测试时不想刷屏，但代码路径要走通）。
func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

// rateLimitedErr 是一个被 provider 层识别的限流错误。
type rateLimitedErr struct{}

func (rateLimitedErr) Error() string       { return "上游限流 429" }
func (rateLimitedErr) IsRateLimited() bool { return true }
func (rateLimitedErr) IsAuthFailure() bool { return false }

// authFailErr 是一个被识别为凭证失效的错误。
type authFailErr struct{}

func (authFailErr) Error() string       { return "上游 401" }
func (authFailErr) IsRateLimited() bool { return false }
func (authFailErr) IsAuthFailure() bool { return true }

// ─────────────────────────────────────────────────────────────
// 🔴 核心：换号
// ─────────────────────────────────────────────────────────────

// TestFailoverOnRateLimit 是【本轮核心测试】。
//
// 委托人的要求：「账号异常了就换用另一个号的额度」。
// 首选账号限流 → 必须自动换到备用账号并成功。
func TestFailoverOnRateLimit(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-aaa", "uid-bbb")

	chatter := newScriptedChatter()
	chatter.set("uid-aaa", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-bbb", &acctBehavior{events: textEvents("可用")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatalf("应换号成功，实际失败: %v", err)
	}
	defer sess.Close()

	if sess.Account == nil || sess.Account.UID != "uid-bbb" {
		t.Fatalf("应换到 uid-bbb，实际 %v", sess.Account)
	}

	// 两个账号都被试过，且顺序正确
	if got := chatter.callOrder(); len(got) != 2 ||
		got[0] != "uid-aaa" || got[1] != "uid-bbb" {
		t.Errorf("调用顺序 = %v，期望 [uid-aaa uid-bbb]", got)
	}
}

// TestFailoverNeverTriesMoreThanTwoAccounts 验证不会打穿所有账号。
func TestFailoverNeverTriesMoreThanTwoAccounts(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a", "uid-b", "uid-c", "uid-d")

	chatter := newScriptedChatter()
	for _, uid := range []string{"uid-a", "uid-b", "uid-c", "uid-d"} {
		chatter.set(uid, &acctBehavior{failWith: rateLimitedErr{}})
	}

	_, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err == nil {
		t.Fatal("全部失败时应返回错误")
	}

	if n := len(chatter.callOrder()); n != maxAccountAttempts {
		t.Errorf("尝试了 %d 个账号，期望最多 %d 个（不该打穿所有号）",
			n, maxAccountAttempts)
	}
}

// TestNoFailoverOnUnsupportedReasoning 验证参数类错误【不】换号。
//
// 换号也解决不了档位不支持，只会白耗另一个号的配额。
func TestNoFailoverOnUnsupportedReasoning(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a", "uid-b")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{failWith: provider.ErrUnsupportedReasoning})
	chatter.set("uid-b", &acctBehavior{events: textEvents("不应到达")})

	_, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if !errors.Is(err, provider.ErrUnsupportedReasoning) {
		t.Fatalf("应返回档位错误，实际 %v", err)
	}

	if n := chatter.callCount("uid-b"); n != 0 {
		t.Errorf("档位不支持时不该换号，但 uid-b 被调用了 %d 次", n)
	}
}

// TestFailoverAfterStreamStartedIsImpossible 验证流已开始后不再换号。
//
// 这是"换号只能在首字节之前"的边界 —— 一旦调用方开始消费事件，
// 就只能把当前流走完（或报错），不能偷偷换成另一个账号，
// 否则客户端会收到两个账号的内容拼接。
func TestFailoverAfterStreamStartedIsImpossible(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a", "uid-b")

	chatter := newScriptedChatter()
	// uid-a 先产出一个事件，之后失败
	chatter.set("uid-a", &acctBehavior{
		events:    []provider.Event{{Type: provider.EventContent, Text: "开头"}},
		failAfter: 1,
	})
	chatter.set("uid-b", &acctBehavior{events: textEvents("不该出现")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatalf("首个事件已到达，TryChat 应成功: %v", err)
	}
	defer sess.Close()

	if sess.Account.UID != "uid-a" {
		t.Fatalf("应锁定在 uid-a，实际 %v", sess.Account.UID)
	}

	// 消费剩余事件时应拿到错误（而不是静默换号）
	restErr := sess.Rest(func(provider.Event) error { return nil })
	if restErr == nil {
		t.Error("流中途失败应返回错误")
	}

	if n := chatter.callCount("uid-b"); n != 0 {
		t.Errorf("🔴 流已开始却换了号！uid-b 被调用 %d 次，"+
			"客户端会收到两个账号拼接的内容", n)
	}
}

// TestFailoverRecordsAccountStatus 验证失败会写入账号状态（面板要用）。
//
// 🔴 2026-10-09 按模型限流后：限流记在 **ModelCooldowns**（按模型），
// 账号级状态保持正常 —— 否则该账号其他仍可用的模型会被一起挡掉。
func TestFailoverRecordsAccountStatus(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a", "uid-b")

	const model = "workbuddy/space-bunny"
	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-b", &acctBehavior{events: textEvents("ok")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	a, _ := store.Get("uid-a")
	// 账本级：不该被标成 rate_limited（那会冻住其他模型）
	if a.Status.Normalize() == pool.StatusRateLimited {
		t.Errorf("限流账号状态 = %q，期望**正常**（限流按模型记，不冻整个账号）", a.Status)
	}
	// 模型级：冷却必须落在本次模型上
	if len(a.ModelCooldowns) == 0 {
		t.Error("限流应设置**模型级**冷却（ModelCooldowns），实际为空")
	} else if until, ok := a.ModelCooldowns[model]; !ok || until.IsZero() {
		t.Errorf("ModelCooldowns 缺少 %q，实际 %v", model, a.ModelCooldowns)
	}
	if !a.StatusUntil.IsZero() {
		t.Errorf("账号级 StatusUntil = %v，期望零值（模型级限流不该设账号级冷却）", a.StatusUntil)
	}
	if !strings.Contains(a.StatusReason, "限流") {
		t.Errorf("状态原因应说明是限流，实际 %q", a.StatusReason)
	}

	// 成功的账号应被标为正常
	sessB, _ := store.Get("uid-b")
	if sessB.Status.Normalize() != pool.StatusNormal {
		t.Errorf("成功账号状态 = %q，期望 normal", sessB.Status)
	}
}

// TestFailoverSkipsCoolingAccountOnNextRequest 验证冷却中的账号下次不被选。
func TestFailoverSkipsCoolingAccountOnNextRequest(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a", "uid-b")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-b", &acctBehavior{events: textEvents("ok")})

	// 第一次：uid-a 失败 → 换 uid-b
	s1, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()

	before := chatter.callCount("uid-a")

	// 第二次：uid-a 在冷却中，应直接选 uid-b（不再试 uid-a）
	s2, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if after := chatter.callCount("uid-a"); after != before {
		t.Errorf("冷却中的账号不该再次被调用：之前 %d 次，之后 %d 次", before, after)
	}
	if s2.Account.UID != "uid-b" {
		t.Errorf("第二次应直接用 uid-b，实际 %v", s2.Account.UID)
	}
}

// TestModelLimitDoesNotBlockOtherModelsOnSameAccount 是**账号+模型限流**的
// 核心端到端护栏 —— 委托方实测要求的那个行为。
//
// ═══════════════════════════════════════════════════════════════════
// 委托方原话（2026-10-09）：
//
//	「上游的限流我实测之前使用DeepSeek过多导致限制，
//	  我切换glm后能正常使用」
//
// ═══════════════════════════════════════════════════════════════════
//
// 场景：同一个账号，模型 A 被限流；再请求**模型 B** 时，
// 该账号**必须仍然可用**（不能因为 A 被限就把整个账号跳过）。
//
// 这正是"账号级冷却"做不到、而"账号+模型冷却"能做到的事：
// 旧实现会把 StatusUntil 设在账号上，于是 B 也被挡掉 ——
// 用户明明可以切模型继续用，却被本服务拦住了。
//
// ⚠️ 本测试用**真实 TryChat 调用链**（不是直接调 pool），
//
//	所以它同时守住"模型确实被传到了选号那一步"这条接线。
//
// 反向对照：把 recordAccountFailure 里的模型级分支去掉
// （退回写账号级 StatusUntil）⇒ 本测试立刻红。
func TestModelLimitDoesNotBlockOtherModelsOnSameAccount(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a", "uid-b")

	// uid-b 只允许成功一次（第一次换号用掉），之后让它也失败，
	// 这样"能否用 uid-a"就成为可观察的唯一变量。
	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-b", &acctBehavior{events: textEvents("ok")})

	const limited = "workbuddy/deepseek-v4.1-flash"
	const other = "workbuddy/glm-5.3-flash"

	// ① 用 limited 模型打一次：uid-a 被限流 → 换到 uid-b 成功
	s1, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: limited})
	if err != nil {
		t.Fatalf("第一次（限流换号）应成功: %v", err)
	}
	s1.Close()

	// uid-a 的冷却必须记在**模型级**
	a, _ := store.Get("uid-a")
	if until, ok := a.ModelCooldowns[limited]; !ok || until.IsZero() {
		t.Fatalf("限流未记到模型级（ModelCooldowns=%v）", a.ModelCooldowns)
	}
	// 其他模型**不该**被牵连
	if _, ok := a.ModelCooldowns[other]; ok {
		t.Errorf("模型 %q 被无辜牵连（它没有被限流）：%v", other, a.ModelCooldowns)
	}

	// ② 同一个账号，换**另一个模型**请求 —— uid-a 必须重新可用
	//
	// 让 uid-a 这次成功，以证明它确实被选中了（而不是被跳过）。
	chatter.set("uid-a", &acctBehavior{events: textEvents("来自 uid-a 的 glm 回复")})

	beforeA := chatter.callCount("uid-a")
	s2, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: other})
	if err != nil {
		t.Fatalf("其他模型应能正常调度: %v", err)
	}
	defer s2.Close()

	if s2.Account == nil || s2.Account.UID != "uid-a" {
		got := "<nil>"
		if s2.Account != nil {
			got = s2.Account.UID
		}
		t.Errorf("其他模型选中 %s，期望 uid-a ——\n"+
			"  只有 %s 被限流，该账号对 %s 仍应可用。\n"+
			"  若这里跳过 uid-a，说明限流被记成了**账号级**冷却，"+
			"  用户明明能切模型继续用却被本服务拦住。", got, limited, other)
	}
	if after := chatter.callCount("uid-a"); after != beforeA+1 {
		t.Errorf("uid-a 对 %q 的调用次数 = %d，期望 %d（该模型未限流，应被尝试）",
			other, after, beforeA+1)
	}

	// ③ 反向：被限流的那个模型仍然要跳过 uid-a
	beforeA2 := chatter.callCount("uid-a")
	s3, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: limited})
	if err != nil {
		t.Fatalf("限流模型应能换号成功: %v", err)
	}
	defer s3.Close()
	if after := chatter.callCount("uid-a"); after != beforeA2 {
		t.Errorf("被限流的模型 %q 不该再试 uid-a：之前 %d，之后 %d",
			limited, beforeA2, after)
	}
}

// TestUpstreamResetTimeIsUsedForModelCooldown 守：上游自述的重置时刻
// 被真正用上（而不是永远走 10 分钟兜底）。
//
// ═══════════════════════════════════════════════════════════════════
// 委托方 2026-10-09 第 2 轮要求（原话）：
//
//	「如果能实现账号+模型限流方式就可以将限制时间改为从上游获取
//	  而不是写死的10分钟」
//
// ═══════════════════════════════════════════════════════════════════
//
// 用真实抓到的中文响应体构造 429（时间戳指向 ~12 小时后），
// 断言落盘的模型冷却 ≈ 上游给的时间，而**不是** 10 分钟。
//
// 反向对照：忽略 RateLimitResetAt、永远用 rateLimitCooldown ⇒ 本条红。
func TestUpstreamResetTimeIsUsedForModelCooldown(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a", "uid-b")

	const model = "workbuddy/deepseek-v4.1-flash"

	// 上游给的重置时刻：取"现在 + 12 小时"，格式与实测完全一致
	base := nowFunc()
	resetAt := base.Add(12 * time.Hour)
	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` +
		resetAt.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02 15:04:05") +
		` UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"x"}`

	parsed, ok := workbuddy.ParseRateLimitReset([]byte(body))
	if !ok {
		t.Fatalf("测试构造的响应体解析失败（格式应与实测一致）: %s", body)
	}

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{
		failWith: &workbuddy.UpstreamError{
			StatusCode:       429,
			Op:               "chat",
			BizCode:          6004,
			BizMsg:           "您的使用量已超出频率限制",
			RateLimitResetAt: parsed,
		},
	})
	chatter.set("uid-b", &acctBehavior{events: textEvents("ok")})

	s, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: model})
	if err != nil {
		t.Fatalf("应换号成功: %v", err)
	}
	s.Close()

	a, _ := store.Get("uid-a")
	until, has := a.ModelCooldowns[model]
	if !has {
		t.Fatalf("没有记下模型冷却（ModelCooldowns=%v）", a.ModelCooldowns)
	}

	// 必须接近上游给的时间（12 小时），而不是兜底的 10 分钟
	got := until.Sub(base)
	if got < 11*time.Hour {
		t.Errorf("模型冷却 = %v，期望接近上游自述的 12 小时 ——\n"+
			"  说明上游的重置时刻**没有被采用**，退化成了兜底值\n"+
			"  （委托方明确要求「改为从上游获取而不是写死的10分钟」）。", got)
	}
	if got > 12*time.Hour+time.Minute {
		t.Errorf("模型冷却 = %v，超过上游给的 12 小时 —— 不该放大上游时间", got)
	}
}

// TestFailoverPrefersEarliestExpiryAccount 验证换号也遵守"最快过期优先"。
func TestFailoverPrefersEarliestExpiryAccount(t *testing.T) {
	store := auth.NewStore()
	// b 先过期，但额度少
	store.Put(&auth.Account{
		UID: "uid-later", AccessToken: "t", Status: pool.StatusNormal,
		Credit: auth.CreditSnapshot{Known: true, Remaining: 9000,
			Packages: []auth.PackageSnapshot{{Remain: 9000, ExpireAt: "2027-06-01"}}},
	})
	store.Put(&auth.Account{
		UID: "uid-sooner", AccessToken: "t", Status: pool.StatusNormal,
		Credit: auth.CreditSnapshot{Known: true, Remaining: 10,
			Packages: []auth.PackageSnapshot{{Remain: 10, ExpireAt: "2027-01-01"}}},
	})

	deps := Deps{Accounts: store, Logger: testLogger(t)}

	chatter := newScriptedChatter()
	chatter.set("uid-sooner", &acctBehavior{events: textEvents("ok")})
	chatter.set("uid-later", &acctBehavior{events: textEvents("ok")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if sess.Account.UID != "uid-sooner" {
		t.Errorf("应选最快过期的 uid-sooner，实际 %v", sess.Account.UID)
	}
}

// TestNoAccountsConfigured 验证无账号时的退化路径不崩。
func TestNoAccountsConfigured(t *testing.T) {
	deps := Deps{Logger: testLogger(t)} // 没有 Router 也没有账号
	chatter := newScriptedChatter()

	_, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err == nil {
		t.Fatal("无渠道应返回错误")
	}
}

// TestAllAccountsUnavailable 验证全部账号不可用时的错误。
func TestAllAccountsUnavailable(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a")
	a, _ := store.Get("uid-a")
	a.ManualDisabled = true

	chatter := newScriptedChatter()
	_, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err == nil {
		t.Fatal("全部禁用应返回错误")
	}
	if !strings.Contains(err.Error(), "没有可用账号") {
		t.Errorf("错误信息应说明没有可用账号，实际: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────
// 事件不丢失（重构最容易出错的地方）
// ─────────────────────────────────────────────────────────────

// TestFirstEventIsDelivered 验证首个事件不会被丢掉。
//
// 重构前 probeOnce 曾因 sent 标志没复位，把第二个及以后的事件全丢了 ——
// 表现是"流式只收到 1 个分片、正文为空"。这个测试锁死该行为。
func TestFirstEventIsDelivered(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{events: textEvents("第一段", "第二段", "第三段")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// 首个事件
	if sess.FirstEvent.Text != "role" {
		t.Errorf("首个事件 = %q，期望 role", sess.FirstEvent.Text)
	}

	// 剩余事件必须全部到达
	var got []string
	if err := sess.Rest(func(ev provider.Event) error {
		if ev.Type == provider.EventContent {
			got = append(got, ev.Text)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{"第一段", "第二段", "第三段"}
	if len(got) != len(want) {
		t.Fatalf("收到 %d 个事件 %v，期望 %d 个 %v（丢事件！）",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个事件 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestSingleEventStream 验证只有一个事件时不死锁。
func TestSingleEventStream(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{events: []provider.Event{
		{Type: provider.EventContent, Text: "只有这一个"},
	}})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if sess.FirstEvent.Text != "只有这一个" {
		t.Errorf("首个事件 = %q", sess.FirstEvent.Text)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Rest(func(provider.Event) error { return nil }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("单事件流不该报错，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("单事件流死锁了（Rest 没有返回）")
	}
}

// TestUpstreamFailsBeforeAnyEvent 验证上游零事件就失败时能正确换号。
func TestUpstreamFailsBeforeAnyEvent(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a", "uid-b")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{events: nil}) // 零事件、无错误
	chatter.set("uid-b", &acctBehavior{events: textEvents("ok")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatalf("零事件上游应触发换号: %v", err)
	}
	defer sess.Close()

	if sess.Account.UID != "uid-b" {
		t.Errorf("应换到 uid-b，实际 %v", sess.Account.UID)
	}
}

// TestClientCancelStopsUpstream 验证取消能终止上游。
func TestClientCancelStopsUpstream(t *testing.T) {
	deps, _ := newFailoverDeps(t, "uid-a")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{events: textEvents("a", "b", "c")})

	ctx, cancel := context.WithCancel(context.Background())
	sess, err := TryChat(ctx, deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}

	// 消费一个事件后取消
	cancel()
	sess.Close()

	// 不该 panic / 死锁
	_ = sess.Rest(func(provider.Event) error { return nil })
}

// ─────────────────────────────────────────────────────────────
// 状态分类
// ─────────────────────────────────────────────────────────────

// TestClassifyFailureIsConservative 验证错误分类保守、不乱标封号。
func TestClassifyFailureIsConservative(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     pool.Status
		failover bool
	}{
		{"限流", rateLimitedErr{}, pool.StatusRateLimited, true},
		{"401 凭证失效（不等于封号）", authFailErr{}, pool.StatusAuthExpired, true},
		{"档位不支持（不换号）", provider.ErrUnsupportedReasoning, pool.StatusNormal, false},
		{"取消（不换号）", context.Canceled, pool.StatusNormal, false},
		{"超时", context.DeadlineExceeded, pool.StatusTransient, true},
		{"未知错误不标封号", errors.New("莫名其妙的错"), pool.StatusTransient, true},
		{"nil", nil, pool.StatusUnknown, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, reason, _ := classifyFailure(c.err)
			if st != c.want {
				t.Errorf("状态 = %q，期望 %q", st, c.want)
			}
			if c.want == pool.StatusBanned {
				t.Error("测试表里不该有 banned —— 没有证据就不该标封号")
			}
			// 换号决策以 isNonRetryable 为准，而不是由状态反推
			if got := !isNonRetryable(c.err) && shouldFailover(st); got != c.failover {
				t.Errorf("是否换号 = %v，期望 %v", got, c.failover)
			}
			if reason == "" {
				t.Error("原因不该为空（面板要显示）")
			}
		})
	}
}

// TestNonRetryableDoesNotTouchAccountStatus 验证"换号无用"的错误
// 不会把账号标成异常 —— 那不是账号的错。
func TestNonRetryableDoesNotTouchAccountStatus(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a", "uid-b")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{failWith: provider.ErrUnsupportedReasoning})
	chatter.set("uid-b", &acctBehavior{events: textEvents("不该到达")})

	_, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if !errors.Is(err, provider.ErrUnsupportedReasoning) {
		t.Fatalf("应原样返回档位错误，实际 %v", err)
	}

	a, _ := store.Get("uid-a")
	if a.Status.Normalize() != pool.StatusNormal {
		t.Errorf("档位不支持不是账号问题，状态不该变，实际 %q", a.Status)
	}
	if a.StatusReason != "" {
		t.Errorf("不该记录状态原因，实际 %q", a.StatusReason)
	}
}

// TestClassifyNeverReturnsBanned 是全文件的护栏：
// 当前实现【没有任何路径】会产生 banned。
//
// 理由：Codex 第 8 轮指出「403 不能直接等于封号」。
// 只有拿到上游明确的封禁业务码时才该标 banned，而那需要新的实测证据。
// 一旦将来加入该逻辑，这个测试会提醒你去补对应的证据测试。
func TestClassifyNeverReturnsBanned(t *testing.T) {
	errs := []error{
		errors.New("x"), rateLimitedErr{}, authFailErr{},
		provider.ErrUnsupportedReasoning, context.Canceled,
		context.DeadlineExceeded, nil,
	}
	for _, e := range errs {
		st, _, _ := classifyFailure(e)
		if st == pool.StatusBanned {
			t.Errorf("输入 %v 被标成 banned —— 当前没有足够证据支持这个判定", e)
		}
	}
}

// TestRateLimitCooldownIsFallbackTenMinutes 守：429 冷却是 **10 分钟兜底值**，
// 且限流冷却走**模型级**（不冻账号）。
//
// ═══════════════════════════════════════════════════════════════════
// 决策演进（两轮，别把前一轮又改回去）：
//
//	第 1 轮（2026-10-09 早）委托方：「我建议先改成写死10分钟，因为
//	  他确实是按模型限制的，切换其他模型可以继续用」
//	  ⇒ 当时还没有 per-model 能力，只能把账号级冷却从 60 秒改成 10 分钟。
//
//	第 2 轮（2026-10-09 晚）委托方：「如果能实现账号+模型限流方式就可以
//	  将限制时间改为从上游获取而不是写死的10分钟」
//	  ⇒ 已实现 per-model，所以**优先用上游自述的重置时刻**；
//	     10 分钟降级为**解析不到时的兜底**。
//
// ═══════════════════════════════════════════════════════════════════
//
// 所以 rateLimitCooldown 现在的语义是"兜底值"，不再是唯一值：
// classifyFailure 仍返回它（协议层/无 model 的调用方需要某个值），
// 而带 model 的真实路径会优先采用上游时间（见 clampRateLimitReset）。
//
// 反向对照：把常量改回 60s ⇒ 本条红。
func TestRateLimitCooldownIsFallbackTenMinutes(t *testing.T) {
	// ① classifyFailure 仍给出兜底冷却（供无 model 上下文的调用方使用）
	st, reason, cd := classifyFailure(rateLimitedErr{})
	if st != pool.StatusRateLimited {
		t.Fatalf("限流状态 = %q，期望 %q", st, pool.StatusRateLimited)
	}
	if cd != 10*time.Minute {
		t.Errorf("限流兜底冷却 = %v，期望 10 分钟", cd)
	}
	if reason == "" {
		t.Error("原因不该为空（面板要显示）")
	}

	// ② 常量本身（防止有人改常量却让测试仍绿）
	if rateLimitCooldown != 10*time.Minute {
		t.Errorf("rateLimitCooldown = %v，期望 10 分钟", rateLimitCooldown)
	}

	// ③ 上游重置时刻的夹取区间必须存在且合理。
	//
	//	下界 1 分钟：防上游给出"已过去"的时刻导致立即恢复又撞 429（抖动）；
	//	上界 7 天：防解析出垃圾把账号冻到遥遥无期（实测真实值 9~17 小时）。
	if rateLimitResetFloor != time.Minute {
		t.Errorf("rateLimitResetFloor = %v，期望 1 分钟", rateLimitResetFloor)
	}
	if rateLimitResetCeiling != 7*24*time.Hour {
		t.Errorf("rateLimitResetCeiling = %v，期望 7 天", rateLimitResetCeiling)
	}

	// ④ 夹取行为：过近的时刻被抬到 1 分钟，过远的被压到 7 天。
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if got := clampRateLimitReset(now.Add(-time.Hour), now); !got.Equal(now.Add(time.Minute)) {
		t.Errorf("已过去的重置时刻应被抬到 +1 分钟，实际 %v", got.Sub(now))
	}
	if got := clampRateLimitReset(now.Add(30*24*time.Hour), now); !got.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Errorf("过远的重置时刻应被压到 +7 天，实际 %v", got.Sub(now))
	}
	// 合理区间内的值必须**原样保留**（不能改变上游的准确信息）
	in := now.Add(12 * time.Hour)
	if got := clampRateLimitReset(in, now); !got.Equal(in) {
		t.Errorf("12 小时（实测范围内的真实值）被改动为 %v —— "+
			"夹取只应处理越界值，不能篡改正常值", got.Sub(now))
	}
}
