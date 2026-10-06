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
func TestFailoverRecordsAccountStatus(t *testing.T) {
	deps, store := newFailoverDeps(t, "uid-a", "uid-b")

	chatter := newScriptedChatter()
	chatter.set("uid-a", &acctBehavior{failWith: rateLimitedErr{}})
	chatter.set("uid-b", &acctBehavior{events: textEvents("ok")})

	sess, err := TryChat(context.Background(), deps, chatter,
		provider.ChatRequest{Model: "workbuddy/space-bunny"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	a, _ := store.Get("uid-a")
	if a.Status != pool.StatusRateLimited {
		t.Errorf("限流账号状态 = %q，期望 rate_limited", a.Status)
	}
	if a.StatusUntil.IsZero() {
		t.Error("限流应设置冷却截止时间")
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
