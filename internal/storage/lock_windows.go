// lock_windows.go：跨进程文件锁（Windows）。
//
// 🔴 为什么需要（Codex 第 11、12 轮两次要求）：
//
//	用量明细写在按日 JSONL 里。正常情况下只有一个 wbapi 进程在写，
//	但**重启**期间可能短暂出现父子两个进程：
//	父进程还有在途的流式请求未结束，子进程已经开始服务。
//	两者同时 append 同一文件 → 行交错 / 重复 / 半行。
//
//	我做过实测（6 进程 × 400 行 × 300+ 字节，0 损坏），但那只能证明
//	"当前写法在当前环境下"没坏，**不能当成长期契约** ——
//	Codex 指出它覆盖不到：文件轮转、stats 同时读取、杀进程/断电、
//	杀毒软件介入、以及将来有人改成 bufio 多次写。所以加锁。
//
// 实现选择：`LockFileEx` 锁一个**独立的 .lock 文件**，
// 而不是锁定 JSONL 本身 ——
//   - 锁 JSONL 会干扰读取端（stats 页要读，读到一半被锁会失败）；
//   - 独立锁文件语义干净：它只用来串行化"写入这段临界区"。
//
// 用 syscall 直接调 kernel32，不用 cgo（与 DPAPI 同一风格），
// 保持"纯 Go 单 exe、离线可构建"。
package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// Windows 的 LockFileEx 常量。
const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	errLockViolation        = syscall.Errno(0x21) // 33: ERROR_LOCK_VIOLATION
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// FileLock 是一个跨进程的排他文件锁。
//
// 用法：
//
//	lk, err := AcquireFileLock(path, timeout)
//	if err != nil { /* 拿不到锁：调用方决定是等待还是放弃 */ }
//	defer lk.Release()
type FileLock struct {
	f    *os.File
	path string
}

// AcquireFileLock 以排他方式获取锁，最多等待 timeout。
//
// 用"重试 + 短睡眠"而不是阻塞式 LockFileEx：本工具写入频率极低，
// 重试间隔 5ms 带来的额外延迟可忽略，却换来了能实现超时语义。
// 阻塞式调用一旦持锁方卡死，我们也会跟着永久卡住。
func AcquireFileLock(path string, timeout time.Duration) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建锁目录失败: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if ok {
			return &FileLock{f: f, path: path}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("获取锁超时（%v）: %s", timeout, path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tryLock 尝试以非阻塞方式加排他锁。
func tryLock(f *os.File) (bool, error) {
	var ol syscall.Overlapped
	// 锁整个文件范围（前 8 字节足够表达长度：低 32 位 0xFFFFFFFF）
	r1, _, err := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		0xFFFFFFFF, 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 != 0 {
		return true, nil
	}
	// 被占用是正常情况，不算错误
	if errno, ok := err.(syscall.Errno); ok && errno == errLockViolation {
		return false, nil
	}
	// 其它错误（例如句柄无效）明确报出来
	if err == syscall.Errno(0) {
		// Call 在失败时若 LastError 为空，说明是拿不到锁而非真错误
		return false, nil
	}
	return false, fmt.Errorf("加锁失败: %v", err)
}

// Release 释放锁并关闭句柄。可安全重复调用。
func (l *FileLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	var ol syscall.Overlapped
	// 解锁失败不致命：关闭句柄时系统也会释放锁。
	_, _, _ = procUnlockFileEx.Call(
		l.f.Fd(), 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)),
	)
	_ = l.f.Close()
	l.f = nil
}

// LockPathFor 由被保护文件的路径推出锁文件路径。
//
// 约定：在同目录下加 `.lock` 后缀。用独立文件而不是锁原文件，
// 是为了不干扰读取端（见文件头注释）。
func LockPathFor(target string) string {
	return target + ".lock"
}
