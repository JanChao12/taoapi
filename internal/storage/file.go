// Package storage 提供加密原语与原子写盘。
//
// 设计要点（经 DSH × Codex 第 8 轮确认）：
//   - 写入用「临时文件 + rename」原子替换，避免进程中途退出留下半个 JSON
//   - DPAPI 失败【绝不】退回明文
//   - 文件权限 0600 只是额外防护，【不替代】加密
//
// 分层：本包只提供"加密"和"原子写"两个原语，<b>不</b>决定哪些字段该加密。
// 那个决定属于调用方（见 internal/auth），因为只有它知道
// 哪些字段是秘密、哪些要给面板明文读。
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Codec 负责把明文与密文互转。
//
// 抽象出来是为了：
//   - 非 Windows 平台能有明确的"不支持"实现，而不是静默存明文
//   - 测试可以注入确定性 codec
type Codec interface {
	// Encrypt 加密。失败必须返回错误，【不得】退回明文。
	Encrypt(plaintext []byte) ([]byte, error)

	// Decrypt 解密。
	Decrypt(ciphertext []byte) ([]byte, error)

	// Name 返回算法名，写入文件便于将来迁移。
	Name() string
}

// Envelope 是落盘的加密信封。
//
// 把算法名与版本写进文件，将来换算法时能识别旧文件并给出可操作的提示，
// 而不是笼统报"解密失败"。
type Envelope struct {
	// Version 信封格式版本。
	Version int `json:"version"`

	// Codec 加密算法名（如 "dpapi"）。
	Codec string `json:"codec"`

	// Payload 密文。
	Payload []byte `json:"payload"`
}

// EnvelopeVersion 当前信封版本。
const EnvelopeVersion = 1

// NewEnvelope 用 codec 加密并打包成信封。
//
// 🔴 加密失败时直接返回错误 —— 调用方【必须】放弃写入，
// 绝不能拿明文兜底。
func NewEnvelope(codec Codec, plaintext []byte) (*Envelope, error) {
	cipher, err := codec.Encrypt(plaintext)
	if err != nil {
		return nil, fmt.Errorf("加密失败（不会退回明文）: %w", err)
	}
	return &Envelope{
		Version: EnvelopeVersion,
		Codec:   codec.Name(),
		Payload: cipher,
	}, nil
}

// Open 校验信封并用 codec 解密。
func (e *Envelope) Open(codec Codec) ([]byte, error) {
	if e.Version != EnvelopeVersion {
		return nil, fmt.Errorf("信封版本 %d 不受支持（本程序支持 %d）",
			e.Version, EnvelopeVersion)
	}
	if e.Codec != codec.Name() {
		return nil, fmt.Errorf("数据用 %q 加密，当前 codec 是 %q；"+
			"换 Windows 用户或换机器后需重新导入", e.Codec, codec.Name())
	}
	plain, err := codec.Decrypt(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("解密失败: %w", err)
	}
	return plain, nil
}

// AtomicWrite 原子地把 data 写到 path。
//
// 步骤：写同目录临时文件 → Sync → rename。
// rename 在同分区上是原子的，所以任何时刻磁盘上要么是旧文件、要么是新文件，
// 【不会】出现半个 JSON（这正是"进程中途被杀"时的失败模式）。
//
// 调用方负责先做好加密；本函数只保证写入的原子性。
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	return atomicWriteWithBudget(path, data, perm, renameRetries, renameRetryDelay)
}

// atomicWriteWithBudget 与 AtomicWrite 相同，但可指定 rename 重试预算。
//
// 🔴 存在的唯一目的是**让"预算是否足够"可被实验证伪**（见 rename_budget_test.go）。
//
//	背景：全量测试偶发 `rename ...: Access is denied`，我先是**未能复现**
//	（因此没有证据、也没动参数）；后来用压力诊断复现并确认
//	**100% 是"目标被占用"类**。此时问题收敛为：
//	**是预算不足，还是本就不该靠重试？**
//
//	要回答它，必须能在**同一并发压力下对比不同预算**，
//	而不是凭直觉把 10 改成 50。所以把预算抽成参数 ——
//	生产入口 AtomicWrite 仍用编译期常量，行为完全不变。
func atomicWriteWithBudget(path string, data []byte, perm os.FileMode,
	retries int, delay time.Duration) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()

	// 失败路径统一清理临时文件
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}

	if err := os.Chmod(tmpName, perm); err != nil {
		// Windows 上 chmod 影响有限，不视为致命
		_ = err
	}

	// 🔴 rename 必须带重试（2026-10-05 实测发现的真实缺陷）。
	//
	// Windows 的 os.Rename 在目标路径**正被另一个句柄使用**时会失败，
	// 报 `Access is denied`。实测：8 个 goroutine 各 200 次并发替换同一文件，
	// **稳定出现若干次 Access is denied** —— 不是罕见竞态。
	//
	// 后果很隐蔽：调用方（config.Mutate）只会看到一个错误，
	// 而磁盘上仍是旧值 —— 用户以为自己改了设置，实际没生效。
	//
	// 解法：短暂退避后重试。rename 是很快的操作，且这里的并发压力来自
	// 同进程多 goroutine（真实场景几乎不存在持续争抢），
	// 几次重试足以覆盖。
	if err := renameWithRetryBudget(tmpName, path, retries, delay); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", filepath.Base(path), err)
	}
	tmpName = "" // rename 成功后不再清理
	return nil
}

// renameRetries / renameRetryDelay 控制 rename 的重试次数与间隔。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 为什么是 30 次 × 5ms（2026-10-05 实验定标，**不是拍的**）
// ═══════════════════════════════════════════════════════════════════
//
// 起因：全量测试偶发 `rename ...: Access is denied`，每次失败测试名不同、
// 单独跑必过。我**先没能复现**，因此当时没有证据、也没有动这四个数字
// （这是对的：无证据调参就是"看到异常数字先改代码"）。
//
// 后来加了压力诊断，终于复现并确认失败**100% 是"目标被占用"类**
// （其它错误 0 次）。于是问题收敛为：**是预算不足，还是不该靠重试？**
//
// 为回答它，把预算抽成参数（`atomicWriteWithBudget`），在**同一压力**
// （32 写者 × 600 次 = 19200 次原子写）下对比四档，**两轮结果一致**：
//
//	预算 10 次 × 5ms  ← 旧值 → 失败  7 / 19200  ❌
//	预算 30 次 × 5ms         → 失败  0 / 19200  ✅
//	预算 10 次 × 20ms        → 失败 12 / 19200  ❌ **比只加次数更差**
//	预算 50 次 × 20ms        → 失败  0 / 19200  ✅
//
// 🔑 **关键结论：加"间隔"没用、甚至更差；只有加"次数"有用。**
//
//	机理解释（与数据一致）：目标被占用的窗口**比单次 sleep 长**，
//	少而长的重试等于"探得更稀"，反而更容易错过窗口。
//	所以正确做法是**多探几次**，而不是每次等更久。
//
// 取值 30 次 × 5ms：最坏多等 150ms（旧值 50ms），
// 仍远小于任何网络/上游操作；而实测已把该压力下的失败降到 0。
//
// ⚠️ 注意这是**本机测试环境**的极端压力（32 个写者抢同一个文件）。
//
//	真实场景是"一次对话写一行 usage"，并发度远低于此 ——
//	所以这里的目标不是"消灭一切可能"，而是"让已知压力下不再失败"。
//	如需重新定标，跑：
//	  $env:WBAPI_RUN_RENAME_BUDGET="1"; go test -run TestRenameRetryBudgetAdequacy -v ./internal/storage/
const (
	renameRetries    = 30
	renameRetryDelay = 5 * time.Millisecond
)

// renameWithRetry 在 Windows 的瞬时占用下重试 rename。
//
// 只重试"目标被占用"这一类错误；其它错误（如磁盘满、路径不存在）
// 立即返回，避免把真实故障拖成慢失败。
//
// 时间上界：最坏 renameRetries × renameRetryDelay = 50ms，然后必定返回。
// 这保证了调用方（如 settings 写请求）不会因为抢不到文件而长时间无响应。
func renameWithRetry(from, to string) error {
	return renameWithRetryBudget(from, to, renameRetries, renameRetryDelay)
}

// renameWithRetryBudget 是 renameWithRetry 的可注入预算版本。
//
// 生产路径永远走 renameWithRetry（编译期常量）；本函数供实验对比不同预算，
// 见 atomicWriteWithBudget 的说明。
func renameWithRetryBudget(from, to string, retries int, delay time.Duration) error {
	var err error
	for i := 0; i < retries; i++ {
		err = renameFn(from, to)
		if err == nil {
			return nil
		}
		if !isRetryableRenameErr(err) {
			return err
		}
		time.Sleep(delay)
	}
	return err
}

// renameFn 是 os.Rename 的间接层，**仅供测试注入**。
//
// 🔴 注入点必须放在最底层（即 os.Rename 这一层），不能放在 renameWithRetry 之上。
// 原因：若替换整个 renameWithRetry，重试循环本身就不会被执行 ——
// 于是"重试 10 次才放弃"这条要求根本没被测到。
// 放在这里，被测的仍是【同一个】renameWithRetry 生产控制流。
var renameFn = os.Rename

// isRetryableRenameErr 判断 rename 错误是否值得重试。
//
// Windows 上"目标被占用"表现为 ERROR_ACCESS_DENIED(5) / ERROR_SHARING_VIOLATION(32)。
// Go 会把它包成 *os.LinkError，底层 errno 是 syscall.Errno。
//
// 🔴 2026-10-05 实测修正（此前这里的注释是错的）：
//
//	真实 Windows rename 占用错误的 Errno 是 **5**（ERROR_ACCESS_DENIED），
//	而 Go 的 `syscall.EACCES` 在 Windows 上是 **0x20000001 = 536870913**，
//	两者【不相等】：
//
//	    errors.Is(le.Err, syscall.EACCES) == false   ← 实测结果
//
//	也就是说，原先那行 `errors.Is(err, syscall.EACCES)` 在 Windows 上
//	永远不成立，真正让重试生效的是下面的【文本兜底】。
//	功能上没坏（文本兜底正好覆盖了真实错误文案"Access is denied."），
//	但这是"碰巧对"，不是一个可靠的判断 —— 一旦 Go 或系统改了文案就会静默失效。
//
//	因此这里改为直接比对真实的 Windows errno 值，文本兜底保留作为二道防线。
func isRetryableRenameErr(err error) bool {
	if err == nil {
		return false
	}

	var le *os.LinkError
	if errors.As(err, &le) {
		// 🔴 解包后必须再判一次 nil：LinkError 的 Err 字段可能为空，
		// 而 LinkError.Error() 会解引用它 → 直接 panic。
		// 实测：&os.LinkError{Op:"rename"} 调用 Error() 会崩。
		// 生产路径上这种情况罕见，但 panic 的代价远大于一行守卫。
		if le.Err == nil {
			return false
		}
		err = le.Err
	}

	// 先按跨平台的 syscall 常量判断（在类 Unix 上是正确的）。
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return true
	}

	// Windows 真实的 errno 值：ERROR_ACCESS_DENIED=5、ERROR_SHARING_VIOLATION=32。
	// 注意不能用 syscall.EACCES 代替 5 —— 见上面的实测说明。
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch uintptr(errno) {
		case 5, 32: // ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION
			return true
		}
	}

	// 文本兜底：Go 未必把所有 Windows 错误都映射到 syscall 常量。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "being used by another process") ||
		strings.Contains(msg, "sharing violation")
}

// ReadJSONFile 读取文件并 unmarshal；文件不存在时返回 (nil, false, nil)。
//
// 把"不存在"与"解析失败"分开，是让调用方能区分
// "首次运行"（正常）和"文件损坏"（要告警）。
func ReadJSONFile(path string, v any) (found bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("读取 %s 失败: %w", filepath.Base(path), err)
	}
	if err := unmarshalJSON(raw, v); err != nil {
		return true, fmt.Errorf("解析 %s 失败: %w", filepath.Base(path), err)
	}
	return true, nil
}

// unmarshalJSON 独立出来便于将来替换解析实现。
func unmarshalJSON(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
