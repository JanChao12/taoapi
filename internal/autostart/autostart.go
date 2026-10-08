// Package autostart 实现「开机自启」的注册（Windows）与查询。
//
// # 为什么用注册表 Run 键，而不是计划任务 / 启动文件夹
//
//   - 计划任务（schtasks）能配置更多策略（延迟、失败重试、以最高权限运行），
//     但它需要额外的命令行往返、输出解析，且在某些精简系统/组策略下行为不一致；
//     本项目只需要「登录后把本地服务拉起来」，属于 Run 键的典型用途。
//   - 启动文件夹要往用户目录里放一个 .lnk，需要走 COM（IShellLink）才能正确生成，
//     引入的复杂度比 Run 键只写一个字符串值大得多。
//   - Run 键是【当前用户】范围，不需要管理员权限，符合本项目"自用 + 单 exe"的定位。
//
// # 平台与依赖取舍
//
// 与 internal/storage 的 DPAPI 实现同一模式：
//
//   - Windows 上用 `syscall` 直接调 advapi32.dll（RegOpenKeyExW / RegSetValueExW /
//     RegQueryValueExW / RegDeleteValueW / RegCloseKey），**不用 cgo**（保持纯 Go
//     静态编译、单 exe 无外部依赖），**不引入 golang.org/x/sys**（本项目零第三方依赖）。
//   - 非 Windows 由 autostart_other.go 用 build tag 提供占位实现，**明确报错而不是
//     静默成功**。理由同 DPAPI：静默成功会让面板显示"已开启自启"而实际什么都没做，
//     比直接失败更难排查。
//
// 平台相关的部分拆成两个文件（autostart_windows.go / autostart_other.go），
// 本文件只放【与平台无关的纯逻辑】（命令行拼接），这样引号相关的测试在任何平台上
// 都能无条件运行，不需要 build tag 保护。
package autostart

import (
	"errors"
	"fmt"
	"strings"
)

// ValueName 是注册表 Run 项下本程序使用的值名。
//
// 固定为 "wbapi"：值名就是这个程序在开机自启列表里的身份，
// 改名会导致旧版本注册的项无法被 Disable 清理。
//
// ⚠️ 所有写操作【只允许】碰这个名字，绝不动同一键下的其他值
// （那是用户自己的开机项，弄丢等于帮用户删软件）。
const ValueName = "wbapi"

// runKeyPath 是 Run 键的相对路径（相对于 HKEY_CURRENT_USER）。
//
// 用【当前用户】而不是 HKLM：写 HKLM 需要管理员权限，
// 而本项目刻意要求"普通用户双击 exe 就能用"。
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// DefaultArgs 是注册自启时使用的默认命令行参数。
//
// 🔴 2026-10-07 改为带 `--silent`（委托方要求：开机自启**不弹窗**）：
//
//	手动双击时弹一次提示告知面板地址（用户想看）；
//	开机自启是**后台行为**，弹窗会打断用户登录后的操作。
//
//	⇒ 自启注册的命令行必须带 `--silent`，由 GUI 模式据此跳过 MessageBox。
//
// ⚠️ 用 `serve --silent` 而不是裸 `--silent`：
//
//	`serve` 是明确的子命令，语义更清楚，且与既有注册项兼容
//	（`Enable` 是幂等的：重新 Enable 会用新参数覆盖旧值，
//	 已装老版本的用户点一次"开机自启"即可升级成静默）。
var DefaultArgs = []string{"serve", FlagSilent}

// FlagSilent 是"静默启动"标志，与 internal/app.FlagSilent 必须一致。
//
// ⚠️ 刻意**不 import internal/app**：autostart 是底层工具包，
//
//	反向依赖 app 会造成不该有的耦合（且 app 会 import autostart）。
//	两边是同一个小字符串常量，用一个测试钉住一致性即可。
const FlagSilent = "--silent"

// ErrUnsupported 表示当前平台不支持注册表开机自启。
//
// 之所以导出：调用方（面板/设置 API）需要能把"平台不支持"和
// "写入失败（权限、注册表损坏）"区分开来，前者应提示"非 Windows 不支持"，
// 后者应提示具体的系统错误。
var ErrUnsupported = errors.New("autostart：开机自启仅在 Windows 上可用")

// Supported 报告当前平台是否支持开机自启。
//
// 【为什么单独提供这个函数，而不是让调用方自己判断 runtime.GOOS】：
//
//	平台能力的判断依据必须和实现放在一起。若调用方各自写
//	`if runtime.GOOS == "windows"`，将来实现换成计划任务、或支持了别的平台，
//	就得去改每一个调用点，且很容易漏。这里收口成一处。
//
// 有它面板才能做到"不支持时把开关显示为禁用"，而不是让用户点了才报错。
//
// 各平台的具体取值在 autostart_windows.go / autostart_other.go 里给出。
func Supported() bool { return supported }

// CommandLine 把可执行文件路径与参数拼成注册表 Run 项里的命令行。
//
// 这是本包【最易错】的一段逻辑，所以抽成纯函数单独测试：
//
//   - 路径【含空格时必须加双引号】。Windows 创建进程时先做命令行分词，
//     不加引号的 `C:\Program Files\wbapi\wbapi.exe serve` 会被拆成
//     `C:\Program` + `Files\wbapi\wbapi.exe` + `serve`，
//     结果是开机时静默启动失败（Run 键的失败没有任何界面提示，极难发现）。
//   - 路径【不含空格时也照样加引号】：一是统一行为便于断言，
//     二是路径里可能有 `&`、`^`、`(` 等在 cmd 语境下有特殊含义的字符，
//     加引号能一并规避；Windows 的 CreateProcess 对带引号路径处理是明确的。
//   - 参数里若本身含空格（例如 `--config C:\my dir\a.json`），同样需要引号；
//     这里用 quoteArg 处理，顺带把内嵌的双引号按 Windows 规则转义成 `\"`。
//   - 空参数会被丢弃（`serve ""` 这种空前缀没有意义，且会让下游解析出空 argv）。
func CommandLine(exePath string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteArg(exePath))
	for _, a := range args {
		if a == "" {
			continue
		}
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// quoteArg 对单个命令行片段做 Windows 风格的引号处理。
//
// 规则（与 Go 标准库 os/exec 在 Windows 上的做法一致）：
//
//   - 空串 → 返回 `""`（保持位置，避免参数错位）；
//   - 不含空格 / 制表符 / 双引号 → 原样返回（可读性更好）；
//   - 否则用双引号包裹，并把内部的双引号转义为 `\"`。
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	// 先转义内嵌引号，再整体加引号。
	// 反斜杠的处理在这里刻意【不做】：Run 键里写的是路径，
	// 以反斜杠结尾的路径在 Windows 上非法（API 层会拒绝），
	// 因此不需要 os/exec 那套"反斜杠翻倍"的完整算法。
	escaped := strings.ReplaceAll(s, `"`, `\"`)
	return `"` + escaped + `"`
}

// validateExePath 校验待注册的可执行文件路径。
//
// 为什么要在写入注册表【之前】校验，而不是写完就算了：
// Run 键里存的是不存在的路径时，开机只会静默失败（没有任何提示），
// 用户会以为"自启坏了"。宁可 Enable 直接返回错误，让它当场暴露。
func validateExePath(exePath string) error {
	if strings.TrimSpace(exePath) == "" {
		return fmt.Errorf("autostart：exe 路径为空")
	}
	if strings.ContainsRune(exePath, '"') {
		return fmt.Errorf("autostart：exe 路径含双引号，无法安全写入注册表: %s", exePath)
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════
// 自愈：exe 被移到别处后，自动修正注册表里的旧路径
// ═══════════════════════════════════════════════════════════════════

// HealOutcome 描述一次自愈检查的结果。
type HealOutcome int

const (
	// HealNotEnabled：值不存在 ⇒ 用户没启用自启（或主动关了）⇒ 不动。
	//
	// 🔴 这一条是**安全边界**：绝不能因为"发现没注册"就顺手替用户打开自启。
	//	用户关掉自启是明确意图，替它打开属于越权。
	HealNotEnabled HealOutcome = iota

	// HealUnchanged：已启用且路径与当前 exe 一致 ⇒ 不动。
	HealUnchanged

	// HealRewritten：已启用但路径指向别处（exe 被移动/改名）⇒ 已用当前路径重写。
	HealRewritten
)

// String 让日志与测试断言都能读到可读的结论。
func (h HealOutcome) String() string {
	switch h {
	case HealNotEnabled:
		return "未启用（不动）"
	case HealUnchanged:
		return "路径一致（不动）"
	case HealRewritten:
		return "路径已过期（已重写）"
	default:
		return fmt.Sprintf("未知(%d)", int(h))
	}
}

// ParseExePath 从注册表存的那条命令行里取出 exe 路径（第一个参数）。
//
// 存在的理由：注册表里存的是**整条命令行字符串**（路径 + 参数），
// 要判断"路径是否变了"就必须先把路径部分切出来。
//
// 规则（与 CommandLine 的写法对称）：
//   - 以双引号开头 → 取到下一个双引号为止（CommandLine 对含空格路径加引号）；
//   - 否则 → 取到第一个空白为止。
//
// 取不到时返回空串，调用方应视为"无法判定"而不是"需要重写"。
func ParseExePath(cmdLine string) string {
	s := strings.TrimLeft(cmdLine, " \t")
	if s == "" {
		return ""
	}
	if s[0] == '"' {
		rest := s[1:]
		if i := strings.IndexByte(rest, '"'); i >= 0 {
			return rest[:i]
		}
		// 引号没闭合：整条都是路径（畸形值，交给调用方当"无法判定"）
		return ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// SameExePath 比较两个路径是否指向同一个可执行文件。
//
// 为什么不用 filepath.Clean 后直接比：
//
//	本项目要在 windows 与 linux 双向编译（CI 检查），
//	而 filepath.Clean 在两个平台对分隔符的处理不同 ——
//	用平台相关的语义去比对"Windows 注册表里的路径"会得出不一致的结论。
//
// 这里采用**与平台无关**的保守比较：忽略大小写（Windows 路径不区分大小写）、
// 去掉尾部斜杠与首尾空白。宁可"判成一致"（少写一次注册表）也不误判。
func SameExePath(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimRight(s, `\/`)
		return s
	}
	return strings.EqualFold(norm(a), norm(b))
}

// NeedsHeal 判断一条已注册的命令行是否需要按当前 exe 路径重写。
//
// 纯函数（不碰注册表），因此可以在任何平台单测 —— 注册表读写必须门控，
// 但"该不该写"这个**决策**是本缺陷的核心，必须能无条件测到。
//
//   - cmdLine 为空（值不存在）→ false：用户没启用自启，不越权打开。
//   - 解析不出路径（畸形值）→ false：**不猜**，留给用户手动处理。
//   - 路径不同 → true。
func NeedsHeal(cmdLine, exePath string) bool {
	if strings.TrimSpace(cmdLine) == "" {
		return false
	}
	registered := ParseExePath(cmdLine)
	if registered == "" {
		return false
	}
	return !SameExePath(registered, exePath)
}

// IsTestBinary 判断一个 exe 路径是否属于 Go 的**临时测试二进制**。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么必须有这道闸（2026-10-08 我自己踩的坑，比原缺陷更糟）
// ═══════════════════════════════════════════════════════════════════
//
//	自愈的判据是 os.Executable()。而 `go test` 下它是
//	`...\Temp\go-build<random>\bNNN\<pkg>.test.exe` —— 一个**临时**路径。
//
//	于是 `go test ./internal/app/` 真的把用户注册表里的自启项
//	  从 `D:\tools\TAOAPI\taoapi.exe`
//	  改写成了 `C:\...\Temp\go-build...\app.test.exe`（**实测发生**，已手工恢复）。
//
//	后果极严重：开机自启指向一个**测试结束后就被删掉**的临时文件，
//	而 Run 键失效是**静默的** ⇒ 用户下次开机服务不会起来，且毫无线索。
//
//	⚠️ 教训：**自愈类逻辑必须限定在"真实运行"语境**。
//	  原缺陷是"该修没修"，这个坑是"**主动改坏了用户的注册表**"——
//	  方向相反，破坏力更大。所以判定要保守到"宁可少修"。
//
// 判定依据（三条任一命中即认为是测试二进制）：
//   - 以 `.test.exe` 结尾（Go 测试二进制的固定命名）；
//   - 路径里含 `go-build` 段（go 的临时构建目录）；
//   - 路径里含临时目录的 `\Temp\` 段。
func IsTestBinary(exePath string) bool {
	p := strings.TrimSpace(exePath)
	if p == "" {
		return false
	}
	lower := strings.ToLower(p)
	if strings.HasSuffix(lower, ".test.exe") {
		return true
	}
	if strings.Contains(lower, `\go-build`) || strings.Contains(lower, `/go-build`) {
		return true
	}
	if strings.Contains(lower, `\temp\`) || strings.Contains(lower, `/temp/`) {
		return true
	}
	return false
}
