package app

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
)

// 本文件守住「立即重启」的失败语义。
//
// 🔴 Codex 第 10 轮指出我最初的设计有真实缺陷：
//
//	先说"先验证可拉起再退出"，但端口还握在父进程手里 ——
//	子进程 Listen 必然失败并立刻退出，父进程却已退出，服务彻底消失。
//	"验证可执行文件存在"挡不住这种失败（文件明明在）。
//
// 修正后：父进程先 Shutdown 释放端口 → 等端口真空出来 → 才拉子进程 →
// 子进程秒退则【回滚】重新监听原地址继续服务。
// 下面这些测试守住"回滚确实发生"这一条。

// TestRestartRequiresDedicatedSupport 守：没有 restartCh 时接口返回 503。
//
// 而不是假装成功 —— 否则用户点了按钮、服务却不会重启，无从判断。
func TestRestartRequiresDedicatedSupport(t *testing.T) {
	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(Deps{Settings: store}))
	defer srv.Close()

	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/settings/restart", "{}", nil)
	if code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503（不支持自动重启）", code)
	}
	if !strings.Contains(body, "restart_unavailable") {
		t.Errorf("应给出可识别的错误码，实际 %s", body)
	}
}

// TestRestartRejectsWrongMethod 守方法限制。
func TestRestartRejectsWrongMethod(t *testing.T) {
	store, _ := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	rst := newRestartState()
	srv := httptest.NewServer(newMux(Deps{Settings: store, restart: rst}))
	defer srv.Close()

	code, _ := doJSON(t, http.MethodGet, srv.URL+"/api/settings/restart", "", nil)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET 状态码 = %d，期望 405", code)
	}
	// 没被真正触发
	select {
	case <-rst.ch:
		t.Error("方法不对时不应触发重启")
	default:
	}
}

// TestRestartReturns202AndSignals 守：返回 202（已受理，未完成）并触发信号。
//
// 用 202 而不是 200 是 Codex 的建议：200 会让前端以为服务此刻已就绪，
// 立刻去连新地址而失败。
func TestRestartReturns202AndSignals(t *testing.T) {
	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rst := newRestartState()
	srv := httptest.NewServer(newMux(Deps{Settings: store, restart: rst}))
	defer srv.Close()

	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/settings/restart", "{}", nil)
	if code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202；body=%s", code, body)
	}
	if !strings.Contains(body, "127.0.0.1:") {
		t.Errorf("响应应带 newAddr 供前端跳转，实际 %s", body)
	}

	// 信号是异步发的（留了 300ms 让响应送达），故等待
	select {
	case <-rst.ch:
	case <-time.After(3 * time.Second):
		t.Error("未收到重启信号")
	}
}

// TestWaitForPortFreeDetectsBusyPort 守端口占用检测。
//
// 这是重启流程的关键一环：父进程必须确认端口真的空出来了才拉子进程。
func TestWaitForPortFreeDetectsBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	// 端口被占 → 必须返回 false（而不是傻等成功）
	if waitForPortFree(addr, 300*time.Millisecond) {
		t.Error("端口被占用时应返回 false")
	}

	// 释放后 → 返回 true
	_ = ln.Close()
	if !waitForPortFree(addr, 3*time.Second) {
		t.Error("端口释放后应返回 true")
	}
}

// TestRelaunchRefusesBusyPort 守：端口没释放时不拉子进程。
//
// 对应 Codex 指出的那个缺陷场景：如果端口还占着就 Start，
// 子进程会立刻失败退出，而调用方可能已经准备退出了。
// 这里验证 helper 本身会拒绝，把失败留在"还能回滚"的时刻。
func TestRelaunchRefusesBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	// 直接用很短的超时，避免测试卡 3 秒
	start := time.Now()
	if waitForPortFree(addr, 200*time.Millisecond) {
		t.Fatal("端口被占用，不应报告空闲")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("等待超时未生效，耗时 %v", elapsed)
	}
}

// TestRestoreAfterFailedRestartKeepsServing 是本次修正的核心测试。
//
// 验证：当子进程起不来时，父进程能重新监听原地址并继续提供服务 ——
// 而不是一走了之让用户的服务消失。
//
// 做法：直接调用 restoreAfterFailedRestart，让它绑一个空闲端口，
// 然后用 HTTP 请求确认它真的在服务。
func TestRestoreAfterFailedRestartKeepsServing(t *testing.T) {
	// 先占一个端口拿到地址，再释放，把它交给 restore 去监听
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	store, err := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Settings: store, Logger: testLogger(t), Router: router.New()}

	// rebuild 回调模拟"重新装配"：返回一份可用的 deps 与清理函数。
	// 真实调用方传的是 buildDeps 的结果（含新的签到守护）。
	rebuild := func() (Deps, []func()) {
		var stopped bool
		return deps, []func(){func() { stopped = true; _ = stopped }}
	}

	done := make(chan int, 1)
	go func() {
		done <- restoreAfterFailedRestart(testLogger(t), addr, rebuild)
	}()

	// 等它起来并能响应 /healthz
	var ok bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ok = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		t.Fatal("恢复后服务未能响应 /healthz —— 重启失败时用户的服务丢了")
	}
}

// TestRestoreAfterFailedRestartReportsWhenPortTaken 守：连原端口都占不回时
// 返回非 0（让进程以失败退出，日志里有原因），而不是静默死掉。
func TestRestoreAfterFailedRestartReportsWhenPortTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	store, _ := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	deps := Deps{Settings: store, Logger: testLogger(t), Router: router.New()}

	code := restoreAfterFailedRestart(testLogger(t), ln.Addr().String(),
		func() (Deps, []func()) { return deps, nil })
	if code == 0 {
		t.Error("端口被占时恢复应失败并返回非 0")
	}
}

// TestRestartStateRejectsConcurrent 守：重启进行中再点一次 → 409 而不是静默丢弃。
//
// 🔴 Codex 第 11 轮指出 v2 用"channel 满就丢弃"，用户连点几次
// 完全不知道哪次生效了。状态机让第二次得到明确的 409。
func TestRestartStateRejectsConcurrent(t *testing.T) {
	store, _ := config.Load(filepath.Join(testConfigDir(t), "config.json"))
	rst := newRestartState()
	srv := httptest.NewServer(newMux(Deps{Settings: store, restart: rst}))
	defer srv.Close()

	// 第一次：受理
	code, _ := doJSON(t, http.MethodPost, srv.URL+"/api/settings/restart", "{}", nil)
	if code != http.StatusAccepted {
		t.Fatalf("第一次重启状态码 = %d，期望 202", code)
	}

	// 第二次：此时状态是"重启中" → 必须 409
	code, body := doJSON(t, http.MethodPost, srv.URL+"/api/settings/restart", "{}", nil)
	if code != http.StatusConflict {
		t.Errorf("并发重启状态码 = %d，期望 409；body=%s", code, body)
	}
	if !strings.Contains(body, "restart_in_progress") {
		t.Errorf("应返回可识别的错误码，实际 %s", body)
	}
}

// TestRestartStateAbortAllowsRetry 守：失败回滚后可以再次尝试。
//
// 若 abort 没把状态复位，用户一次失败后就永远点不动重启了。
func TestRestartStateAbortAllowsRetry(t *testing.T) {
	rst := newRestartState()

	if !rst.begin() {
		t.Fatal("首次 begin 应成功")
	}
	if rst.begin() {
		t.Fatal("重启中再次 begin 应失败")
	}
	rst.abort()
	if !rst.begin() {
		t.Error("abort 之后应能再次 begin（否则用户一次失败就永久卡住）")
	}
}

// TestHealthMatchesDetectsUnhealthy 守健康握手的判据。
//
// 演进：v2 靠"1.5 秒没退出"猜 → v3 探测 /healthz 是否为 200
// → v4（Codex 第 19 轮）必须**四条件全中**：
//
//	HTTP 200 && service=="wbapi" && ready==true && restartId==本次标识
//
// 只判 200 不够：实测"恰好占着端口、返回通配 CORS + 200 的无关程序"
// 会让仅检查状态码的实现误判成功。
func TestHealthMatchesDetectsUnhealthy(t *testing.T) {
	const anyID = "00112233445566778899aabbccddeeff"

	// 没人监听的地址 → 不健康
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := probe.Addr().String()
	_ = probe.Close()
	if healthMatches(deadAddr, anyID) {
		t.Error("没有服务监听时应报告不健康")
	}

	// 非 200 的 /healthz → 不健康
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if healthMatches(strings.TrimPrefix(bad.URL, "http://"), anyID) {
		t.Error("返回 500 时应报告不健康")
	}

	// 返回 200 但**不是**我们的服务（如无关程序恰好占了端口）→ 不健康
	//
	// 🔴 这正是第 19 轮实测暴露的场景：只判状态码会被这种服务骗过去。
	//
	// ⚠️ 隔离技巧（反向对照实验发现的，两次）：
	//   只返回 `{"hello":"unrelated"}` 不行 —— 那样 `!h.Ready` 也会拦住它，
	//   "去掉 service 校验"的破坏仍然全绿，测试等于没守 service。
	//   所以这里**除 service 外处处像一个真服务**（ready:true + 正确 restartId），
	//   逼着失败原因只能是 service。
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"not-wbapi","ready":true,"restartId":"` + anyID + `"}`))
	}))
	defer impostor.Close()
	if healthMatches(strings.TrimPrefix(impostor.URL, "http://"), anyID) {
		t.Error("200 但不是 wbapi 的服务必须报告不健康（否则会跳到无关程序）")
	}

	// 服务标识正确但 **ready=false** → 不健康
	//
	// ⚠️ 这条单独隔离 `ready`：上面那条 impostor 已经因 service 不符被拒，
	// 若不另起一个"service 正确但没就绪"的服务，"去掉 ready 校验"的破坏
	// 就不会变红（反向对照实验发现）。
	notReady := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"wbapi","ready":false,"restartId":"` + anyID + `"}`))
	}))
	defer notReady.Close()
	if healthMatches(strings.TrimPrefix(notReady.URL, "http://"), anyID) {
		t.Error("ready=false 时必须报告不健康（对面还没就绪，跳过去会白屏）")
	}

	// 通配 CORS + 200 + 冒充 wbapi 标识的无关服务 → 仍必须不健康
	//
	// 这一条对应"面板侧不能只查 response.ok"：服务端能返回这些字段，
	// 但 restartId 对不上（它不知道本次交接标识）。
	wildcard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write([]byte(`{"service":"wbapi","ready":true,"restartId":"deadbeefdeadbeefdeadbeefdeadbeef"}`))
	}))
	defer wildcard.Close()
	if healthMatches(strings.TrimPrefix(wildcard.URL, "http://"), anyID) {
		t.Error("restartId 对不上的冒名服务必须报告不健康")
	}

	// 正确的服务且标识匹配 → 健康
	good := httptest.NewServer(newMux(Deps{
		Logger:    testLogger(t),
		restartID: anyID,
	}))
	defer good.Close()
	if !healthMatches(strings.TrimPrefix(good.URL, "http://"), anyID) {
		t.Error("正常服务且标识匹配时应报告健康")
	}
}

// TestHealthMatchesRequiresMatchingRestartID 守第四条件（交接标识）。
//
// 🔴 为什么必须有：上一次重启**遗留的旧进程**同样会返回
// service=wbapi / ready=true，只有 restartId 能区分
// "它是不是**本次**交接启动的那个进程"。
// 缺了这条，面板可能跳到"上次遗留、还活着"的旧进程上。
func TestHealthMatchesRequiresMatchingRestartID(t *testing.T) {
	const wantID = "00112233445566778899aabbccddeeff"
	const otherID = "ffeeddccbbaa99887766554433221100"

	srv := httptest.NewServer(newMux(Deps{
		Logger:    testLogger(t),
		restartID: wantID,
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	if !healthMatches(addr, wantID) {
		t.Error("restartId 与本次交接一致时应报告健康")
	}
	if healthMatches(addr, otherID) {
		t.Error("restartId 不一致时必须报告不健康" +
			"（否则会跳到上次重启遗留的旧进程）")
	}
	if healthMatches(addr, "") {
		t.Error("期望匹配某个标识、而对面没有标识时必须不健康")
	}
}
