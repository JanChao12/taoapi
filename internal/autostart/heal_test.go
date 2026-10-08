package autostart

import (
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 自愈的**决策逻辑**测试（纯函数，任何平台都能跑）
// ═══════════════════════════════════════════════════════════════════
//
// 背景（2026-10-08 委托人提问："如果 taoapi 的文件地址换了，
// 那是不是之后的开机自启又会失效了"）：
//
//	Run 键里存的是**写入那一刻的绝对路径字符串**，不是跟着 exe 走的引用。
//	移动/改名文件夹后自启**静默失效**（不弹框、无日志），
//	而面板 Enabled() 只查"值是否存在"⇒ 仍显示"已开启"，界面在骗用户。
//
//	修复靠启动时自愈。而自愈**最危险的部分是"要不要写"**这个判断：
//	判错方向有两个，代价完全不同：
//	  · 该写没写 → 自启继续失效（回到原缺陷）
//	  · 不该写却写了 → **替用户打开了他关掉的自启**（越权，更糟）
//	⇒ 所以决策逻辑抽成纯函数，在**任何平台无条件测试**，
//	  不依赖注册表门控（真读写的那部分见 autostart_windows_test.go）。

func TestParseExePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"含引号（路径带空格）", `"C:\Program Files\wbapi\wbapi.exe" serve --silent`, `C:\Program Files\wbapi\wbapi.exe`},
		{"无引号", `D:\tools\TAOAPI\taoapi.exe serve --silent`, `D:\tools\TAOAPI\taoapi.exe`},
		{"只有路径没参数", `D:\tools\TAOAPI\taoapi.exe`, `D:\tools\TAOAPI\taoapi.exe`},
		{"前导空白", `   D:\a\b.exe serve`, `D:\a\b.exe`},
		{"空串", ``, ``},
		{"只有空白", `   `, ``},
		{"引号未闭合（畸形）", `"D:\a\b.exe serve`, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseExePath(tc.in); got != tc.want {
				t.Errorf("ParseExePath(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNeedsHeal 是本次修复的**核心断言**：什么情况该重写、什么情况绝不能动。
func TestNeedsHeal(t *testing.T) {
	const cur = `D:\tools\TAOAPI\taoapi.exe`

	cases := []struct {
		name    string
		cmdLine string
		want    bool
		why     string
	}{
		{
			name:    "🔴 未启用（值不存在）→ 绝不能动",
			cmdLine: "",
			want:    false,
			why:     "用户没开自启（或主动关了），替他打开是越权",
		},
		{
			name:    "🔴 只有空白 → 同样视为未启用",
			cmdLine: "   ",
			want:    false,
			why:     "空白值没有意义，不该被当成'需要修'",
		},
		{
			name:    "路径一致（无引号）→ 不动",
			cmdLine: `D:\tools\TAOAPI\taoapi.exe serve --silent`,
			want:    false,
			why:     "每次都无谓写注册表没有意义",
		},
		{
			name:    "路径一致（带引号）→ 不动",
			cmdLine: `"D:\tools\TAOAPI\taoapi.exe" serve --silent`,
			want:    false,
			why:     "引号不该影响'是否同一路径'的判断",
		},
		{
			name:    "大小写不同 → 视为一致，不动",
			cmdLine: `d:\TOOLS\taoapi\TAOAPI.EXE serve`,
			want:    false,
			why:     "Windows 路径不区分大小写，判成不同会导致无谓重写",
		},
		{
			name:    "尾部反斜杠 → 视为一致，不动",
			cmdLine: `D:\tools\TAOAPI\taoapi.exe\ serve`,
			want:    false,
			why:     "尾部分隔符不该影响判定",
		},
		{
			name:    "★ 文件夹改名 → 需要重写",
			cmdLine: `D:\tools\TAOAPI-OLD\taoapi.exe serve --silent`,
			want:    true,
			why:     "这正是委托人担心的场景",
		},
		{
			name:    "★ exe 改名 → 需要重写",
			cmdLine: `D:\tools\TAOAPI\wbapi.exe serve --silent`,
			want:    true,
			why:     "只改文件名同样让路径失效",
		},
		{
			name:    "★ 整个目录搬走 → 需要重写",
			cmdLine: `E:\backup\TAOAPI\taoapi.exe serve --silent`,
			want:    true,
			why:     "换盘符是最常见的搬家方式",
		},
		{
			name:    "★ 指向开发目录的旧路径 → 需要重写",
			cmdLine: `"D:\download\DSH\构建反代项目\workbuddy-api\wbapi.exe" serve`,
			want:    true,
			why:     "第 55 轮实测踩到的真实场景（自启跑错实例）",
		},
		{
			name:    "🔴 畸形值（引号未闭合）→ 不猜、不动",
			cmdLine: `"D:\broken\path.exe serve`,
			want:    false,
			why:     "解析不出路径时保持原样，总好过程序擅自改成它以为对的值",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsHeal(tc.cmdLine, cur); got != tc.want {
				t.Errorf("NeedsHeal(%q, %q) = %v，期望 %v —— %s",
					tc.cmdLine, cur, got, tc.want, tc.why)
			}
		})
	}
}

// TestSameExePath 守路径比较的边界。
func TestSameExePath(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`D:\a\b.exe`, `D:\a\b.exe`, true},
		{`D:\a\b.exe`, `d:\A\B.EXE`, true},
		{`D:\a\b.exe`, `D:\a\b.exe\`, true},
		{`  D:\a\b.exe  `, `D:\a\b.exe`, true},
		{`D:\a\b.exe`, `D:\a\c.exe`, false},
		{`D:\a\b.exe`, `D:\aa\b.exe`, false},
		{``, `D:\a\b.exe`, false},
	}
	for _, tc := range cases {
		if got := SameExePath(tc.a, tc.b); got != tc.want {
			t.Errorf("SameExePath(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestHealOutcomeString 守三个结论都有可读文案（日志与断言都依赖它）。
func TestHealOutcomeString(t *testing.T) {
	for _, o := range []HealOutcome{HealNotEnabled, HealUnchanged, HealRewritten} {
		s := o.String()
		if s == "" || strings.HasPrefix(s, "未知") {
			t.Errorf("HealOutcome(%d).String() = %q，不该是空或未知", int(o), s)
		}
	}
}

// TestIsTestBinary 守"测试二进制绝不写注册表"这道闸。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 2026-10-08 我自己踩的坑（比原缺陷更糟，实测发生）
// ═══════════════════════════════════════════════════════════════════
//
//	加了自愈之后跑 `go test ./internal/app/`，真实发生了：
//
//	  注册表 [wbapi] 从
//	    "D:\tools\TAOAPI\taoapi.exe" serve --silent
//	  被改写成
//	    C:\Users\...\Temp\go-build3565253560\b136\app.test.exe serve --silent
//
//	那个临时文件**测试结束就被删掉**，而 Run 键失效是**静默的** ⇒
//	用户下次开机服务不会起来，且完全没有线索。
//
//	⇒ 自愈必须限定在"真实运行"语境。这里用**真实踩到的那个路径**
//	  当用例，确保它永远被拦住。
func TestIsTestBinary(t *testing.T) {
	// ⚠️ 第一条就是**实测污染过我注册表的那个真实路径**（原样抄下来）。
	blocked := []string{
		`C:\Users\Administrator\AppData\Local\Temp\go-build3565253560\b136\app.test.exe`,
		`C:\Users\Administrator\AppData\Local\Temp\go-build123\b001\autostart.test.exe`,
		`D:\a\b\app.test.exe`,
		`C:\Temp\anything.exe`,
		`/tmp/go-build999/b002/x.test.exe`,
		`C:\x\go-build\y.exe`,
	}
	for _, p := range blocked {
		if !IsTestBinary(p) {
			t.Errorf("IsTestBinary(%q) = false，必须为 true —— "+
				"放行它会把用户的自启项改成临时测试路径（实测踩过）", p)
		}
	}

	// 反向：真实的生产 exe 绝不能被误判（否则自愈永远不生效）。
	allowed := []string{
		`D:\tools\TAOAPI\taoapi.exe`,
		`D:\download\DSH\构建反代项目\workbuddy-api\wbapi.exe`,
		`C:\Program Files\TAOAPI\taoapi.exe`,
		`E:\tools\taoapi.exe`,
		``,
	}
	for _, p := range allowed {
		if IsTestBinary(p) {
			t.Errorf("IsTestBinary(%q) = true，真实 exe 不该被误判"+
				"（误判会让自愈永远不生效）", p)
		}
	}
}
