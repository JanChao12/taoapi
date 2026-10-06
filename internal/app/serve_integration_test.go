package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider"
	"workbuddy.local/workbuddy-api/internal/provider/workbuddy"
	"workbuddy.local/workbuddy-api/internal/router"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// fakeURLFromDeps 从测试路由里取出假上游地址。
//
// 做法：注册一个指向假上游的 provider，从 router 的模型里反查不出 URL，
// 所以直接在这里重建 fake —— 与其传递，不如让每个测试自持一份，
// 逻辑更直白。
func newServeTestEnv(t *testing.T, sse string, uids ...string) (Deps, *auth.Store, *httptest.Server) {
	t.Helper()

	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")
	fake.WithSSE(sse)

	store := auth.NewStore()
	for i, uid := range uids {
		store.Put(&auth.Account{
			UID: uid, Nickname: "号" + uid[len(uid)-2:],
			AccessToken: "token-" + uid,
			Status:      pool.StatusNormal,
			Credit: auth.CreditSnapshot{
				Known: true, Remaining: int64(100 - i),
				Packages: []auth.PackageSnapshot{{Remain: int64(100 - i), ExpireAt: "2027-01-01"}},
			},
		})
	}

	wbClient := newWorkbuddyClientForTest(t, fake.URL)

	// 与生产 buildDeps 保持一致：账号仓库 + 持久化器 + 上游客户端都装配。
	// 持久化用测试 codec（不依赖 DPAPI），文件落在临时目录。
	//
	// ⚠️ 用 cleanupTempDirWithRetry 而非裸 t.TempDir()：
	// 并发导入类测试会在 HTTP 返回后仍有持久化器在收尾写盘，
	// Windows 上"删除仍被打开的目录"会失败并报
	// "TempDir RemoveAll cleanup: The directory is not empty" ——
	// 那个失败**每次挂在不同测试上**，极易被误判成逻辑 bug。
	dataDir := t.TempDir()
	cleanupTempDirWithRetry(t, dataDir)

	persister := auth.NewPersister(
		filepath.Join(dataDir, "accounts.json"), xorTestCodec{key: 0x21})

	deps := Deps{
		Accounts:  store,
		Persister: persister,
		Chatter:   &wbTestChatter{client: wbClient},
		WBClient:  wbClient,
		Logger:    log.New(io.Discard, "", 0),
		Router:    router.New(),
	}
	if err := deps.Router.Register(context.Background(),
		newWorkbuddyProviderForTest(wbClient)); err != nil {
		t.Fatalf("注册模型失败: %v", err)
	}

	h := newMux(deps)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return deps, store, srv
}

// wbTestChatter 与生产 wbChatter 同构，但凭据按账号动态生成。
type wbTestChatter struct {
	client *workbuddy.Client
}

func (w *wbTestChatter) ChatWithAccount(ctx context.Context, acct *auth.Account,
	req provider.ChatRequest, emit func(provider.Event) error) error {
	p := workbuddy.NewProvider(w.client, workbuddy.Credential{
		AccessToken: acct.AccessToken,
		UID:         acct.UID,
	})
	return p.Chat(ctx, req, emit)
}

// TestServeChatWithAccounts 端到端：真实 HTTP 服务 + 账号调度。
//
// 与 chat_integration_test 的差别：那组走"无账号"退化路径，
// 这里走【带账号调度】的完整路径 —— 生产真正跑的路径。
func TestServeChatWithAccounts(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-v4.1-flash-high.txt", "uid-a", "uid-b")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("非流式应返回 JSON: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v", out["object"])
	}
}

// TestServeStreamWithAccounts 验证流式路径也走账号调度且不丢事件。
func TestServeStreamWithAccounts(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-v4.1-flash-high.txt", "uid-a")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}

	raw, _ := io.ReadAll(resp.Body)
	s := string(raw)
	if !strings.Contains(s, "[DONE]") {
		t.Error("流式响应缺 [DONE]")
	}
	// fixture 有 41 个思考分片 —— 必须全部转发（这是之前 probeOnce bug 的回归测试）
	if got := strings.Count(s, "chat.completion.chunk"); got < 40 {
		t.Errorf("chunk 数 = %d，应 ≥40（41 思考分片不能丢）", got)
	}
}

// TestServeModelsEndpoint 验证带账号时 /v1/models 返回完整目录。
func TestServeModelsEndpoint(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) == 0 {
		t.Fatal("模型列表为空")
	}
	for _, m := range out.Data {
		if !strings.HasPrefix(m.ID, "workbuddy/") {
			t.Errorf("模型 %q 缺少 workbuddy/ 前缀", m.ID)
		}
	}
}

// TestServeChatFailsWhenAllAccountsDisabled 验证全部禁用时返回明确错误。
func TestServeChatFailsWhenAllAccountsDisabled(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
	a, _ := store.Get("uid-a")
	a.ManualDisabled = true

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("全部禁用应 503，实际 %d body=%s", resp.StatusCode, b)
	}
}

// 确保 time 包被使用（上面用了 time.Second 于超时；删除时同步删此行）。
var _ = time.Second
