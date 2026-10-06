//go:build windows

package autostart

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// 本文件里的测试会【真实读写本机注册表】的 HKCU\...\Run 键。
//
// 安全设计（三条，缺一不可）：
//
//  1. 【只碰 ValueName(wbapi)】。所有写操作都经过 Enable/Disable，
//     它们内部写死的值名就是 wbapi。本文件里的辅助读函数虽然会枚举同键下
//     的全部值名，但那只是为了【断言没被改动】，绝不写入。
//  2. 【测试前先备份、测试后恢复】。snapshotRunKey 记录测试开始前 wbapi
//     是否存在及其原值；t.Cleanup 里把注册表恢复成原样
//     （原本没有 → 删掉；原本有 → 写回原值）。这样即使本机上真的已经
//     配好了 wbapi 自启项，跑测试也不会把它弄丢。
//  3. 【门控：默认跳过，需显式开启】（2026-10-05 反转，原因见下）
//     不设任何环境变量时，真实注册表测试【跳过】；
//     只有显式设置 WBAPI_RUN_REGISTRY_TESTS=1 才会真跑。
//     ⚠️ 引号拼接等纯逻辑测试【不受】此门控影响，永远运行。
//
// 【为什么从"默认跑"反转成"默认跳过"】
// 本机杀软（卡巴斯基）每次 go test 都会对 Temp\go-build*\autostart.test.exe
// 报 PDM:Trojan.Win32.Generic —— 误报的成因正是"测试进程在改启动项"这个
// 被准确观察到、但结论错了的行为。反复弹窗会淹没真正的告警。
// 反转后普通 go test 不再触发，需要验证注册表链路时再显式开启。
//
// 【代价，必须知道】默认不跑意味着"注册表写入链路"平时不被覆盖。
// 这是有意的取舍：本包拆成了两层 ——
//   - CommandLine / validateExePath / quoteArg 等【纯逻辑】永远跑（autostart_test.go），
//     它们覆盖的是"开机静默不启动"这类最难查的错；
//   - 真实注册表读写只在显式开启时跑。
// 因此【改动了 autostart_windows.go 的注册表相关代码后，必须跑一次】：
//     $env:WBAPI_RUN_REGISTRY_TESTS="1"; go test ./internal/autostart/
// 为了让"跳过"不是无声的，下面的 requireRegistry 会打印一行提示，
// 且 TestRegistrySuiteWasActuallyRun 会在跳过时明确报告本次未覆盖。
//
// 另外用互斥锁串行化：注册表是全局资源，t.Parallel 会让多个用例
// 互相覆盖同一个值，产生假失败。

// runEnv 是开启真实注册表测试的开关。值必须恰好是 "1"。
const runEnv = "WBAPI_RUN_REGISTRY_TESTS"

// legacySkipEnv 是反转前的旧开关，保留识别只为给出更准确的提示：
// 老文档/老习惯里写的 WBAPI_SKIP_REGISTRY_TESTS=1 现在【不再必要】
// （默认就是跳过），但也不该报错，只是提示一句。
const legacySkipEnv = "WBAPI_SKIP_REGISTRY_TESTS"

// registryMu 串行化所有真实注册表测试。
var registryMu sync.Mutex

// registrySuiteRan 记录本次进程里是否有真实注册表测试真的跑了，
// 供 TestMain 在【全部测试结束后】汇总。用 mutex 保护。
var (
	registrySuiteRan   bool
	registrySuiteRanMu sync.Mutex
)

// requireRegistry 判断是否允许跑真实注册表测试，并在需要时跳过。
func requireRegistry(t *testing.T) {
	t.Helper()

	// 旧开关先提示：用户可能照着旧文档设了它，以为能控制行为。
	if os.Getenv(legacySkipEnv) == "1" {
		t.Logf("提示：%s=1 已不再需要（现在默认就跳过）；"+
			"要跑真实注册表测试请改设 %s=1", legacySkipEnv, runEnv)
	}

	if os.Getenv(runEnv) != "1" {
		t.Skipf("默认跳过真实注册表测试（避免杀软对 autostart.test.exe 反复误报）；"+
			"要运行请设置 %s=1", runEnv)
	}

	registrySuiteRanMu.Lock()
	registrySuiteRan = true
	registrySuiteRanMu.Unlock()
}

// TestMain 在整个包的测试【全部结束后】汇总"注册表链路是否被覆盖"。
//
// 为什么不用一条普通测试来做这件事：Go 按源码顺序/名称顺序执行测试，
// 一条测试无法可靠地"看到"它之后的测试是否跑过。
// 早期版本用普通测试实现时，`-run` 过滤下真的出现过
// "TestEnableDisableRoundTrip 明明跑了，汇总却报未覆盖"的假阴性 ——
// 一个会撒谎的检查器比没有检查器更糟，所以改成在退出前统一汇报。
//
// 这条汇总【永远不让测试失败】：跳过是合法且默认的状态。它的唯一作用
// 是防止把"绿了"误读成"注册表链路验证过了"。
func TestMain(m *testing.M) {
	code := m.Run()

	registrySuiteRanMu.Lock()
	ran := registrySuiteRan
	registrySuiteRanMu.Unlock()

	if ran {
		fmt.Printf("[autostart] ✅ 本次已真实读写注册表：注册表链路被覆盖\n")
	} else {
		fmt.Printf("[autostart] ⚠️ 本次未真实读写注册表：注册表读写链路未被覆盖"+
			"（纯逻辑测试照常运行）。要完整验证请设置 %s=1\n", runEnv)
	}
	os.Exit(code)
}

// runKeySnapshot 记录测试前 wbapi 值的存在性与原值，用于事后恢复。
type runKeySnapshot struct {
	existed bool
	value   string
}

// otherValues 记录测试前同键下【除 wbapi 之外】的所有值名，
// 用于断言测试没有误伤用户的其他开机项。
type otherValues struct {
	names []string
}

// snapshotRunKey 备份 wbapi 的当前状态。
func snapshotRunKey(t *testing.T) runKeySnapshot {
	t.Helper()

	key, err := openRunKey(keyQueryValue)
	if err != nil {
		t.Fatalf("备份：打开 Run 键失败: %v", err)
	}
	defer closeKey(key)

	data, _, err := queryStringValue(key, ValueName)
	if err != nil {
		// 值不存在 —— 记录为"原本没有"。
		return runKeySnapshot{existed: false}
	}
	return runKeySnapshot{existed: true, value: data}
}

// restore 把 wbapi 恢复成备份时的状态。
func (s runKeySnapshot) restore(t *testing.T) {
	t.Helper()
	if err := Disable(); err != nil {
		t.Errorf("清理：Disable 失败: %v", err)
		return
	}
	if !s.existed {
		return // 原本就没有，删掉即已恢复。
	}
	// 原本存在：写回原值。
	key, err := openRunKey(keySetValue)
	if err != nil {
		t.Errorf("清理：写回原值前打开键失败: %v", err)
		return
	}
	defer closeKey(key)
	if err := setStringValue(key, ValueName, s.value); err != nil {
		t.Errorf("清理：写回原值失败: %v", err)
	}
}

// listValueNames 枚举 Run 键下的所有值名（只读，用于断言未误伤他人）。
func listValueNames(t *testing.T) []string {
	t.Helper()

	key, err := openRunKey(keyQueryValue)
	if err != nil {
		t.Fatalf("枚举：打开 Run 键失败: %v", err)
	}
	defer closeKey(key)

	// RegQueryInfoKey：先问有多少个值。
	var valueCount uint32
	ret, _, callErr := procRegQueryInfoKeyW.Call(
		uintptr(key),
		0, 0, 0,
		0, 0, 0,
		uintptr(unsafe.Pointer(&valueCount)),
		0, 0, 0, 0,
	)
	if ret != 0 {
		t.Fatalf("枚举：RegQueryInfoKeyW 失败: %v", callErr)
	}

	names := make([]string, 0, valueCount)
	buf := make([]uint16, 512) // 值名最长 16383 字符，512 对本用途足够
	for i := uint32(0); i < valueCount; i++ {
		nameLen := uint32(len(buf))
		ret, _, callErr := procRegEnumValueW.Call(
			uintptr(key),
			uintptr(i),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&nameLen)),
			0, 0, 0, 0,
		)
		if ret != 0 {
			t.Fatalf("枚举：RegEnumValueW(index=%d) 失败: %v", i, callErr)
		}
		names = append(names, syscall.UTF16ToString(buf[:nameLen]))
	}
	return names
}

// snapshotOtherValues 记录除 wbapi 外的值名（排序后便于比较）。
func snapshotOtherValues(t *testing.T) otherValues {
	t.Helper()
	var others []string
	for _, n := range listValueNames(t) {
		if n != ValueName {
			others = append(others, n)
		}
	}
	return otherValues{names: others}
}

// assertUnchanged 断言其他开机项一个不多一个不少。
//
// 这是本文件最重要的一条断言：测试【绝不允许】写坏用户本机已有的其他 Run 项。
func (o otherValues) assertUnchanged(t *testing.T) {
	t.Helper()
	got := make([]string, 0)
	for _, n := range listValueNames(t) {
		if n != ValueName {
			got = append(got, n)
		}
	}
	if len(got) != len(o.names) {
		t.Fatalf("其他 Run 项数量被改变（严重）：测试前 %d 项 %v，测试后 %d 项 %v",
			len(o.names), o.names, len(got), got)
	}
	for i := range got {
		if got[i] != o.names[i] {
			t.Fatalf("其他 Run 项被改变（严重）：测试前 %v，测试后 %v", o.names, got)
		}
	}
}

// setupRegistryTest 统一做：门控检查 + 加锁 + 备份 + 注册清理 + 记录其他项。
//
// 返回的 others 供测试末尾显式断言（虽然 t.Cleanup 也会恢复，
// 但显式断言能在"恢复动作本身出错"时更早暴露问题）。
func setupRegistryTest(t *testing.T) otherValues {
	t.Helper()
	requireRegistry(t)

	registryMu.Lock()
	t.Cleanup(registryMu.Unlock)

	snap := snapshotRunKey(t)
	others := snapshotOtherValues(t)

	t.Cleanup(func() {
		snap.restore(t)
		others.assertUnchanged(t)
	})

	return others
}

// testExePath 返回一个用于写入注册表的路径。
//
// 用 os.Executable()（即测试二进制自身的绝对路径）而不是假路径：
// 一来它确实存在，二来它通常位于带空格的 Temp 目录下，
// 顺带让"含空格路径"这条真实链路也被覆盖到。
func testExePath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径失败: %v", err)
	}
	if !strings.HasPrefix(exe, `C:\`) && !strings.HasPrefix(exe, `\\`) {
		t.Fatalf("测试二进制路径不是绝对 Windows 路径: %q", exe)
	}
	return exe
}

// TestEnableDisableRoundTrip 是契约要求的往返测试：
// Enable → Enabled()==true → Disable → Enabled()==false。
func TestEnableDisableRoundTrip(t *testing.T) {
	setupRegistryTest(t)
	exe := testExePath(t)

	// 起点强制为"未启用"，避免本机原有状态干扰断言。
	if err := Disable(); err != nil {
		t.Fatalf("预清理 Disable 失败: %v", err)
	}
	if on, err := Enabled(); err != nil || on {
		t.Fatalf("预清理后期望 (false,nil)，实际 (%v,%v)", on, err)
	}

	if err := Enable(exe, []string{"serve"}); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}

	on, err := Enabled()
	if err != nil {
		t.Fatalf("Enable 后 Enabled 报错: %v", err)
	}
	if !on {
		t.Fatal("Enable 后 Enabled() 应当为 true")
	}

	if err := Disable(); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}

	on, err = Enabled()
	if err != nil {
		t.Fatalf("Disable 后 Enabled 报错: %v", err)
	}
	if on {
		t.Fatal("Disable 后 Enabled() 应当为 false")
	}
}

// TestEnableWritesQuotedCommandLine 断言真正写进注册表的字符串是正确的命令行。
//
// 这条是引号逻辑的【端到端】验证：纯逻辑测试只证明 CommandLine 的返回值对，
// 这里证明它确实被原样写进了注册表（没有被截断、没有类型写错）。
func TestEnableWritesQuotedCommandLine(t *testing.T) {
	setupRegistryTest(t)

	// 造一个【含空格 + 含中文】的路径，直接验证引号在真实注册表里有效。
	exe := `D:\构建 反代\wbapi.exe`
	args := []string{"serve", "--port", "8787"}
	want := `"D:\构建 反代\wbapi.exe" serve --port 8787`

	if err := Enable(exe, args); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}

	got, err := CommandLineFromRegistry()
	if err != nil {
		t.Fatalf("CommandLineFromRegistry 失败: %v", err)
	}
	if got != want {
		t.Fatalf("注册表里的命令行不符\n  期望 %q\n  实际 %q", want, got)
	}
}

// TestEnablePathWithoutSpaces 覆盖"无空格路径不额外加引号"的真实写入。
func TestEnablePathWithoutSpaces(t *testing.T) {
	setupRegistryTest(t)

	exe := `C:\wbapi\wbapi.exe`
	if err := Enable(exe, []string{"serve"}); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}
	got, err := CommandLineFromRegistry()
	if err != nil {
		t.Fatalf("CommandLineFromRegistry 失败: %v", err)
	}
	if want := `C:\wbapi\wbapi.exe serve`; got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
}

// TestEnableIsIdempotent 幂等测试：连续两次 Enable 不报错，且注册表里只有一项。
//
// "只有一项"这个断言在 Run 键里等价于"值名 wbapi 唯一"——
// 注册表的值名天然唯一，所以真正要防的是【有人改成往值名里带序号/时间戳】
// 这种"看起来能去重、实际会越写越多"的实现。
// 这里通过"总数在两次 Enable 之间不增加"来守住。
func TestEnableIsIdempotent(t *testing.T) {
	setupRegistryTest(t)
	exe := testExePath(t)

	if err := Enable(exe, []string{"serve"}); err != nil {
		t.Fatalf("第一次 Enable 失败: %v", err)
	}
	firstCmd, err := CommandLineFromRegistry()
	if err != nil {
		t.Fatalf("第一次读取失败: %v", err)
	}
	firstTotal := len(listValueNames(t))

	if err := Enable(exe, []string{"serve"}); err != nil {
		t.Fatalf("第二次 Enable 失败（幂等要求不报错）: %v", err)
	}

	// 值名总数不得增加。
	if secondTotal := len(listValueNames(t)); secondTotal != firstTotal {
		t.Fatalf("重复 Enable 后 Run 值名总数从 %d 变成 %d —— 幂等被破坏",
			firstTotal, secondTotal)
	}

	secondCmd, err := CommandLineFromRegistry()
	if err != nil {
		t.Fatalf("第二次读取失败: %v", err)
	}
	if secondCmd != firstCmd {
		t.Fatalf("重复 Enable 写入了不同的内容:\n  第一次 %q\n  第二次 %q", firstCmd, secondCmd)
	}

	on, err := Enabled()
	if err != nil || !on {
		t.Fatalf("重复 Enable 后期望 (true,nil)，实际 (%v,%v)", on, err)
	}
}

// TestDisableIsIdempotent 幂等测试：连续两次 Disable 都不报错。
//
// 两个子场景都要覆盖：
//   - 第一次真的删掉了某项，第二次是"本来就没有"；
//   - 从头到尾都没有（从没 Enable 过就直接 Disable）。
func TestDisableIsIdempotent(t *testing.T) {
	setupRegistryTest(t)

	t.Run("先启用再连续两次 Disable", func(t *testing.T) {
		if err := Enable(testExePath(t), []string{"serve"}); err != nil {
			t.Fatalf("Enable 失败: %v", err)
		}
		if err := Disable(); err != nil {
			t.Fatalf("第一次 Disable 失败: %v", err)
		}
		if err := Disable(); err != nil {
			t.Fatalf("第二次 Disable 失败（幂等要求不报错）: %v", err)
		}
		on, err := Enabled()
		if err != nil || on {
			t.Fatalf("期望 (false,nil)，实际 (%v,%v)", on, err)
		}
	})

	t.Run("从未启用时 Disable 也算成功", func(t *testing.T) {
		if err := Disable(); err != nil { // 先确保干净
			t.Fatalf("预清理 Disable 失败: %v", err)
		}
		if err := Disable(); err != nil {
			t.Fatalf("对不存在的项 Disable 应当成功（幂等），实际报错: %v", err)
		}
		on, err := Enabled()
		if err != nil || on {
			t.Fatalf("期望 (false,nil)，实际 (%v,%v)", on, err)
		}
	})
}

// TestEnabledReadsRegistryNotConfig 验证 Enabled 反映的是注册表真实状态。
//
// 做法：绕过本包的 API，直接用 syscall 【手工】写一个 wbapi 值，
// 再看 Enabled() 是否认。如果实现退化成"读某个内存/配置缓存"，这条会失败。
func TestEnabledReadsRegistryNotConfig(t *testing.T) {
	setupRegistryTest(t)

	if err := Disable(); err != nil {
		t.Fatalf("预清理失败: %v", err)
	}
	if on, _ := Enabled(); on {
		t.Fatal("预清理后应当为 false")
	}

	// 手工写一个值（模拟"别的工具/旧版本写进去的自启项"）。
	key, err := openRunKey(keySetValue)
	if err != nil {
		t.Fatalf("打开键失败: %v", err)
	}
	if err := setStringValue(key, ValueName, `C:\somewhere\else\wbapi.exe serve`); err != nil {
		closeKey(key)
		t.Fatalf("手工写值失败: %v", err)
	}
	closeKey(key)

	on, err := Enabled()
	if err != nil {
		t.Fatalf("Enabled 报错: %v", err)
	}
	if !on {
		t.Fatal("注册表里已存在 wbapi 值，Enabled() 应当为 true")
	}

	// 再手工删掉，Enabled 应当立刻变 false。
	if err := Disable(); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}
	if on, _ := Enabled(); on {
		t.Fatal("删掉后 Enabled() 应当为 false")
	}
}

// TestEnabledTreatsEmptyValueAsDisabled 覆盖"值存在但为空"的边界。
//
// 空命令行没有任何意义（Windows 会尝试执行空命令并失败），
// 按"未启用"处理比返回 true 更符合事实，也避免面板显示一个点了没反应的开关。
func TestEnabledTreatsEmptyValueAsDisabled(t *testing.T) {
	setupRegistryTest(t)

	key, err := openRunKey(keySetValue)
	if err != nil {
		t.Fatalf("打开键失败: %v", err)
	}
	if err := setStringValue(key, ValueName, ""); err != nil {
		closeKey(key)
		t.Fatalf("写入空值失败: %v", err)
	}
	closeKey(key)

	on, err := Enabled()
	if err != nil {
		t.Fatalf("Enabled 对空值报错了（应当返回 false,nil）: %v", err)
	}
	if on {
		t.Fatal("空值应当被视为未启用")
	}
}

// TestEnableRejectsBadPath 确认非法路径在【写注册表之前】就被挡住。
//
// 断言注册表确实没被写：否则"报错但已经写进去了"会让用户面对一个
// 面板说失败、实际开机却会启动的诡异状态。
func TestEnableRejectsBadPath(t *testing.T) {
	setupRegistryTest(t)

	if err := Disable(); err != nil {
		t.Fatalf("预清理失败: %v", err)
	}

	for _, bad := range []string{"", "   ", `C:\a"b\wbapi.exe`} {
		if err := Enable(bad, []string{"serve"}); err == nil {
			t.Errorf("Enable(%q) 应当报错", bad)
		}
	}

	if _, err := CommandLineFromRegistry(); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	on, err := Enabled()
	if err != nil {
		t.Fatalf("Enabled 报错: %v", err)
	}
	if on {
		t.Fatal("非法路径被拒绝后，注册表里不应留下任何值")
	}
}

// TestRegistryTestsDoNotTouchOtherValues 是"不误伤"红线的显式守卫。
//
// 做法：在测试前后对比除 wbapi 外的全部值名，必须完全一致。
// 本机 Run 键下通常有 OneDrive / 输入法 / 其他自启软件等条目，
// 这条测试保证整套 Enable/Disable 流程只动自己那一个值。
func TestRegistryTestsDoNotTouchOtherValues(t *testing.T) {
	others := setupRegistryTest(t)
	exe := testExePath(t)

	// 走一遍完整生命周期。
	if err := Enable(exe, DefaultArgs); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}

	others.assertUnchanged(t)
}

// TestValueNameIsStable 守护值名常量。
//
// 值名就是本程序在开机自启列表里的身份：一旦改动，
// 老版本写下的项将无法被新版本的 Disable 清理，
// 用户会看到"关掉了但下次开机还在"。所以这个值必须是稳定的常量。
func TestValueNameIsStable(t *testing.T) {
	if ValueName != "wbapi" {
		t.Fatalf("值名被改动：期望 %q，实际 %q（改动会导致旧版本注册项无法清理）",
			"wbapi", ValueName)
	}
}

// TestHKEYCurrentUserConstant 守护 HKEY_CURRENT_USER 的取值方式。
//
// 回归测试：曾经写成 `const hkeyCurrentUser = 0x80000000`，
// 在 64 位进程里被零扩展成 0x0000000080000000（非法伪句柄），
// 导致所有注册表调用失败，且错误信息还是误导性的
// "The operation completed successfully."。
//
// 这里断言它与 syscall 包里的值一致 —— 只要有人手抄常量就会失败。
func TestHKEYCurrentUserConstant(t *testing.T) {
	if want := uintptr(syscall.HKEY_CURRENT_USER); hkeyCurrentUser != want {
		t.Fatalf("HKEY_CURRENT_USER 取值错误：0x%016X，应当直接用 syscall.HKEY_CURRENT_USER = 0x%016X",
			uint64(hkeyCurrentUser), uint64(want))
	}
}

// TestRunKeyPathIsCurrentUserRun 守护注册表路径。
//
// 必须是 HKCU（当前用户），不能是 HKLM —— 写 HKLM 需要管理员权限，
// 会让"普通用户双击 exe 就能用"的定位失效。
func TestRunKeyPathIsCurrentUserRun(t *testing.T) {
	const want = `Software\Microsoft\Windows\CurrentVersion\Run`
	if runKeyPath != want {
		t.Fatalf("Run 键路径被改动：期望 %q，实际 %q", want, runKeyPath)
	}
}

// TestUnsupportedErrorIsExported 确认非 Windows 用的哨兵错误已被导出。
//
// 这条在 Windows 上只是编译期/存在性检查，真正使用它的是 autostart_other.go。
func TestUnsupportedErrorIsExported(t *testing.T) {
	if ErrUnsupported == nil {
		t.Fatal("ErrUnsupported 不应为 nil")
	}
	if !strings.Contains(ErrUnsupported.Error(), "Windows") {
		t.Errorf("ErrUnsupported 的文案应说明只支持 Windows，实际: %q", ErrUnsupported.Error())
	}
}
