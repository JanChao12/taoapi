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
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/router"
)

// contextBackground 避免 go1.22 下 t.Context 不可用。
func contextBackground() context.Context { return context.Background() }

// newValidationServer 起一个带 provider 的服务，用于测入站校验。
func newValidationServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := router.New()
	p := &stubProvider{
		id: "workbuddy",
		models: []provider.Model{{
			ID: "workbuddy/m1", UpstreamID: "m1", Name: "M1",
			Capabilities: provider.Capabilities{ContextWindow: 1000, MaxOutputTokens: 100},
		}},
	}
	if err := r.Register(contextBackground(), p); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(Deps{Router: r, Logger: log.New(io.Discard, "", 0)}))
	t.Cleanup(srv.Close)
	return srv
}

// postChat 发一个 chat 请求并返回响应。
func postChat(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestValidationMissingModel 验证缺 model 返回 400 + 明确错误码。
func TestValidationMissingModel(t *testing.T) {
	srv := newValidationServer(t)
	resp := postChat(t, srv, `{"messages":[{"role":"user","content":"x"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
	var e openai.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "missing_model" {
		t.Errorf("error.code = %q，期望 missing_model", e.Error.Code)
	}
}

// TestValidationMissingMessages 验证缺 messages 返回 400。
func TestValidationMissingMessages(t *testing.T) {
	srv := newValidationServer(t)
	resp := postChat(t, srv, `{"model":"workbuddy/m1"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
	var e openai.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "missing_messages" {
		t.Errorf("error.code = %q", e.Error.Code)
	}
}

// TestValidationEmptyMessages 验证 messages 为空数组也返回 400。
func TestValidationEmptyMessages(t *testing.T) {
	srv := newValidationServer(t)
	resp := postChat(t, srv, `{"model":"workbuddy/m1","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

// TestValidationBadJSONReturnsJSONError 验证非法 JSON 时返回的是 JSON 而非 HTML。
func TestValidationBadJSONReturnsJSONError(t *testing.T) {
	srv := newValidationServer(t)
	resp := postChat(t, srv, `{"model":`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 JSON", ct)
	}
	var e openai.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	if e.Error.Code != "invalid_json" {
		t.Errorf("error.code = %q", e.Error.Code)
	}
}

// TestValidationUnknownModel 验证未知模型返回 404。
func TestValidationUnknownModel(t *testing.T) {
	srv := newValidationServer(t)
	resp := postChat(t, srv, `{"model":"workbuddy/nope","messages":[{"role":"user","content":"x"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", resp.StatusCode)
	}
	var e openai.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q", e.Error.Code)
	}
}

// TestValidationNoProvider 验证未配渠道时返回 503（而不是 500 或崩溃）。
func TestValidationNoProvider(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	resp := postChat(t, srv, `{"model":"x","messages":[{"role":"user","content":"y"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", resp.StatusCode)
	}
}

// TestValidationKnownModelReachesProvider 验证已知模型能走到 provider。
//
// stubProvider 返回 ErrNotImplemented，所以预期 502 ——
// 重点是【没有】在路由/校验阶段就被拒。
func TestValidationKnownModelReachesProvider(t *testing.T) {
	srv := newValidationServer(t)
	resp := postChat(t, srv, `{"model":"workbuddy/m1","messages":[{"role":"user","content":"x"}]}`)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("已知模型不该在校验阶段被拒：状态码 %d，响应 %s", resp.StatusCode, b)
	}
}
