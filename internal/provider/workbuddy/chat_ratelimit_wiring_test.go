package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// newStatusProvider 构造一个**总是返回指定状态码与响应体**的 provider。
//
// ⚠️ 为什么不用 testutil.FakeUpstream.WithStatus：
//
//	它 `w.WriteHeader(code)` 之后**直接 return，不写 body**（见
//	fake_upstream.go:291）。而 429 的重置时间恰恰在 body 里 ——
//	用它测不出解析，只会得到"有状态码没 body"的假象。
//	（第一次写这条测试时就踩了：失败信息是 `HTTP 429` 后面什么都没有。）
func newStatusProvider(t *testing.T, code int, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.SetBases(srv.URL, srv.URL)
	return NewProvider(c, Credential{AccessToken: "t", UID: "uid-test"})
}

func chatOnce(t *testing.T, p *Provider) error {
	t.Helper()
	return p.Chat(context.Background(),
		provider.ChatRequest{
			Model:   "deepseek-v4.1-flash",
			RawBody: []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		},
		func(provider.Event) error { return nil })
}

// TestChat429CarriesParsedResetTime 是**接线**护栏：真实 HTTP 429 走完
// Provider.Chat 之后，错误对象上必须带着解析好的重置时刻。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么不能只测 ParseRateLimitReset（见 ratelimit_reset_test.go）
// ═══════════════════════════════════════════════════════════════════
//
//	只测纯函数会漏掉"**没人调用它**"这种缺陷 —— 解析函数写得再对，
//	chat.go 里没接上线，运行时就是永远走兜底冷却。
//	而那种缺陷在单元测试里**全绿**（正是本项目反复踩的那一类）。
//
//	本测试用真假的 HTTP 上游返回 429，走完整的 Provider.Chat，
//	断言错误上带 RateLimitResetAt。
//
// 反向对照：删掉 chat.go 里那段
//
//	`if resp.StatusCode == http.StatusTooManyRequests { ParseRateLimitReset(...) }`
//
// ⇒ 本条立刻红。
func TestChat429CarriesParsedResetTime(t *testing.T) {
	// 真实形状的 429（逐字取自 2026-10-09 日志的中文版）
	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-09 21:24:55 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"x"}`
	p := newStatusProvider(t, http.StatusTooManyRequests, body)

	err := chatOnce(t, p)
	if err == nil {
		t.Fatal("429 应返回错误")
	}

	// ① 分类正确
	cls, ok := err.(interface{ IsRateLimited() bool })
	if !ok || !cls.IsRateLimited() {
		t.Fatalf("错误应被识别为限流，实际 %T: %v", err, err)
	}

	// ② 必须带上解析出的重置时刻
	r, ok := err.(interface{ RateLimitReset() (time.Time, bool) })
	if !ok {
		t.Fatalf("错误类型 %T 没有 RateLimitReset() —— app 层取不到上游时间，"+
			"只能永远用兜底冷却", err)
	}
	got, has := r.RateLimitReset()
	if !has {
		t.Fatalf("没有解析出重置时刻 —— chat.go 里那段解析没接上？"+
			"（body 里明明有 2026-10-09 21:24:55 UTC+8）err=%v", err)
	}
	want := time.Date(2026, 10, 9, 13, 24, 55, 0, time.UTC) // 21:24:55 UTC+8
	if !got.Equal(want) {
		t.Errorf("重置时刻 = %v，期望 %v", got.UTC(), want)
	}
}

// TestChatNon429DoesNotParseResetTime 守：只有 429 才解析重置时间。
//
// 非限流的错误体里通常没有这个形状；即便有（比如某天上游在 500 里
// 也写了时间），也不该把它当成限流冷却用 —— 那会张冠李戴。
func TestChatNon429DoesNotParseResetTime(t *testing.T) {
	p := newStatusProvider(t, http.StatusInternalServerError,
		`{"code":6004,"msg":"将在 2026-10-09 21:24:55 UTC+8 重置"}`)

	err := chatOnce(t, p)
	if err == nil {
		t.Fatal("500 应返回错误")
	}
	if r, ok := err.(interface{ RateLimitReset() (time.Time, bool) }); ok {
		if _, has := r.RateLimitReset(); has {
			t.Error("非 429 响应不该解析出重置时刻（只有限流才有这个语义）")
		}
	}
}

// TestChat429WithoutTimestampFallsBack 守：429 但体里没有时间戳时，
// 错误上不带重置时刻（调用方据此退回兜底冷却）。
//
// 这是"上游换了措辞"的降级路径：**不报错**，只是用兜底值。
func TestChat429WithoutTimestampFallsBack(t *testing.T) {
	p := newStatusProvider(t, http.StatusTooManyRequests,
		`{"code":6004,"msg":"too many requests","requestId":"x"}`)

	err := chatOnce(t, p)
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	if r, ok := err.(interface{ RateLimitReset() (time.Time, bool) }); ok {
		if _, has := r.RateLimitReset(); has {
			t.Error("体里没有时间戳时不该解析出重置时刻")
		}
	}
	// 但错误信息里仍要能看出是限流（诊断需要）
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("错误信息应含状态码，实际 %v", err)
	}
}
