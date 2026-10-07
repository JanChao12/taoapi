package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequireLoopback 是安全边界测试：
// 服务只允许监听回环地址，任何对外地址都必须被拒绝。
func TestRequireLoopback(t *testing.T) {
	allowed := []string{
		"127.0.0.1:8787",
		"127.0.0.1:0",
		"localhost:8787",
		"[::1]:8787",
	}
	for _, addr := range allowed {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("requireLoopback(%q) 应通过，却报错: %v", addr, err)
		}
	}

	// 拒绝集：这些一旦放行就会把服务暴露出去
	rejected := []struct {
		addr string
		why  string
	}{
		{"0.0.0.0:8787", "监听全部网卡"},
		{":8787", "空主机等于全部网卡"},
		{"192.168.1.10:8787", "局域网地址"},
		{"10.0.0.1:8787", "内网地址"},
		{"example.com:8787", "外部主机名"},
		{"8.8.8.8:8787", "公网地址"},
		{"not-an-address", "格式非法"},
	}
	for _, tc := range rejected {
		if err := requireLoopback(tc.addr); err == nil {
			t.Errorf("requireLoopback(%q) 必须被拒绝（%s），却通过了", tc.addr, tc.why)
		}
	}
}

// TestHealthz 验证健康检查可用。
//
// ⚠️ 响应体已从纯文本 "ok" 改为 JSON（Codex 第 19 轮）：
//
//	面板要判"对面是不是我们的服务、就绪没有、是不是本次交接的进程"，
//	纯文本 200 无法表达这些，会让探针退化成"端口是否有人监听"。
//	详见 health.go 的包注释。
func TestHealthz(t *testing.T) {
	h := newMux(Deps{Logger: log.New(io.Discard, "", 0), Verbose: false})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("请求 /healthz 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz 状态码 = %d，期望 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)

	var hr healthResponse
	if err := json.Unmarshal(body, &hr); err != nil {
		t.Fatalf("/healthz 响应体不是合法 JSON: %v（body=%q）", err, string(body))
	}
	if hr.Service != ProductName {
		t.Errorf("service = %q，期望 \"wbapi\"（面板靠它区分无关服务）", hr.Service)
	}
	if !hr.Ready {
		t.Error("ready 应为 true")
	}
	// 非重启启动时 restartId 省略（omitempty）——面板在无标识时不校验该项。
	if hr.RestartID != "" {
		t.Errorf("非重启启动时 restartId 应为空，实际 %q", hr.RestartID)
	}
}

// TestHealthzMethodNotAllowed 验证非 GET/HEAD 被拒。
func TestHealthzMethodNotAllowed(t *testing.T) {
	h := newMux(Deps{Logger: log.New(io.Discard, "", 0), Verbose: false})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/healthz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz 状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestStatusReportsLimits 验证 /status 暴露了限额，
// 这样"限额被误改"能立刻在测试里暴露。
func TestStatusReportsLimits(t *testing.T) {
	h := newMux(Deps{Logger: log.New(io.Discard, "", 0), Verbose: false})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("请求 /status 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status 状态码 = %d，期望 200", resp.StatusCode)
	}

	var got struct {
		Service string `json:"service"`
		Limits  struct {
			MaxConcurrentStreams int `json:"maxConcurrentStreams"`
			MaxRequestBodyBytes  int `json:"maxRequestBodyBytes"`
		} `json:"limits"`
		Memory struct {
			NumGoroutine int `json:"numGoroutine"`
		} `json:"memory"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析 /status 失败: %v", err)
	}

	if got.Service != ProductName {
		t.Errorf("service = %q，期望 %q", got.Service, ProductName)
	}
	if got.Limits.MaxConcurrentStreams != MaxConcurrentStreams {
		t.Errorf("maxConcurrentStreams = %d，期望 %d", got.Limits.MaxConcurrentStreams, MaxConcurrentStreams)
	}
	if got.Limits.MaxRequestBodyBytes != MaxRequestBodyBytes {
		t.Errorf("maxRequestBodyBytes = %d，期望 %d", got.Limits.MaxRequestBodyBytes, MaxRequestBodyBytes)
	}
	if got.Memory.NumGoroutine <= 0 {
		t.Errorf("numGoroutine = %d，应为正数", got.Memory.NumGoroutine)
	}
}

// TestUnknownPathReturnsJSON 验证 404 返回 JSON 而不是 HTML，
// 这样 OpenAI 兼容客户端能正确解析错误。
func TestUnknownPathReturnsJSON(t *testing.T) {
	h := newMux(Deps{Logger: log.New(io.Discard, "", 0), Verbose: false})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 JSON", ct)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("404 响应不是合法 JSON: %v", err)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("error.code = %q，期望 not_found", body.Error.Code)
	}
}

// TestRunUnknownCommand 验证未知命令返回退出码 2。
func TestRunUnknownCommand(t *testing.T) {
	if code := Run([]string{"bogus"}); code != 2 {
		t.Errorf("未知命令退出码 = %d，期望 2", code)
	}
}

// TestRunNoArgs 无参数时的行为**取决于是否 GUI 模式**。
//
// 🔴 契约在 2026-10-07 变了（委托方要求，照 wild-work）：
//
//	改造前：无参数 → 打印用法、退出码 2
//	改造后：无参数 → **GUI 模式**（起服务 + 托盘 + 弹提示）
//
//	所以"无参数"到底做什么，由 `useGUIForNoArgs()` 决定：
//	  · 生产（双击 exe）⇒ GUI 模式
//	  · 测试（WBAPI_NO_GUI=1）⇒ 退回打印用法 + 2
//
//	⚠️ 这里**必须显式设 WBAPI_NO_GUI=1**：否则会真的去起服务、
//	  建托盘窗口，把测试**永久挂住**（实测挂到 121 秒超时），
//	  还可能真绑上 8787 端口污染开发机实例。
func TestRunNoArgs(t *testing.T) {
	t.Setenv("WBAPI_NO_GUI", "1")
	if code := Run(nil); code != 2 {
		t.Errorf("非 GUI 模式下无参数退出码 = %d，期望 2（打印用法）", code)
	}
}

// TestRunNoArgsInGUIEnvironmentEntersGUIMode 守：GUI 环境下无参数**不进 usage**。
//
// ⚠️ 不能真的调用 Run(nil)（会起服务并阻塞）—— 所以这里只断言
//
//	"决定是否进 GUI"的判定函数本身。真正的 GUI 行为由人工实测覆盖
//	（托盘需要真实通知区，自动化测试拿不到）。
func TestRunNoArgsInGUIEnvironmentEntersGUIMode(t *testing.T) {
	t.Setenv("WBAPI_NO_GUI", "")
	if !useGUIForNoArgs() {
		t.Fatal("未设 WBAPI_NO_GUI 时应判定为 GUI 模式（双击 exe 的路径）")
	}

	t.Setenv("WBAPI_NO_GUI", "1")
	if useGUIForNoArgs() {
		t.Fatal("设了 WBAPI_NO_GUI 时应判定为非 GUI（测试路径）")
	}
}

// TestRunVersion 版本命令返回 0。
func TestRunVersion(t *testing.T) {
	if code := Run([]string{"version"}); code != 0 {
		t.Errorf("version 退出码 = %d，期望 0", code)
	}
}

// TestServeRejectsNonLoopback 验证 serve 子命令会拒绝对外地址。
func TestServeRejectsNonLoopback(t *testing.T) {
	if code := runServe([]string{"--addr", "0.0.0.0:8787"}); code != 2 {
		t.Errorf("--addr 0.0.0.0:8787 退出码 = %d，期望 2（拒绝）", code)
	}
	if code := runServe([]string{"--addr", ":8787"}); code != 2 {
		t.Errorf("--addr :8787 退出码 = %d，期望 2（拒绝）", code)
	}
}
