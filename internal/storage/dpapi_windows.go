//go:build windows

// Package storage: DPAPI 加密实现（Windows）。
//
// 用 CryptProtectData / CryptUnprotectData 通过 syscall 直接调用，
// 【不用 cgo】—— 保持纯 Go 静态编译，单 exe 无外部依赖。
//
// 🔴 关键约束：加密失败时【绝不】退回明文（见 file.go 的 Save 错误处理）。
package storage

import (
	"fmt"
	"syscall"
	"unsafe"
)

// dataBlob 对应 Windows 的 DATA_BLOB 结构。
type dataBlob struct {
	cbData uint32
	pbData *byte
}

// DPAPICodec 用 Windows DPAPI 加密。
//
// 特性：
//   - 密钥绑定【当前 Windows 用户】，不需自己管理密钥
//   - 换 Windows 用户或换机器后【无法解密】（设计意图，不是缺陷）
//   - 因此计划任务必须用同一用户运行
type DPAPICodec struct {
	// Description 写入密文的描述串（便于排查）。
	Description string

	// Entropy 可选的附加熵；为空则不用。
	//
	// 加熵能防止同机其他程序用裸 DPAPI 解开密文，
	// 但熵本身又要存下来 —— 对自用工具收益有限，默认不用。
	Entropy []byte
}

// Name 实现 Codec。
func (c DPAPICodec) Name() string { return "dpapi" }

// Encrypt 实现 Codec。
func (c DPAPICodec) Encrypt(plaintext []byte) ([]byte, error) {
	return dpapiCall(procEncryptData, plaintext, c.Entropy, c.Description, "加密")
}

// Decrypt 实现 Codec。
func (c DPAPICodec) Decrypt(ciphertext []byte) ([]byte, error) {
	return dpapiCall(procDecryptData, ciphertext, c.Entropy, "", "解密")
}

// dpapiCall 统一处理 CryptProtectData / CryptUnprotectData。
//
// 两个函数签名完全一致，只差输入是明文还是密文，
// 合并实现可避免复制粘贴出错。
func dpapiCall(proc *syscall.LazyProc, input, entropy []byte, desc, op string) ([]byte, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("DPAPI %s：输入为空", op)
	}

	in := dataBlob{cbData: uint32(len(input)), pbData: &input[0]}

	var entropyPtr *dataBlob
	if len(entropy) > 0 {
		ev := dataBlob{cbData: uint32(len(entropy)), pbData: &entropy[0]}
		entropyPtr = &ev
	}

	var descPtr *uint16
	if desc != "" {
		d, err := utf16Ptr(desc)
		if err != nil {
			return nil, fmt.Errorf("DPAPI %s：描述串转换失败: %w", op, err)
		}
		descPtr = d
	}

	var out dataBlob

	// CryptProtectData(pDataIn, szDataDescr, pOptionalEntropy, pvReserved,
	//                  pPromptStruct, dwFlags, pDataOut)
	ret, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(&in)),
		uintptr(unsafe.Pointer(descPtr)),
		uintptr(unsafe.Pointer(entropyPtr)),
		0, // pvReserved 必须为 NULL
		0, // pPromptStruct 必须为 NULL
		0, // dwFlags
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("DPAPI %s 调用失败: %w"+
			"（若换了 Windows 用户或换机器，旧凭据无法解密，需重新导入）", op, callErr)
	}

	// ⚠️ 返回的内存必须 LocalFree，不归 Go GC 管
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))

	if out.pbData == nil || out.cbData == 0 {
		return nil, fmt.Errorf("DPAPI %s 返回空结果", op)
	}

	result := make([]byte, out.cbData)
	copy(result, unsafe.Slice(out.pbData, out.cbData))
	return result, nil
}
