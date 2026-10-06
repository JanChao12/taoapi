//go:build windows

package storage

import "syscall"

// 用 syscall.NewLazyDLL 而不是 golang.org/x/sys/windows：
// 保持【零第三方依赖】，符合"自持"要求（见 README）。
//
// ⚠️ NewLazyDLL（非 MustLoadDLL）：crypt32 在极少数精简系统上可能缺失，
// 此时应在调用时给出明确错误，而不是加载包时就 panic。
var (
	crypt32DLL      = syscall.NewLazyDLL("crypt32.dll")
	procEncryptData = crypt32DLL.NewProc("CryptProtectData")
	procDecryptData = crypt32DLL.NewProc("CryptUnprotectData")
	kernel32DLL     = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree   = kernel32DLL.NewProc("LocalFree")
)

// utf16Ptr 把 Go 字符串转为以 NUL 结尾的 UTF-16 指针。
//
// 用 syscall.UTF16PtrFromString：它已处理 NUL 结尾。
func utf16Ptr(s string) (*uint16, error) {
	if s == "" {
		return nil, syscall.EINVAL
	}
	return syscall.UTF16PtrFromString(s)
}
