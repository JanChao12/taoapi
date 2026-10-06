package autostart

import (
	"strings"
	"testing"
)

// TestCommandLine 覆盖【命令行拼接】这一最易错的纯逻辑。
//
// 为什么这段必须无条件运行（不受平台/环境变量门控）：
// 拼错的后果是"开机时静默不启动"——Run 键启动失败没有任何界面提示，
// 用户只会觉得"自启没生效"，几乎不可能定位到少了一对引号。
// 所以它是本包唯一必须 100% 覆盖的路径。
//
// 关键断言点：路径【含空格时必须带引号】。
// 反例：`C:\Program Files\wbapi\wbapi.exe serve` 不加以引号，
// Windows 会按空格拆成 `C:\Program` + `Files\wbapi\wbapi.exe` + `serve`。
func TestCommandLine(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		args []string
		want string
	}{
		{
			name: "路径无空格",
			exe:  `C:\wbapi\wbapi.exe`,
			args: []string{"serve"},
			want: `C:\wbapi\wbapi.exe serve`,
		},
		{
			name: "路径有空格必须加引号",
			exe:  `C:\Program Files\wbapi\wbapi.exe`,
			args: []string{"serve"},
			want: `"C:\Program Files\wbapi\wbapi.exe" serve`,
		},
		{
			name: "路径有多个空格",
			exe:  `D:\my tools\the app\wbapi.exe`,
			args: []string{"serve"},
			want: `"D:\my tools\the app\wbapi.exe" serve`,
		},
		{
			name: "args 为空：只有可执行文件",
			exe:  `C:\wbapi\wbapi.exe`,
			args: nil,
			want: `C:\wbapi\wbapi.exe`,
		},
		{
			name: "args 为空切片（与 nil 等价）",
			exe:  `C:\wbapi\wbapi.exe`,
			args: []string{},
			want: `C:\wbapi\wbapi.exe`,
		},
		{
			name: "args 多个",
			exe:  `C:\wbapi\wbapi.exe`,
			args: []string{"serve", "--port", "8787"},
			want: `C:\wbapi\wbapi.exe serve --port 8787`,
		},
		{
			name: "路径含空格 + args 多个",
			exe:  `C:\Program Files\wbapi\wbapi.exe`,
			args: []string{"serve", "--port", "8787"},
			want: `"C:\Program Files\wbapi\wbapi.exe" serve --port 8787`,
		},
		{
			name: "路径含中文（无空格，不加引号）",
			exe:  `D:\工具\反代\wbapi.exe`,
			args: []string{"serve"},
			want: `D:\工具\反代\wbapi.exe serve`,
		},
		{
			name: "路径含中文且有空格：必须加引号",
			exe:  `D:\我的 工具\wbapi.exe`,
			args: []string{"serve"},
			want: `"D:\我的 工具\wbapi.exe" serve`,
		},
		{
			name: "含中文空格路径 + 中文参数",
			exe:  `D:\构建反代 项目\wbapi.exe`,
			args: []string{"serve", "--备注", "测试 用"},
			// 注意 `--备注` 本身无空格 → 不加引号；`测试 用` 含空格 → 加引号。
			// 引号是【按需】加的，不是给每个参数都套一层。
			want: `"D:\构建反代 项目\wbapi.exe" serve --备注 "测试 用"`,
		},
		{
			name: "参数自身含空格要加引号",
			exe:  `C:\wbapi\wbapi.exe`,
			args: []string{"serve", "--config", `C:\my dir\a.json`},
			want: `C:\wbapi\wbapi.exe serve --config "C:\my dir\a.json"`,
		},
		{
			name: "参数内的双引号被转义",
			exe:  `C:\wbapi\wbapi.exe`,
			args: []string{`a"b c`},
			want: `C:\wbapi\wbapi.exe "a\"b c"`,
		},
		{
			name: "空参数被丢弃（避免参数错位）",
			exe:  `C:\wbapi\wbapi.exe`,
			args: []string{"serve", "", "--port"},
			want: `C:\wbapi\wbapi.exe serve --port`,
		},
		{
			name: "UNICODE 全角空格不加引号（不是命令行分隔符）",
			exe:  `D:\工具　全角\wbapi.exe`,
			args: []string{"serve"},
			want: `D:\工具　全角\wbapi.exe serve`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandLine(tc.exe, tc.args)
			if got != tc.want {
				t.Fatalf("CommandLine 不符\n  输入 exe=%q args=%q\n  期望 %q\n  实际 %q",
					tc.exe, tc.args, tc.want, got)
			}
		})
	}
}

// TestCommandLineQuotesExeWithSpaces 单独把"引号"这个关键性质拎出来断言。
//
// 与上表有重叠是【故意】的：上表任一行被改坏都可能被人当成"格式调整"放过，
// 而这一条的名字直接说明了红线的存在理由，改坏时更容易在 review 里被拦住。
func TestCommandLineQuotesExeWithSpaces(t *testing.T) {
	const exe = `C:\Program Files\WorkBuddy\wbapi.exe`
	got := CommandLine(exe, []string{"serve"})

	if !strings.HasPrefix(got, `"`+exe+`"`) {
		t.Fatalf("含空格的可执行路径必须被双引号完整包裹，实际得到 %q", got)
	}
	// 再做一个"语义"层面的检查：把引号剥掉后按空格分词，
	// 第一个 token 必须【不是】原路径的前半段 —— 那正是漏引号时会发生的事。
	if strings.HasPrefix(got, `C:\Program `) {
		t.Fatalf("检测到漏引号（路径会在第一个空格处被截断）: %q", got)
	}
}

// TestQuoteArg 覆盖底层引号函数的边界。
func TestQuoteArg(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", `""`},
		{"plain", "plain"},
		{"has space", `"has space"`},
		{"has\ttab", "\"has\ttab\""},
		{`has"quote`, `"has\"quote"`},
		{`C:\path\to\file.exe`, `C:\path\to\file.exe`},
		{`C:\Program Files\a.exe`, `"C:\Program Files\a.exe"`},
		{"中文", "中文"},
		{"中文 空格", `"中文 空格"`},
	}
	for _, tc := range cases {
		if got := quoteArg(tc.in); got != tc.want {
			t.Errorf("quoteArg(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestCommandLineIsIdempotent 确认同样的输入永远得到同样的输出。
//
// 意义：Enable 是幂等的，而它每次都用 CommandLine 重新生成要写的内容。
// 若拼接带任何不确定性（比如依赖 map 顺序），第二次 Enable 就会写进不同的值，
// "幂等"就名存实亡了。
func TestCommandLineIsIdempotent(t *testing.T) {
	const exe = `C:\Program Files\wbapi\wbapi.exe`
	args := []string{"serve", "--port", "8787"}

	first := CommandLine(exe, args)
	for i := 0; i < 100; i++ {
		if got := CommandLine(exe, args); got != first {
			t.Fatalf("第 %d 次调用结果不同:\n  %q\n  %q", i, first, got)
		}
	}
}

// TestValidateExePath 覆盖写入前的路径校验。
//
// 为什么这条也要测：校验的失败模式是"往注册表里写了个不存在的路径"，
// 开机时表现为静默不启动 —— 和引号拼错一样难查。
func TestValidateExePath(t *testing.T) {
	t.Run("空路径被拒绝", func(t *testing.T) {
		if err := validateExePath(""); err == nil {
			t.Fatal("空路径必须报错")
		}
	})
	t.Run("纯空白路径被拒绝", func(t *testing.T) {
		if err := validateExePath("   \t "); err == nil {
			t.Fatal("纯空白路径必须报错")
		}
	})
	t.Run("含双引号的路径被拒绝", func(t *testing.T) {
		// 双引号会破坏命令行结构（无法区分是路径的一部分还是定界符），
		// 宁可拒绝也不猜测用户意图。
		if err := validateExePath(`C:\a"b\wbapi.exe`); err == nil {
			t.Fatal("含双引号的路径必须报错")
		}
	})
	t.Run("正常路径通过", func(t *testing.T) {
		for _, p := range []string{
			`C:\wbapi\wbapi.exe`,
			`C:\Program Files\wbapi\wbapi.exe`,
			`D:\工具\反代\wbapi.exe`,
			`D:\我的 工具\wbapi.exe`,
		} {
			if err := validateExePath(p); err != nil {
				t.Errorf("路径 %q 应当通过，却报错: %v", p, err)
			}
		}
	})
}

// TestCommandLineNeverContainsUnquotedSpacedPath 是一个性质测试：
// 对任意含空格的路径，生成的命令行里该路径必须整体处于引号内。
//
// 用"反例驱动"的方式写：只要漏引号，就能立刻定位到是哪个输入触发的。
func TestCommandLineNeverContainsUnquotedSpacedPath(t *testing.T) {
	paths := []string{
		`C:\Program Files\wbapi\wbapi.exe`,
		`C:\Program Files (x86)\wbapi\wbapi.exe`,
		`D:\a b c\wbapi.exe`,
		`D:\构建反代 项目\out\wbapi.exe`,
		`D:\工 具\反 代\wbapi.exe`,
		`\\server\share with space\wbapi.exe`,
	}
	for _, exe := range paths {
		got := CommandLine(exe, []string{"serve"})
		quoted := `"` + exe + `"`
		if !strings.HasPrefix(got, quoted) {
			t.Errorf("路径 %q 未被完整引号包裹: %q\n（期望以 %q 开头）", exe, got, quoted)
		}
	}
}
