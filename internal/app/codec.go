package app

import (
	"runtime"

	"workbuddy.local/workbuddy-api/internal/storage"
)

// storageCodec 返回本平台的凭据加密实现。
//
// Windows → DPAPI（绑定当前用户，密钥不用自己管）。
// 其他平台 → 明确不支持的实现（编译也能过，但一用就报错，
// 不静默降级成明文）。
func storageCodec() storage.Codec {
	if runtime.GOOS == "windows" {
		return storage.DPAPICodec{Description: "wbapi 账号凭据"}
	}
	return storage.DPAPICodec{} // 非 Windows 上 Encrypt/Decrypt 返回明确错误
}
