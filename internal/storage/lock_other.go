//go:build !windows

// Package storage: 跨进程文件锁的非 Windows 占位实现。
//
// 与 dpapi_other.go 同一风格：**显式报错，不静默成功**。
// 静默返回一个"永远能拿到"的假锁比没有锁更危险 ——
// 调用方会以为自己受保护了。
package storage

import (
	"errors"
	"time"
)

// ErrLockUnsupported 表示当前平台没有实现跨进程文件锁。
var ErrLockUnsupported = errors.New("storage: 当前平台未实现跨进程文件锁")

// FileLock 是跨进程文件锁的占位类型。
type FileLock struct{}

// AcquireFileLock 在非 Windows 平台总是返回 ErrLockUnsupported。
//
// 不返回"假锁"：调用方必须能区分"拿到了锁"与"这个平台没有锁"，
// 否则会把无保护误当成有保护。
func AcquireFileLock(path string, timeout time.Duration) (*FileLock, error) {
	return nil, ErrLockUnsupported
}

// Release 占位实现（永远不会被拿到锁的调用方调用）。
func (l *FileLock) Release() {}

// LockPathFor 由被保护文件的路径推出锁文件路径（与 Windows 版一致）。
func LockPathFor(target string) string {
	return target + ".lock"
}
