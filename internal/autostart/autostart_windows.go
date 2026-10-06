//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// 用 syscall.NewLazyDLL 而不是 MustLoadDLL，也不用 golang.org/x/sys/windows：
//
//   - NewLazyDLL：advapi32.dll 在极少数精简/受限系统上可能不可用，
//     此时应该在【调用时】返回明确错误，而不是在包初始化时 panic ——
//     一个自启开关的功能不该把整个进程带崩。
//   - 不用 x/sys：本项目的硬约束是零第三方依赖（见 README 与维护备忘），
//     而注册表相关的 5 个 API 用 syscall 手写成本很低。
var (
	advapi32DLL = syscall.NewLazyDLL("advapi32.dll")

	// ⚠️ 这里【全部自己加载】，不混用 syscall.RegOpenKeyEx / syscall.RegQueryValueEx。
	//
	// syscall 包确实封装了其中几个（RegOpenKeyExW / RegQueryValueExW / RegCloseKey），
	// 但 RegSetValueExW 与 RegDeleteValueW 【没有】封装 —— 而这两个恰恰是
	// 写操作的核心。若一半用 syscall 封装、一半自己调，两条路径的错误处理
	// 风格会不一致（封装版返回 errno，自建版要自己判 LSTATUS），
	// 排查"到底哪一步失败"时会很别扭。统一走 callOK 一条路径更省心。
	procRegOpenKeyExW   = advapi32DLL.NewProc("RegOpenKeyExW")
	procRegSetValueExW  = advapi32DLL.NewProc("RegSetValueExW")
	procRegQueryValueEx = advapi32DLL.NewProc("RegQueryValueExW")
	procRegDeleteValueW = advapi32DLL.NewProc("RegDeleteValueW")
	procRegCloseKey     = advapi32DLL.NewProc("RegCloseKey")

	// 下面两个只被测试用于【枚举同键下的其他值名】，
	// 目的是断言测试没有误伤用户已有的开机项（见 autostart_windows_test.go）。
	// 放在非测试文件里是因为 _test.go 里的包级变量无法被同包的其他测试文件引用，
	// 且声明成本极低（LazyProc 不会真的加载 DLL，只有调用时才 LoadLibrary）。
	procRegQueryInfoKeyW = advapi32DLL.NewProc("RegQueryInfoKeyW")
	procRegEnumValueW    = advapi32DLL.NewProc("RegEnumValueW")
)

// Windows 常量：KEY_* 与 REG_SZ 用 syscall 已导出的，避免手抄数值。
//
// 只申请用到的权限：KEY_QUERY_VALUE / KEY_SET_VALUE 足够完成
// "读一项 / 写一项 / 删一项"，刻意【不用】KEY_ALL_ACCESS ——
// 最小权限原则，降低误伤同键下用户其他开机项的可能。
const (
	keyQueryValue = syscall.KEY_QUERY_VALUE // 0x0001
	keySetValue   = syscall.KEY_SET_VALUE   // 0x0002

	regSZ = syscall.REG_SZ // 1
)

// 🔴 hkeyCurrentUser 必须用 syscall.HKEY_CURRENT_USER，不能自己写 0x80000000。
//
// 这里踩过一次真实的坑（本机实测复现，回归测试见 TestHKEYCurrentUserConstant）：
//
//   - 注册表的 HKEY_* 是【伪句柄】，Win32 头文件里定义成
//     `((HKEY)(ULONG_PTR)((LONG)0x80000000))` —— 先当成【有符号】32 位数
//     （即 -2147483648），再扩宽成指针宽度。64 位进程里正确值是 0xFFFFFFFF80000000。
//   - 若按直觉写成 `const hkeyCurrentUser = 0x80000000`，Go 对无符号常量做
//     【零扩展】，得到 0x0000000080000000 —— 这是【非法伪句柄】，调用直接失败。
//   - 更迷惑的是失败方式（实测）：RegOpenKeyExW 返回 2，而 GetLastError 因未被
//     置位仍是残留值，于是错误文案显示为 "The operation completed successfully."
//     —— 字面上是"成功"，实际是失败。照字面排查会完全跑偏。
//     这也是 callOK 必须以【函数返回值】而不是 lastErr 判成败的原因。
//
// 另外 Go 的 syscall.HKEY_CURRENT_USER 实际值是 0x80000001，不是 0x80000000
// （见 types_windows.go 的 `HKEY_CLASSES_ROOT = 0x80000000 + iota`，
// CURRENT_USER 是第二项）—— 更说明这个值不该手抄，直接用标准库的。
var hkeyCurrentUser = uintptr(syscall.HKEY_CURRENT_USER)

// ERROR_FILE_NOT_FOUND：值不存在，属于"没启用"而不是"出错"。
//
// 用 syscall.ERROR_FILE_NOT_FOUND 而不是自己写 syscall.Errno(2)：
// 语义化且与 RegDeleteValueW/RegQueryValueExW 返回的 errno 可直接 errors.Is 比较。
const errorFileNotFound = syscall.ERROR_FILE_NOT_FOUND

// supported 表示本平台支持开机自启（Windows 上恒为 true）。
//
// 声明成常量而不是直接让 Supported() 返回 true，是为了让
// autostart.go 里的 Supported() 对两个平台保持同一份代码。
const supported = true

// callOK 调用一个 Win32 注册表过程，把返回的 LSTATUS 归一成 error。
//
// 🔴 这里【必须用第一个返回值（r1）作为错误码】，不能信 LazyProc.Call 的第三个
// 返回值（lastErr）。已实测（本机 Windows，64 位）：
//
//	RegDeleteValueW(不存在的值) → r1=2, r2=0, lastErr="The operation completed successfully."
//
// 原因是 Win32 注册表 API 返回的是 LSTATUS（函数返回值本身），
// 【不通过 SetLastError 报错】。于是 GetLastError 停留在上一次的残留值上，
// 通常是 0，syscall 把它格式化成 "The operation completed successfully."。
//
// 踩过的两个坑（都由本文件的测试抓出来，详见包内测试）：
//
//  1. 早期版本写成 `ret, _, callErr := proc.Call(...)` 后判 `ret != 0` 是对的，
//     但把 callErr 当错误内容返回 → 报出 "The operation completed successfully."
//     这种自相矛盾的文案，Disable 的幂等分支（比对 ERROR_FILE_NOT_FOUND）
//     也因此永远匹配不上，重复 Disable 反而报错 —— 幂等被破坏。
//  2. 反过来若只看 lastErr 判成败，则"值不存在"会被当成成功，
//     与"真的删掉了"无法区分（本包不依赖这个区分，但语义上仍是错的）。
//
// 因此：成功与否看 r1，错误内容也从 r1 取。
func callOK(proc *syscall.LazyProc, args ...uintptr) (syscall.Errno, error) {
	r1, _, _ := proc.Call(args...)
	if r1 == 0 {
		return 0, nil
	}
	errno := syscall.Errno(r1)
	return errno, errno
}

// Enable 注册开机自启。
//
// exePath 传【可执行文件的绝对路径】，args 传命令行参数（一般为 []string{"serve"}）。
//
// 幂等性说明：Run 键下同名值只会有一条 —— RegSetValueExW 对已存在的值名
// 是"覆盖"语义，所以连续调用两次不会产生重复项，第二次只是重写同样的数据。
// 这正是幂等成立的原因，不需要先查询再决定写不写。
//
// ⚠️ 只写 ValueName（wbapi）这一个值，不会触碰 Run 键下用户的其他项。
func Enable(exePath string, args []string) error {
	if err := validateExePath(exePath); err != nil {
		return err
	}

	cmdLine := CommandLine(exePath, args)

	key, err := openRunKey(keySetValue | keyQueryValue)
	if err != nil {
		return err
	}
	defer closeKey(key)

	return setStringValue(key, ValueName, cmdLine)
}

// Disable 取消开机自启。
//
// 幂等性说明：值本来就不存在（ERROR_FILE_NOT_FOUND）时【视为成功】——
// "让它变成没启用"这个目标已经达成，此时报错只会让调用方（面板按钮）
// 在重复点击时弹一个无意义的失败提示。
//
// ⚠️ 只删除 ValueName（wbapi）这一个值，绝不动 Run 键下的其他项。
func Disable() error {
	key, err := openRunKey(keySetValue | keyQueryValue)
	if err != nil {
		// 键都打不开（权限/注册表异常）—— 报告真实错误，不假装成功。
		return err
	}
	defer closeKey(key)

	namePtr, err := syscall.UTF16PtrFromString(ValueName)
	if err != nil {
		return fmt.Errorf("autostart：值名转换失败: %w", err)
	}

	errno, err := callOK(procRegDeleteValueW,
		uintptr(key),
		uintptr(unsafe.Pointer(namePtr)),
	)
	if err != nil {
		if errno == errorFileNotFound {
			// 本来就没有 —— 幂等，算成功。
			return nil
		}
		return fmt.Errorf("autostart：删除注册表值 %s 失败: %w", ValueName, err)
	}
	return nil
}

// Enabled 查询当前是否已启用。
//
// 【读注册表的实际值，不读配置文件】：配置文件可能与注册表不一致
// （用户手工删掉了注册表项、或用了系统优化工具），
// 此时面板显示"已开启"而实际不会自启，是很糟的体验。
// 以注册表为准是唯一不会骗人的做法。
//
// 返回值语义：
//   - (true, nil)  值存在且是 REG_SZ 的非空字符串
//   - (false, nil) 值不存在；或存在但不是 REG_SZ / 内容为空（视为无效项）
//   - (false, err) 打不开键、或读取失败 —— 与"没启用"区分开
func Enabled() (bool, error) {
	key, err := openRunKey(keyQueryValue)
	if err != nil {
		return false, err
	}
	defer closeKey(key)

	data, valType, err := queryStringValue(key, ValueName)
	if err != nil {
		if errors.Is(err, errorFileNotFound) {
			return false, nil // 值不存在 = 没启用
		}
		return false, err
	}
	if valType != regSZ {
		// 类型不对（比如别人手工塞了个 REG_DWORD 同名值）。
		// 不报错，按"未启用"处理，避免把面板卡在一个无法解释的错误上。
		return false, nil
	}
	return len(data) > 0, nil
}

// CommandLineFromRegistry 读回当前注册表里实际写入的命令行。
//
// 仅供诊断/测试使用（面板可以显示"当前注册的命令行是什么"）。
// 值不存在时返回 ("", nil)。
func CommandLineFromRegistry() (string, error) {
	key, err := openRunKey(keyQueryValue)
	if err != nil {
		return "", err
	}
	defer closeKey(key)

	data, valType, err := queryStringValue(key, ValueName)
	if err != nil {
		if errors.Is(err, errorFileNotFound) {
			return "", nil
		}
		return "", err
	}
	if valType != regSZ {
		return "", fmt.Errorf("autostart：注册表值 %s 类型不是 REG_SZ（实际 %d）", ValueName, valType)
	}
	return data, nil
}

// openRunKey 以 desiredAccess 权限打开 HKCU 下的 Run 键。
func openRunKey(desiredAccess uint32) (syscall.Handle, error) {
	subKeyPtr, err := syscall.UTF16PtrFromString(runKeyPath)
	if err != nil {
		return 0, fmt.Errorf("autostart：Run 键路径转换失败: %w", err)
	}

	var key syscall.Handle
	_, err = callOK(procRegOpenKeyExW,
		uintptr(hkeyCurrentUser),
		uintptr(unsafe.Pointer(subKeyPtr)),
		0, // ulOptions 必须为 0
		uintptr(desiredAccess),
		uintptr(unsafe.Pointer(&key)),
	)
	if err != nil {
		return 0, fmt.Errorf("autostart：打开 HKCU\\%s 失败: %w", runKeyPath, err)
	}
	return key, nil
}

// closeKey 关闭注册表句柄。忽略错误：句柄关闭失败无法补救，
// 也无法影响已经完成的读写结果。
func closeKey(key syscall.Handle) {
	procRegCloseKey.Call(uintptr(key))
}

// setStringValue 写入一个 REG_SZ 值。
//
// ⚠️ 长度必须是【字节数】，且要包含结尾的 NUL —— 这是 Win32 注册表 API
// 最常见的踩坑点：写成字符个数会把字符串截断。
func setStringValue(key syscall.Handle, name, value string) error {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("autostart：值名转换失败: %w", err)
	}
	valuePtr, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return fmt.Errorf("autostart：值内容转换失败: %w", err)
	}

	// UTF16PtrFromString 内部已按 UTF-16 编码，且字符串已以 NUL 结尾。
	byteLen := (len(value) + 1) * 2

	_, err = callOK(procRegSetValueExW,
		uintptr(key),
		uintptr(unsafe.Pointer(namePtr)),
		0, // Reserved 必须为 0
		uintptr(regSZ),
		uintptr(unsafe.Pointer(valuePtr)),
		uintptr(byteLen),
	)
	if err != nil {
		return fmt.Errorf("autostart：写注册表值 %s 失败: %w", name, err)
	}
	return nil
}

// queryStringValue 读取一个字符串值，返回内容与类型。
//
// 分两步：先问长度（buf 传 nil），再分配缓冲读取。
// 不用"猜一个足够大的固定缓冲"，因为命令行长度没有上限。
//
// 注意：RegQueryValueExW 返回的 cbData 是【字节数】，转成 UTF-16 字符数要除以 2。
func queryStringValue(key syscall.Handle, name string) (string, uint32, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", 0, fmt.Errorf("autostart：值名转换失败: %w", err)
	}

	var valType uint32
	var byteLen uint32

	// 第一次调用：buf = nil 只取长度和类型。
	_, err = callOK(procRegQueryValueEx,
		uintptr(key),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		uintptr(unsafe.Pointer(&valType)),
		0,
		uintptr(unsafe.Pointer(&byteLen)),
	)
	if err != nil {
		return "", 0, err
	}
	if byteLen == 0 {
		// 存在但为空值。
		return "", valType, nil
	}

	buf := make([]uint16, (byteLen+1)/2)
	_, err = callOK(procRegQueryValueEx,
		uintptr(key),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		uintptr(unsafe.Pointer(&valType)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&byteLen)),
	)
	if err != nil {
		return "", 0, err
	}

	return syscall.UTF16ToString(buf), valType, nil
}
