// Package usage 定义用量事件并写入按日 JSONL。
//
// 设计要点（经 DSH × Codex 确认）：
//   - 明细写 JSONL 落盘，【不常驻内存】
//   - 查询时按需流式读取，聚合完即释放
//   - 上游未返回的字段写 null，【不用 0 冒充事实】
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/datadir"
	"workbuddy.local/workbuddy-api/internal/storage"
)

// appendLockTimeout 是获取跨进程写入锁的等待上限。
//
// 取 3 秒：正常竞争窗口只有毫秒级（一次对话写一行），
// 给足余量应对"另一个进程正在写一大批量"的极端情况；
// 超时则返回错误让调用方记日志 —— 宁可丢一条统计，不能卡住请求。
const appendLockTimeout = 3 * time.Second

// Event 是一次请求的用量记录。
//
// 分三层来源（见 docs/limits.md）：
//   - 协议层：客户端维度（账号/模型/协议/耗时/成败）
//   - provider 层：上游明确返回的额度（credit）
//   - 本层负责把两者合成一条脱敏记录
type Event struct {
	// Time 请求开始时间。
	Time time.Time `json:"time"`

	// Account 账号标识（脱敏后的短 ID）。
	Account string `json:"account"`

	// Model 对外模型 ID（含渠道前缀）。
	Model string `json:"model"`

	// ProviderID 实际处理该请求的渠道（如 "workbuddy"）。
	//
	// 🔴 为什么要单独记（Codex 第 40 轮指出，2026-10-06）：
	//
	//	此前只有 Model 一个字段，而它存的是**客户端写的原文**。
	//	于是"这个请求到底走了哪个渠道"只能靠**当前注册表回推** ——
	//	而注册表是会变的：
	//	  · 今天只有 WorkBuddy，裸 ID "glm-5.3" 能安全补成
	//	    "workbuddy/glm-5.3"；
	//	  · 明天接入国际版后若也有个裸 ID "glm-5.3"，
	//	    这条旧记录就**无法判定属于谁**了 —— 历史统计被改写。
	//
	//	⇒ 请求**当时就知道**答案（路由已经解析过了），必须当场记下来。
	//	  事后再猜，是在丢掉已知信息后再去伪造它。
	//
	// ⚠️ 老记录该字段为空（文件里没有这一列）—— 聚合时回退到旧逻辑。
	ProviderID string `json:"provider_id,omitempty"`

	// ResolvedModel 实际转发给上游的模型 ID（不含渠道前缀的裸 ID）。
	//
	// 与 ProviderID 配合，可精确还原"这次调用最终打到哪个模型的哪个渠道"，
	// 不再依赖别名表/注册表的当前状态。
	ResolvedModel string `json:"resolved_model,omitempty"`

	// Protocol 协议名（chat / responses / messages）。
	Protocol string `json:"protocol"`

	// Stream 客户端是否要求流式。
	Stream bool `json:"stream"`

	// OK 请求是否成功。
	OK bool `json:"ok"`

	// Error 失败原因（已脱敏，不含凭据）。
	Error string `json:"error,omitempty"`

	// DurationMS 总耗时（毫秒）。
	DurationMS int64 `json:"duration_ms"`

	// TTFTMS 首字耗时（time to first token，毫秒）。
	//
	// 🔴 为什么要单独记（2026-10-09 委托方要求「能不能获取到首字耗时」）：
	//
	//	总耗时对流式请求意义有限 —— 用户体感的是"多久开始出字"。
	//	「总 40 秒 + 首字 0.8 秒」与「总 40 秒 + 首字 20 秒」是
	//	完全不同的体验，只看总耗时分不出来。
	//
	// ⚠️ 用指针，语义有**三种**，不能混：
	//	nil = 不适用或没测到（非流式请求、或在拿到首字前就失败了）
	//	0   = 合法值（首字几乎与请求同时到）
	//	>0  = 正常测到
	//	若用值类型 + 0，就分不出"没测"与"确实 0ms"了。
	//
	// ⚠️ 只对**流式**有意义：非流式要收齐才返回，"首字"与"完成"是同一刻。
	//	所以非流式路径**不采集**它（留 nil），前端据此退回只显示总耗时。
	TTFTMS *int64 `json:"ttft_ms"`

	// PromptTokens 输入 token。
	PromptTokens int64 `json:"prompt_tokens"`

	// CompletionTokens 输出 token。
	CompletionTokens int64 `json:"completion_tokens"`

	// TotalTokens 合计。
	TotalTokens int64 `json:"total_tokens"`

	// ReasoningTokens 思考 token（实测字段）。
	ReasoningTokens int64 `json:"reasoning_tokens"`

	// CacheHitTokens 缓存命中（用于算命中率）。
	CacheHitTokens int64 `json:"cache_hit_tokens"`

	// CacheMissTokens 缓存未命中。
	CacheMissTokens int64 `json:"cache_miss_tokens"`

	// Credit 上游返回的额度消耗。
	//
	// ⚠️ 用指针：上游未返回时为 nil，【不能用 0 冒充】—— 0 表示"确实没消耗"。
	Credit *float64 `json:"credit"`

	// RequestID 关联同一次客户端请求的所有事件。
	//
	// 为什么要它（Codex 第 9 轮建议）：路径有分叉（流式/非流式/换号重试），
	// 没有这个字段时，将来做"重试不重复统计"或排查"一次请求为何两条记录"
	// 就得靠时间戳猜。有它在，聚合与去重都能精确到请求。
	RequestID string `json:"request_id,omitempty"`

	// Status 终态。取值见下面的 Status* 常量。
	//
	// 与 OK 的分工：OK 只回答"成功与否"，Status 说明"怎么结束的"。
	// 例如客户端主动断开是一种【非失败】的未完成，OK=false 但 Status 能区分
	// 它和上游报错 —— 否则成功率会被客户端行为污染。
	Status string `json:"status,omitempty"`

	// UsageKnown 表示 token 数是否来自上游的最终 usage。
	//
	// 🔴 关键语义（Codex 第 9 轮明确要求）：客户端中途断开时我们【拿不到】
	// 最终 usage，此时绝不能把已累计的中途分片当最终值写进去。
	// 这种情况写 usageKnown=false 且 token 字段保持 0，
	// 让查询方能把这些记录排除在 token 统计之外，而不是被假数据拉低平均值。
	UsageKnown bool `json:"usage_known"`
}

// Status 取值。
const (
	// StatusOK 正常完成并收到上游 usage。
	StatusOK = "ok"

	// StatusUpstreamError 上游返回错误或流异常中断。
	StatusUpstreamError = "upstream_error"

	// StatusClientDisconnected 客户端提前断开。
	//
	// 单独成一类：这不是服务端故障，不该计入失败率去告警。
	StatusClientDisconnected = "client_disconnected"

	// StatusNoUsage 请求成功（拿到了正文）但上游没给 usage。
	StatusNoUsage = "no_usage"

	// StatusInvalidRequest 请求在进入上游之前就被我们拒绝。
	StatusInvalidRequest = "invalid_request"
)

// Store 按日写入 JSONL。
type Store struct {
	dir string

	mu sync.Mutex
}

// NewStore 创建存储。dir 为空时使用默认目录。
func NewStore(dir string) *Store {
	if dir == "" {
		dir = defaultDir()
	}
	return &Store{dir: dir}
}

// Dir 返回存储目录。
func (s *Store) Dir() string { return s.dir }

// LockAvailable 报告当前平台是否支持跨进程写入锁。
//
// 🔴 为什么要暴露它（Codex 第 13 轮要求）：
//
//	非 Windows 平台上 storage.AcquireFileLock 返回 ErrLockUnsupported。
//	若只是让 Append 失败并被日志吞掉，就会表现为
//	**"usage 看着正常，但偶尔不写"** —— 这是最糟的故障形态：
//	用户以为在用统计功能，实际数据在悄悄丢。
//
//	把可用性做成可查询的，让 /api/stats 能明确显示
//	"usage 写入不可用（平台不支持文件锁）"，而不是继续显示 0。
func (s *Store) LockAvailable() bool {
	lk, err := storage.AcquireFileLock(storage.LockPathFor(s.dir), time.Second)
	if err != nil {
		return false
	}
	lk.Release()
	return true
}

// Append 追加一条事件。
//
// 按日分文件；写失败不应阻断请求，由调用方决定是否记录日志。
//
// 🔴 双重保护（Codex 第 11、12 轮两次要求）：
//
//  1. **进程内 mutex**（s.mu）—— 同进程多 goroutine。
//
//  2. **跨进程文件锁**（storage.AcquireFileLock）—— 重启期间可能
//     短暂出现父子两个进程同时写。
//
//     为什么不能只靠"一次 Write + O_APPEND"：
//     我实测过 6 进程 × 400 行 0 损坏，但那只能证明"当前写法在当前环境下"
//     没坏，**不构成长期契约** —— 覆盖不到文件轮转、stats 同时读取、
//     杀进程/断电、杀毒软件介入，以及将来有人改成 bufio 多次写。
//     所以老老实实加锁，并且让失败**可见**（超时返回错误由调用方记日志），
//     而不是假装成功。
//
// 锁的粒度是"单次 append"，不覆盖读取：读取端（stats）不加锁，
// 因为它读的是已经写完整的历史行，且流式解析会跳过末尾残缺行。
func (s *Store) Append(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("创建存储目录失败: %w", err)
	}

	path := filepath.Join(s.dir, fileName(ev.Time))

	// 跨进程锁：独立 .lock 文件，不锁 JSONL 本身（免得干扰读取端）。
	//
	// 🔴 锁路径用**数据目录级的固定名**，不含日期（Codex 第 13 轮要求）：
	//
	//	若锁按日生成（events/2026-10-05.jsonl.lock），跨日轮转时
	//	两个进程可能各自持有"不同日期"的锁而同时写入 —— 锁就白加了。
	//	固定一把覆盖整个存储目录的锁，才能保证"选文件 + 写"是串行的。
	lk, err := storage.AcquireFileLock(storage.LockPathFor(s.dir), appendLockTimeout)
	if err != nil {
		// 不静默降级：把错误如实返回，调用方会记日志；
		// 且 stats 会通过 LockAvailable() 明确报告 usage 是否可用。
		return fmt.Errorf("获取写入锁失败: %w", err)
	}
	defer lk.Release()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开事件文件失败: %w", err)
	}
	defer f.Close()

	// 先完整序列化（含换行），再一次性写出 —— 见上方注释，这是原子性的前提
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("序列化事件失败: %w", err)
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("写入事件失败: %w", err)
	}
	return nil
}

// ReadResult 报告一次读取的完整性。
//
// 🔴 为什么必须把"不完整"显式报出来（Codex 第 17 轮要求）：
//
//	`Read` 遇到坏行会 break（跳过残缺尾部），这是有意的 fail-closed 取舍。
//	但**如果调用方不知道发生过截断**，面板就会把"部分统计"当成"完整统计"
//	显示出来 —— 用户看到数字变小却不知道原因，比报错更误导。
//
//	所以顺带数据一起返回"是否完整 + 断在哪"，让上游能显式提示。
type ReadResult struct {
	// Truncated 表示至少有一个文件在解析中途中断（末尾残缺或中途坏行）。
	Truncated bool `json:"truncated"`

	// TruncatedAt 是第一个中断的文件名；未截断时为空。
	TruncatedAt string `json:"truncated_at,omitempty"`

	// Events 是成功解析出的事件条数。
	Events int `json:"events"`
}

// Read 流式读取指定天数内的事件并回调。
//
// 采用流式解析（逐行），【不把整个文件读进内存】——
// 这是"内存第一"要求的直接体现。
//
// 返回值报告完整性，见 ReadResult 的说明。
func (s *Store) Read(days int, fn func(Event) error) error {
	_, err := s.ReadResult(days, fn)
	return err
}

// ReadResult 与 Read 相同，但额外报告读取完整性。
//
// 需要区分"读到全部"与"读到一半"的调用方（例如 /api/stats）应当用这个。
func (s *Store) ReadResult(days int, fn func(Event) error) (ReadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res ReadResult

	if days <= 0 {
		days = 1
	}
	today := time.Now()
	for i := 0; i < days; i++ {
		day := today.AddDate(0, 0, -i)
		path := filepath.Join(s.dir, fileName(day))

		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 那天没有数据是正常的
			}
			return res, fmt.Errorf("打开 %s 失败: %w", path, err)
		}

		dec := json.NewDecoder(f)
		for dec.More() {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				// 末尾残缺或中途坏行 —— 停在坏行，保留已解析的部分。
				//
				// ⚠️ 这里 break 是**有意**的（不用 continue 跳过坏行）：
				// 跳过坏行继续读，会把乱序/错位的数据混进统计；
				// 停在坏行则至少保证"读到的都是真的"。
				// 但必须把这件事**报给调用方**，否则就是静默降级。
				res.Truncated = true
				if res.TruncatedAt == "" {
					res.TruncatedAt = fileName(day)
				}
				break
			}
			res.Events++
			if err := fn(ev); err != nil {
				_ = f.Close()
				return res, err
			}
		}
		_ = f.Close()
	}
	return res, nil
}

// fileName 返回某天的事件文件名。
func fileName(t time.Time) string {
	return t.Format("2006-01-02") + ".jsonl"
}

// defaultDir 返回默认存储目录（`data/events`）。
//
// 🔴 2026-10-07 改：位置由 `internal/datadir` 统一决定
// （exe 同目录下的 `data/events`，见该包注释）。
func defaultDir() string {
	return datadir.EventsDir()
}
