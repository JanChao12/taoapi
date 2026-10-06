package app

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/protocol/openai"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// 本文件守住"用量统计真的会写数据"这一环。
//
// 🔴 背景（2026-10-05 委托人问"用量统计实现了吗"）：
// 此前【全仓库没有任何地方调 usage.Store.Append】——
// stats.go 读的一端写好了、serve 也构造了 Store，但没人写。
// 结果是用量页永远"暂无数据"，即使真跑过对话。
//
// 下面这些测试保证：跑一次对话就真的落一条记录，且各种边界都不造假。

// newUsageServeEnv 起一个带用量存储的完整 HTTP 链路。
//
// 复用 newServeTestEnv（它已经装配了账号 + 假上游 + 路由），
// 只把 Usage 换成测试自己的 store —— 这样测试能直接读回写入的事件。
func newUsageServeEnv(t *testing.T, sse string, store *usagepkg.Store,
	uids ...string) *httptest.Server {
	t.Helper()

	deps, _, _ := newServeTestEnv(t, sse, uids...)
	deps.Usage = store // 注入可读回的存储

	srv := httptest.NewServer(newMux(deps))
	t.Cleanup(srv.Close)
	return srv
}

// TestUsageRecordedOnAggregatePath 是核心回归：非流式请求必须落一条用量记录。
//
// 这条测试存在的唯一理由：防止"读的一端有、写的一端没有"再次发生。
func TestUsageRecordedOnAggregatePath(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	srv := newUsageServeEnv(t, "sse-deepseek-v4.1-flash-high.txt", store, "uid-a")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	events := readAllUsage(t, store)
	if len(events) != 1 {
		t.Fatalf("用量记录数 = %d，期望 1（写的一端缺失会导致 0）", len(events))
	}
	ev := events[0]
	if !ev.OK {
		t.Errorf("OK = false，期望 true；Status=%q Error=%q", ev.Status, ev.Error)
	}
	if ev.Status != usagepkg.StatusOK {
		t.Errorf("Status = %q，期望 %q", ev.Status, usagepkg.StatusOK)
	}
	if ev.Model != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("Model = %q（应记录客户端请求的模型名）", ev.Model)
	}
	if ev.Protocol != "chat" {
		t.Errorf("Protocol = %q，期望 chat", ev.Protocol)
	}
	if ev.RequestID == "" {
		t.Error("RequestID 为空 —— 将来无法关联同一次请求的多条记录")
	}
	if ev.Account == "" {
		t.Error("Account 为空 —— 用量页按账号聚合会漏掉这条")
	}
}

// TestUsageRecordedOnStreamPath 验证流式路径也落记录。
func TestUsageRecordedOnStreamPath(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	srv := newUsageServeEnv(t, "sse-deepseek-v4.1-flash-high.txt", store, "uid-a")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	events := readAllUsage(t, store)
	if len(events) != 1 {
		t.Fatalf("流式路径的用量记录数 = %d，期望 1", len(events))
	}
	if !events[0].Stream {
		t.Error("Stream 应为 true")
	}
}

// TestUsageNeverFabricatesTokensWhenUnknown 守：没拿到 usage 时不写 token 数。
//
// 🔴 这是 Codex 第 9 轮明确要求的语义：客户端中途断开时我们拿不到最终 usage，
// 绝不能把中途累计的分片当最终值写进去 —— 那会污染平均值，
// 而且用户会看到一个"用过但 token 为 0"的怪记录且无从判断真假。
func TestUsageNeverFabricatesTokensWhenUnknown(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "upstream-m", false, "uid", "workbuddy")

	rec.recordOK(nil) // 上游没给 usage

	events := readAllUsage(t, store)
	if len(events) != 1 {
		t.Fatalf("记录数 = %d，期望 1", len(events))
	}
	ev := events[0]
	if ev.UsageKnown {
		t.Error("UsageKnown 应为 false（上游没给 usage）")
	}
	if ev.PromptTokens != 0 || ev.CompletionTokens != 0 || ev.TotalTokens != 0 {
		t.Errorf("未拿到 usage 时 token 必须留 0，实际 p=%d c=%d t=%d",
			ev.PromptTokens, ev.CompletionTokens, ev.TotalTokens)
	}
	if ev.Status != usagepkg.StatusNoUsage {
		t.Errorf("Status = %q，期望 %q", ev.Status, usagepkg.StatusNoUsage)
	}
	if !ev.OK {
		t.Error("拿到了正文但没有 usage，仍应算成功（OK=true）")
	}
}

// TestUsageRecordsRealTokens 验证拿到 usage 时数字确被写入。
func TestUsageRecordsRealTokens(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "upstream-m", false, "uid", "workbuddy")

	credit := 1.25
	rec.recordOK(&openai.EventUsage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		ReasoningTokens: 30, Credit: &credit,
	})

	ev := readAllUsage(t, store)[0]
	if !ev.UsageKnown {
		t.Error("UsageKnown 应为 true")
	}
	if ev.PromptTokens != 100 || ev.CompletionTokens != 50 || ev.TotalTokens != 150 {
		t.Errorf("token 数不符: %+v", ev)
	}
	if ev.ReasoningTokens != 30 {
		t.Errorf("思考 token = %d，期望 30", ev.ReasoningTokens)
	}
	if ev.Credit == nil || *ev.Credit != 1.25 {
		t.Errorf("credit 未记录: %v", ev.Credit)
	}
}

// TestUsageClientDisconnectIsNotFailure 守：客户端断开单独分类，不算服务端失败。
//
// 否则用户自己在 DSH 里点"停止"就会拉低成功率，让人以为服务有问题。
func TestUsageClientDisconnectIsNotFailure(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "upstream-m", true, "uid", "workbuddy")

	rec.recordClientDisconnected()

	ev := readAllUsage(t, store)[0]
	if ev.Status != usagepkg.StatusClientDisconnected {
		t.Errorf("Status = %q，期望 %q", ev.Status, usagepkg.StatusClientDisconnected)
	}
	if ev.UsageKnown {
		t.Error("客户端断开时不应声称知道 usage")
	}
	if ev.OK {
		t.Error("客户端断开不应算 OK")
	}
}

// TestUsageWriteFailureDoesNotBreakRequest 守：记账失败不能把成功的对话变成失败。
//
// Codex 第 9 轮要求。做法：让 Append 必然失败（目录位置被一个【文件】占住）。
func TestUsageWriteFailureDoesNotBreakRequest(t *testing.T) {
	// 把 store 的目录指向一个"已经是文件"的路径 → MkdirAll 必失败
	blocker := t.TempDir() + string(os.PathSeparator) + "blocked"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := usagepkg.NewStore(blocker)

	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "upstream-m", false, "uid", "workbuddy")

	// 不应 panic；recordOK 内部已吞掉错误并只记日志
	rec.recordOK(&openai.EventUsage{PromptTokens: 1})

	// 反向确认：这个 store 确实写不进去（否则本测试没验证到东西）
	if err := store.Append(usagepkg.Event{}); err == nil {
		t.Fatal("预期该 store 写入失败，但成功了 —— 本测试失去意义")
	}
}

// TestIsClientDisconnect 验证断开判定的边界（宁可少判，不可多判）。
func TestIsClientDisconnect(t *testing.T) {
	yes := []error{
		errors.New("write tcp: broken pipe"),
		errors.New("read tcp: connection reset by peer"),
		errors.New("context canceled"),
		errors.New("http: abort Handler"),
	}
	for _, e := range yes {
		if !isClientDisconnect(e) {
			t.Errorf("%q 应判定为客户端断开", e)
		}
	}

	no := []error{
		nil,
		errors.New("上游返回 429"),
		errors.New("dial tcp: connection refused"),
		errors.New("unexpected EOF"),
	}
	for _, e := range no {
		if isClientDisconnect(e) {
			t.Errorf("%q 不应判定为客户端断开（会掩盖真实故障）", e)
		}
	}
}

// TestUsageDurationIsRecorded 验证耗时被记录（面板要显示）。
func TestUsageDurationIsRecorded(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "upstream-m", false, "uid", "workbuddy")

	time.Sleep(2 * time.Millisecond)
	rec.recordOK(&openai.EventUsage{PromptTokens: 1})

	ev := readAllUsage(t, store)[0]
	if ev.DurationMS < 1 {
		t.Errorf("DurationMS = %d，期望 >= 1（睡了 2ms）", ev.DurationMS)
	}
}

// TestUsageAliasRecordsClientModel 守：用别名请求时，记录的是客户端写的那名字。
//
// Codex 第 9 轮要求"统计同时记录请求模型名与解析后的模型"。
// 这里验证客户端维度被保留 —— 否则用量页会显示上游 ID，用户对不上自己起的别名。
func TestUsageAliasRecordsClientModel(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}

	// 客户端用 "dsf"，上游实际是 workbuddy/deepseek-v4.1-flash
	rec := newUsageRecorder(deps, "dsf", "deepseek-v4.1-flash", false, "uid", "workbuddy")
	rec.recordOK(nil)

	ev := readAllUsage(t, store)[0]
	if ev.Model != "dsf" {
		t.Errorf("Model = %q，期望客户端原名 dsf（而非上游 ID）", ev.Model)
	}
}

// TestStatsAverageExcludesUnknownUsage 守平均 token 的分母只用已知 usage。
//
// 🔴 Codex 第 11 轮要求：客户端断开 / 上游没给 usage 的记录 token 是 0
// （我们不伪造），若把它们算进平均值分母，会把"平均每请求 token"拉低，
// 看起来像"这反代很省 token"，实际是分母掺了未知样本。
func TestStatsAverageExcludesUnknownUsage(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())

	// 两条已知 usage（各 100 token）+ 一条未知（0 token）
	known := &openai.EventUsage{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100}
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "m", false, "uid", "workbuddy")
	rec.recordOK(known)
	rec.recordOK(known)
	rec.recordOK(nil) // 未知

	srv := httptest.NewServer(newMux(deps))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stats?days=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	if got.Total.Requests != 3 {
		t.Errorf("requests = %d，期望 3（未知样本仍应计入请求数）", got.Total.Requests)
	}
	if got.Total.UsageKnownCount != 2 {
		t.Errorf("usage_known_count = %d，期望 2", got.Total.UsageKnownCount)
	}
	if got.Total.UsageUnknownCount != 1 {
		t.Errorf("usage_unknown_count = %d，期望 1", got.Total.UsageUnknownCount)
	}
	if got.Total.AverageTokensPerKnownRequest == nil {
		t.Fatal("平均值不应为 nil（有 2 条已知样本）")
	}
	// 200 token / 2 条已知 = 100；若错用总请求数 3 会得到 66.67
	if avg := *got.Total.AverageTokensPerKnownRequest; avg != 100 {
		t.Errorf("平均 token = %v，期望 100（分母用已知样本数，不是总请求数）", avg)
	}
}

// TestStatsAverageNilWhenNoKnownUsage 守：没有已知样本时平均值为 null。
//
// 返回 0 会被误读成"平均消耗 0 token"。
func TestStatsAverageNilWhenNoKnownUsage(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	deps := Deps{Logger: log.New(io.Discard, "", 0), Usage: store}
	rec := newUsageRecorder(deps, "m", "m", false, "uid", "workbuddy")
	rec.recordOK(nil) // 唯一一条是未知

	srv := httptest.NewServer(newMux(deps))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stats?days=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got statsResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Total.AverageTokensPerKnownRequest != nil {
		t.Errorf("没有已知样本时平均值应为 null，实际 %v",
			*got.Total.AverageTokensPerKnownRequest)
	}
}

// ── 小工具 ──

// readAllUsage 读出存储里的全部事件（测试用）。
func readAllUsage(t *testing.T, store *usagepkg.Store) []usagepkg.Event {
	t.Helper()
	var out []usagepkg.Event
	if err := store.Read(1, func(ev usagepkg.Event) error {
		out = append(out, ev)
		return nil
	}); err != nil {
		t.Fatalf("读取用量事件失败: %v", err)
	}
	return out
}
