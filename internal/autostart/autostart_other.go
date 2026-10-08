//go:build !windows

package autostart

// 非 Windows 平台的占位实现。
//
// 🔴 与 storage/dpapi_other.go 同一取舍：【故意】不提供任何"可用"的替代实现
// （比如写 ~/.config/autostart/*.desktop）。理由：
//
//   - 本项目只面向 Windows 自用，跨平台自启实现无法在本机验证，
//     写出来就是"没测过的代码"，等于把 bug 藏在最不容易发现的地方。
//   - 更糟的是【静默降级】：面板显示"已开启自启"，实际什么都没做，
//     用户下次开机发现服务没起来，还得回头怀疑是程序本身的问题。
//   - 明确失败只花一次排查成本，静默成功会花很多次。
//
// 因此这里所有写操作都返回 ErrUnsupported，Enabled 返回 (false, ErrUnsupported)。

// ErrUnsupported 的具体用法见 autostart.go 的说明。

// supported 表示本平台不支持开机自启。
//
// 用常量而非让 Supported() 在各平台各写一份，是为了保证
// "Supported() == false" 与"所有操作都返回 ErrUnsupported"
// 这两个事实由同一处代码决定，不会出现"说支持但一调用就报错"的不一致。
const supported = false

// Enable 在非 Windows 平台直接失败。
func Enable(exePath string, args []string) error {
	_ = exePath
	_ = args
	return ErrUnsupported
}

// Disable 在非 Windows 平台直接失败。
//
// 这里【不】做成"返回 nil"：调用方若在校验返回值的分支里写
// `if err := Disable(); err != nil {...}`，返回 nil 会让它以为清理成功。
func Disable() error {
	return ErrUnsupported
}

// Enabled 在非 Windows 平台无法查询，返回明确错误而不是 false。
func Enabled() (bool, error) {
	return false, ErrUnsupported
}

// CommandLineFromRegistry 在非 Windows 平台没有注册表可读。
func CommandLineFromRegistry() (string, error) {
	return "", ErrUnsupported
}

// Heal 在非 Windows 平台没有注册表可自愈。
//
// 🔴 刻意**不**返回 (HealUnchanged, nil)：
//
//	那会让调用方以为"检查过了、没问题"。这里必须让"做不到"这件事
//	带着 ErrUnsupported 显式暴露出来，与 Enable/Disable/Enabled 同一取舍
//	（见本文件顶部关于"静默降级"的说明）。
func Heal(exePath string) (HealOutcome, error) {
	_ = exePath
	return HealUnchanged, ErrUnsupported
}
