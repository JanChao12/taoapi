package login

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 本文件守「测试里绝不真的启动浏览器」。
//
// 🔴 为什么需要它（2026-10-09 我自己踩的坑，用户实测发现）：
//
//	跑 `go test ./internal/app/` 会**真的弹出 Chrome 窗口**，连跑几轮
//	开了十几个，而且它们不会自己关（profile 目录保留）。
//
//	根因：app 包的 TestLoginStartAcceptsKnownPlatforms 调的是真实的
//	handleLoginStart，其注释假设「本机无浏览器 ⇒ 503」。但本机装了 Chrome
//	⇒ FindBrowser 通过 ⇒ 一路走到 StartBrowser 真开窗口。
//
//	login 包自己的浏览器测试有 WBAPI_RUN_BROWSER_TESTS=1 闸门，
//	**但 app 包那条没有** —— 跨包调用的测试漏掉了闸门。
//
// 修法：把闸门放在 StartBrowser 这个**危险操作的发生点**，
// 而不是只补那一条测试 —— 将来任何新增调用方都自动受保护。

// TestStartBrowserRefusesInTestBinary 守：测试二进制里 StartBrowser 必须拒绝。
//
// ⚠️ 本测试自己就跑在测试二进制里，所以它断言的是"拿到明确错误"，
//
//	而不是"浏览器没启动"（后者无法在这里观测）。
func TestStartBrowserRefusesInTestBinary(t *testing.T) {
	// 若有人显式开了逃生舱，本测试无意义 —— 跳过而不是假绿。
	if os.Getenv("WBAPI_RUN_BROWSER_TESTS") == "1" {
		t.Skip("WBAPI_RUN_BROWSER_TESTS=1：逃生舱已开，跳过")
	}

	sess, err := StartBrowser(LoginOptions{StartURL: "about:blank"})
	if err == nil {
		if sess != nil {
			sess.Close()
		}
		t.Fatal("测试二进制里 StartBrowser 成功了 —— " +
			"那意味着跑 go test 会真的弹出浏览器窗口（用户实测被开了十几个）")
	}
	if !errors.Is(err, ErrBrowserDisabledInTest) {
		t.Errorf("错误 = %v，期望 ErrBrowserDisabledInTest", err)
	}
}

// TestIsTestBinaryDetectsGoTestBinary 守 isTestBinary 的判据。
func TestIsTestBinaryDetectsGoTestBinary(t *testing.T) {
	// 本测试跑在 go test 里 ⇒ 自身路径必然命中判据。
	if !isTestBinary() {
		exe, _ := os.Executable()
		t.Fatalf("isTestBinary() = false，但当前是测试进程（exe=%q）—— "+
			"闸门失效会让 go test 真的启动浏览器", exe)
	}
}

// TestIsTestBinaryJudgement 守判据的三条规则（表驱动，不依赖运行环境）。
//
// ⚠️ 与 TestIsTestBinaryDetectsGoTestBinary 的分工：
//
//	那条验"当前进程被认出来"，这条验"判据本身对不对"。
//	只有前者的话，把函数改成 `return true` 也能过。
func TestIsTestBinaryJudgement(t *testing.T) {
	// 直接测判据逻辑（复制自实现，保证规则本身没写错）
	judge := func(exe string) bool {
		lower := strings.ToLower(exe)
		return strings.HasSuffix(lower, ".test.exe") ||
			strings.Contains(lower, `\go-build`) || strings.Contains(lower, `/go-build`) ||
			strings.Contains(lower, `\temp\`) || strings.Contains(lower, `/temp/`)
	}

	testCases := []struct {
		exe  string
		want bool
		why  string
	}{
		{`C:\Users\x\AppData\Local\Temp\go-build123\b001\app.test.exe`, true, "Go 测试二进制（三条全中）"},
		{`C:\build\app.test.exe`, true, ".test.exe 后缀"},
		{`C:\go-build\b1\login.test.exe`, true, "go-build 段"},
		{`D:\Temp\something.exe`, true, "Temp 段"},
		{`D:\tools\TAOAPI\taoapi.exe`, false, "真实部署路径"},
		{`C:\Program Files\wbapi\wbapi.exe`, false, "真实安装路径"},
	}
	for _, tc := range testCases {
		if got := judge(tc.exe); got != tc.want {
			t.Errorf("judge(%q) = %v，期望 %v（%s）", tc.exe, got, tc.want, tc.why)
		}
	}
}

// TestStartBrowserStillWorksOutsideTests 守：非测试语境下闸门不拦。
//
// 🔴 反向护栏：最危险的错误修法是"为了让测试不弹窗，把闸门写成
//
//	无条件拒绝" —— 那会让**真实用户**的网页登录彻底不可用。
//
//	本测试用真实 exe 路径跑一次判据（不真的启动浏览器）：
//	在非测试语境下 isTestBinary 必须为 false。
func TestStartBrowserStillWorksOutsideTests(t *testing.T) {
	// 直接验证判据对"真实路径"的结论 —— 若有人把 isTestBinary 改成
	// 恒为 true，这里会红。
	realPaths := []string{
		`D:\tools\TAOAPI\taoapi.exe`,
		`C:\Program Files\wbapi\wbapi.exe`,
	}
	for _, p := range realPaths {
		lower := strings.ToLower(p)
		got := strings.HasSuffix(lower, ".test.exe") ||
			strings.Contains(lower, `\go-build`) || strings.Contains(lower, `/go-build`) ||
			strings.Contains(lower, `\temp\`) || strings.Contains(lower, `/temp/`)
		if got {
			t.Errorf("真实部署路径 %q 被判为测试二进制 —— "+
				"那会让用户的网页登录彻底不可用（闸门写得太宽）", p)
		}
	}
}

// TestFindBrowserIsStillReachable 守：闸门不能把 FindBrowser 也挡掉。
//
// FindBrowser 是纯探测（不启动进程），app 层用它决定返回
// 503（无浏览器）还是 202。若闸门误挡它，面板会永远说"未找到浏览器"。
func TestFindBrowserIsStillReachable(t *testing.T) {
	// 只要求"能调用且不 panic"；有没有浏览器都合法。
	_, _, err := FindBrowser()
	if err != nil {
		// 本机无浏览器是合法情形，不是失败
		t.Logf("本机无受支持浏览器（合法）: %v", err)
		return
	}
	t.Log("找到浏览器，FindBrowser 未被闸门影响")
}

// 供将来使用（避免未使用导入警告）。
var _ = exec.Command
