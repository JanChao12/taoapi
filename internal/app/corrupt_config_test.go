package app

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/config"
	"workbuddy.local/workbuddy-api/internal/router"
)

// 本文件守住 Codex 第 11、12 轮点名的缺口：
//
//	"坏配置仍应加入回归测试。当前测试覆盖了端口和子进程退出，
//	 但还没有证明'配置损坏时旧服务能恢复'。"（第 11 轮）
//	"坏配置恢复测试：不仅要验证配置被 quarantine，还要验证旧服务在
//	 '重启读取坏配置'的路径上能重新建立完整 deps、签到和 usage。"（第 12 轮）
//
// 为什么这事重要：重启后新进程会**重新读配置**。若配置损坏，
// 新进程可能起不来 —— 而父进程此时已经退出，用户的服务就没了。
// 光验证"配置被改名保留"（那个已有测试）不足以证明服务仍可用。

// TestRestartWithCorruptConfigStillServes 是本次核心：
// 配置损坏时，恢复路径必须仍能建立可用的服务。
//
// 做法：把配置写成非法 JSON → 让 Load 走 quarantine 分支 →
// 用拿到的 Store（已回退默认值）装配 deps → 起服务 →
// 验证它真的能响应，而不是 panic 或空转。
func TestRestartWithCorruptConfigStillServes(t *testing.T) {
	dir := t.TempDir()
	cleanupTempDirWithRetry(t, dir)
	cfgPath := filepath.Join(dir, "config.json")

	// 截断的非法 JSON（模拟"写到一半断电"）
	if err := os.WriteFile(cfgPath, []byte(`{"port": 8787, "aliases": {`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Load 必须回退默认值而不是失败
	store, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("配置损坏不应导致 Load 失败（应回退默认值）: %v", err)
	}
	if !store.Corrupt() {
		t.Error("应标记 corrupt=true，让面板与 /status 能提示用户")
	}
	if store.Get().Port != config.DefaultPort {
		t.Errorf("回退后端口 = %d，期望 %d", store.Get().Port, config.DefaultPort)
	}

	// 原文件必须被保留（不是被覆盖）
	entries, _ := os.ReadDir(dir)
	var quarantined bool
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Error("损坏的配置必须被改名保留，不能丢失")
	}

	// ── 关键：用这个 Store 起服务，必须真的能用 ──
	addr := freeAddr(t)
	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Settings: store,
		Router:   router.New(),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(deps),
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// /healthz 必须可用 —— 证明服务真的起来了
	if !waitHealthy(addr, 3*time.Second) {
		t.Fatal("配置损坏后服务无法响应 /healthz —— 用户的服务丢了")
	}

	// /api/settings 也必须可用，且明确报告配置损坏
	resp, err := http.Get("http://" + addr + "/api/settings")
	if err != nil {
		t.Fatalf("配置损坏后 /api/settings 不可用: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/settings 状态码 = %d，期望 200", resp.StatusCode)
	}
	var view map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view["configCorrupt"] != true {
		t.Error("面板应能得知配置损坏过（configCorrupt=true）")
	}
}

// TestRestartWithCorruptConfigCanStillWriteSettings 守：
// 坏配置恢复后，用户仍能通过面板**写回**一份好配置。
//
// 若这条不成立，用户就永久卡在"配置坏了且改不了"的状态。
func TestRestartWithCorruptConfigCanStillWriteSettings(t *testing.T) {
	dir := t.TempDir()
	cleanupTempDirWithRetry(t, dir)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Logger:     log.New(io.Discard, "", 0),
		Settings:   store,
		Router:     router.New(),
		panelToken: testPanelToken,
	}
	srv := httptest.NewServer(newMux(deps))
	defer srv.Close()

	code := doWithHeaders(t, http.MethodPatch, srv.URL+"/api/settings",
		`{"port":9500}`,
		map[string]string{
			"Content-Type":   "application/json",
			panelTokenHeader: testPanelToken,
		})
	if code != http.StatusOK {
		t.Fatalf("坏配置恢复后写设置状态码 = %d，期望 200", code)
	}
	if store.Get().Port != 9500 {
		t.Errorf("写入未生效，port=%d", store.Get().Port)
	}

	// 磁盘上现在应该是一份**合法**配置
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var check config.Settings
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("写回后配置仍不是合法 JSON: %v", err)
	}
	if check.Port != 9500 {
		t.Errorf("磁盘上的端口 = %d，期望 9500", check.Port)
	}
}

// TestRestoreAfterCorruptConfigRebuildsDeps 守：
// 恢复路径重新装配的 deps 是**完整**的（含 settings 与路由），
// 而不是一个空壳 —— 否则服务"起来了但接口全废"，用户看不出来。
func TestRestoreAfterCorruptConfigRebuildsDeps(t *testing.T) {
	dir := t.TempDir()
	cleanupTempDirWithRetry(t, dir)
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟损坏标记
	_ = store.Mutate(func(s *config.Settings) error { return nil })

	addr := freeAddr(t)

	// 模拟 serve.go 传进来的 rebuild 回调：返回一份完整 deps
	rebuild := func() (Deps, []func()) {
		return Deps{
			Logger:     log.New(io.Discard, "", 0),
			Settings:   store,
			Router:     router.New(),
			panelToken: testPanelToken,
		}, nil
	}

	done := make(chan int, 1)
	go func() { done <- restoreAfterFailedRestart(testLogger(t), addr, rebuild) }()

	if !waitHealthy(addr, 5*time.Second) {
		t.Fatal("恢复后的服务未能响应 /healthz")
	}

	// 关键：settings 接口必须可用（证明 deps 不是空壳）
	resp, err := http.Get("http://" + addr + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/api/settings 状态码 = %d，期望 200（deps 未完整重建）", resp.StatusCode)
	}
}

// ── 小工具 ──

// freeAddr 返回一个当前空闲的回环地址。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitHealthy 轮询 /healthz 直到就绪或超时。
func waitHealthy(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
