//go:build !windows

package storage

import "errors"

// ErrDPAPIUnsupported 非 Windows 平台没有 DPAPI。
//
// 🔴 这里【故意】不提供一个可用的替代实现（如明文或固定密钥）。
// 理由：本项目只面向 Windows 自用；在别的平台上静默降级成弱加密，
// 会让人误以为凭据是安全的。宁可明确失败。
var ErrDPAPIUnsupported = errors.New(
	"DPAPI 仅在 Windows 上可用；本项目不支持其他平台存储凭据")

// DPAPICodec 在非 Windows 平台上是不可用的占位实现。
type DPAPICodec struct {
	Description string
	Entropy     []byte
}

// Name 实现 Codec。
func (c DPAPICodec) Name() string { return "dpapi" }

// Encrypt 实现 Codec：非 Windows 直接失败。
func (c DPAPICodec) Encrypt([]byte) ([]byte, error) { return nil, ErrDPAPIUnsupported }

// Decrypt 实现 Codec：非 Windows 直接失败。
func (c DPAPICodec) Decrypt([]byte) ([]byte, error) { return nil, ErrDPAPIUnsupported }
