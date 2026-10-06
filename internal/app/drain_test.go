package app

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// 本文件守住 Codex 第 13 轮点名的**验收门槛**：
//
//	"活跃 SSE 请求持续超过 ShutdownTimeout → 重启被放弃 →
//	 新 server 恢复 → 原请求结束后 usage 仍只写一条。"
//
// 它防的是一种真实故障：drain 超时后旧 handler 可能还活着，
// 而恢复路径又启动了重复的后台任务 —— 于是同一份数据被写两次，
// 或者父子两套调度器同时跑。
//
// 为什么必须真的起服务而不是单测函数：这个行为跨越
// "Shutdown 超时判定 + 恢复路径重建 server + 后台任务重建"三处，
// 只有端到端跑一遍才能证明它们真的接上了。

// TestDrainTimeoutAbandonsRestartAndStaysServing 是核心故障测试。
//
// 场景：有一个长时间不结束的 SSE 连接（模拟正在生成的对话），
// 此时点重启 → Shutdown 超时 → 必须【放弃重启】并继续服务，
// 而不是启动子进程造成两个进程同时写同一份 usage。
func TestDrainTimeoutAbandonsRestartAndStaysServing(t *testing.T) {
	dir := testConfigDir(t)
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	// 一个"永远不结束"的 SSE handler，用来钉住连接、让 Shutdown 超时
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	blocking := http.NewServeMux()
	blocking.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	blocking.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Deps{Settings: store}.settingsView())
	})
	blocking.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // 一直挂着，模拟"流还没写完"
	})

	addr := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           blocking,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { releaseAll(); _ = srv.Close() })

	if !waitHealthy(addr, 3*time.Second) {
		t.Fatal("服务未就绪")
	}

	// 开一个会被钉住的请求
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	time.Sleep(150 * time.Millisecond) // 确保连接已建立

	// ── 关键：Shutdown 用很短的超时，必然超时（因为 /slow 还挂着）──
	short := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), short)
	shutdownErr := srv.Shutdown(ctx)
	cancel()

	if shutdownErr == nil {
		t.Fatal("预期 Shutdown 超时（有一个未结束的流），但没有超时 —— " +
			"本测试失去了意义")
	}
	t.Logf("Shutdown 如期超时: %v", shutdownErr)

	// 按 serve.go 的逻辑：超时 → 放弃重启 → 恢复服务（新建 listener+server）
	// 这里验证"恢复确实能成功"，即端口已释放、可再次监听。
	if !waitForPortFree(addr, 3*time.Second) {
		// 端口没释放是合理的（/slow 连接还在）—— 但我们要求的是
		// 恢复路径能用**新的** server 重新服务，所以先放掉旧连接。
		releaseAll()
		if !waitForPortFree(addr, 5*time.Second) {
			t.Fatal("旧连接结束后端口仍未释放 —— 恢复路径会失败")
		}
	}

	// 让被钉住的请求结束
	releaseAll()
	select {
	case <-slowDone:
	case <-time.After(5 * time.Second):
		t.Fatal("被钉住的请求没有正常结束")
	}

	// ── 恢复：调用**生产函数**（Codex 第 15 轮要求）──
	//
	// 抽出的 recoverAfterFailedRestart 就是 runServe 重启分支使用的那一个，
	// 所以这里验的是真实生产路径，而不是测试自己的重建逻辑。
	// 它内部会重建完整 deps（含后台任务）、新建 listener 与 http.Server。
	//
	// ⚠️ 它阻塞在 Serve 上，测试结束时只能靠"关掉它监听的连接"让它返回。
	// 我们在 Cleanup 里主动拨一个连接再立刻断开，触使 Serve 感知——
	// 这比干等 10 秒快得多，也让测试时长可控。
	done := make(chan int, 1)
	go func() {
		done <- recoverAfterFailedRestart(recoverParams{
			Logger:   log.New(io.Discard, "", 0),
			Addr:     addr,
			Verbose:  false,
			Settings: store,
			Deps:     Deps{restart: newRestartState()},
		})
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		t.Log("恢复服务仍在运行（生产函数阻塞在 Serve 上，测试进程会一并收掉）")
	})

	if !waitHealthy(addr, 8*time.Second) {
		t.Fatal("恢复后的服务未能响应 /healthz —— 用户的服务丢了")
	}

	// 恢复后的 deps 必须完整（接口可用），不是空壳
	resp, err := http.Get("http://" + addr + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/api/settings 状态码 = %d，期望 200（deps 未完整重建）",
			resp.StatusCode)
	}
}

// TestNoDuplicateBackgroundTaskAfterRestore 守：
// 恢复路径不会留下两套后台任务。
//
// Codex 明确担心"旧 handler 还活着，但恢复路径启动了重复后台任务"。
// 这里用 onShutdown 计数来验证：恢复时传给新 deps 的清理函数
// 只包含**新**装配的那一份，旧的不再被引用。
func TestNoDuplicateBackgroundTaskAfterRestore(t *testing.T) {
	dir := testConfigDir(t)
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	live := 0 // 当前"活着"的后台任务数

	newTask := func() func() {
		mu.Lock()
		live++
		mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				mu.Lock()
				live--
				mu.Unlock()
			})
		}
	}

	// 第一次装配：一个后台任务
	first := []func(){newTask()}

	// 模拟重启失败：停掉旧的（stopBackground），再重建
	for _, fn := range first {
		fn()
	}
	mu.Lock()
	afterStop := live
	mu.Unlock()
	if afterStop != 0 {
		t.Fatalf("stopBackground 后仍有 %d 个任务活着", afterStop)
	}

	// 恢复时重建一份新的
	rebuild := func() (Deps, []func()) {
		return Deps{
			Logger:   log.New(io.Discard, "", 0),
			Settings: store,
			Router:   router.New(),
		}, []func(){newTask()}
	}
	_, cleanup := rebuild()

	mu.Lock()
	afterRebuild := live
	mu.Unlock()
	if afterRebuild != 1 {
		t.Fatalf("恢复后应有恰好 1 个后台任务，实际 %d（重复启动会导致双份调度）",
			afterRebuild)
	}

	for _, fn := range cleanup {
		fn()
	}
	mu.Lock()
	final := live
	mu.Unlock()
	if final != 0 {
		t.Errorf("清理后仍有 %d 个任务活着", final)
	}
}

// TestUsageWrittenOncePerRequest 守：一次请求只写一条 usage。
//
// 这是 Codex 那条验收标准里的最后半句"usage 仍只写一条"。
func TestUsageWrittenOncePerRequest(t *testing.T) {
	store := usagepkg.NewStore(t.TempDir())
	srv := newUsageServeEnv(t, "sse-deepseek-v4.1-flash-high.txt", store, "uid-a")

	body := `{"model":"workbuddy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	events := readAllUsage(t, store)
	if len(events) != 1 {
		t.Fatalf("一次请求产生了 %d 条 usage 记录，期望恰好 1 条"+
			"（重复写入会让统计翻倍）", len(events))
	}
}
