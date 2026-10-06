package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	"workbuddy.local/workbuddy-api/internal/router"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// TestChatUpstreamAuthFailureMapping 验证上游 401 映射为 401。
//
// 注意：必须让【对话】端点失败而【目录】端点成功 ——
// 否则 provider 注册阶段就失败了，测不到对话路径的错误映射。
func TestChatUpstreamAuthFailureMapping(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")
	// 只让 chat 路径返回 401
	fake.SetStatusForPath("/chat/completions", http.StatusUnauthorized)

	wbClient := newWorkbuddyClientForTest(t, fake.URL)
	wbProv := newWorkbuddyProviderForTest(wbClient)

	r := router.New()
	if err := r.Register(context.Background(), wbProv); err != nil {
		t.Fatalf("注册不应失败（目录是好的）: %v", err)
	}

	srv := httptest.NewServer(newMux(Deps{Router: r, Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 401（凭证失效应透传）\n响应: %s", resp.StatusCode, b)
	}

	var e openai.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Type != openai.ErrTypeAuth {
		t.Errorf("error.type = %q，期望 %q", e.Error.Type, openai.ErrTypeAuth)
	}
}

// TestChatUpstreamRateLimitMapping 验证 429 映射。
func TestChatUpstreamRateLimitMapping(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")
	fake.SetStatusForPath("/chat/completions", http.StatusTooManyRequests)

	wbClient := newWorkbuddyClientForTest(t, fake.URL)
	wbProv := newWorkbuddyProviderForTest(wbClient)
	r := router.New()
	if err := r.Register(context.Background(), wbProv); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(Deps{Router: r, Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d，期望 429", resp.StatusCode)
	}
}

// TestChatTruncatedStreamReportsError 验证上游流被截断时报错而非假装成功。
//
// 这是真实存在的坑：中间层曾有"流被截断却伪装成 [DONE]"的 bug。
func TestChatTruncatedStreamReportsError(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")
	// 一个没有 [DONE] 的 SSE
	fake.SSEBody = `data: {"id":"x","model":"space-bunny","choices":[{"index":0,"delta":{"content":"半截"},"finish_reason":""}]}` + "\n\n"

	wbClient := newWorkbuddyClientForTest(t, fake.URL)
	wbProv := newWorkbuddyProviderForTest(wbClient)
	r := router.New()
	if err := r.Register(context.Background(), wbProv); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(Deps{Router: r, Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	body := `{"model":"workbuddy/space-bunny","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 非流式路径：聚合出错 → 应返回错误状态而不是伪造成功
	if resp.StatusCode == http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("流被截断却返回 200，这正是要避免的"+"\n响应: %s", b)
	}
}
