// Package workbuddy —— 本文件放 client.go 依赖的小工具函数。
package workbuddy

import (
	"crypto/rand"
	"io"
)

// cryptoRead 读取密码学安全随机字节。
//
// 独立成函数是为了让测试可以替换（虽然目前没用到），
// 并让 client.go 的 newMessageID 保持简短。
func cryptoRead(b []byte) (int, error) {
	return io.ReadFull(rand.Reader, b)
}
