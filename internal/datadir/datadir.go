// Package datadir 统一解析本程序的**数据目录**。
//
// ═══════════════════════════════════════════════════════════════════
// 为什么要有这个包
// ═══════════════════════════════════════════════════════════════════
//
//	同一段"数据目录在哪"的逻辑原先在 **4 个地方各写了一遍**
//	（`auth/persist.go`、`config/config.go`、`usage/event.go`、
//	`app/settings_helpers.go`）。四份实现必须**永远一致** —— 否则会
//	出现"账号落在 A、配置落在 B"这种极难排查的状态。
//
//	委托方 2026-10-07 要求改成 **exe 同目录**（照 wild-work 的布局），
//	四处改动的风险远大于收敛成一处。⇒ 建立本包作为唯一来源。
//
// ═══════════════════════════════════════════════════════════════════
// 目录布局（照 wild-work，委托方 2026-10-07 拍板）
// ═══════════════════════════════════════════════════════════════════
//
//	<exe 所在目录>/
//	├─ wbapi.exe
//	├─ config.json          设置（含密钥、端口、开关、映射表）
//	├─ auths/               账号凭据（加密存储）
//	│  └─ accounts.json
//	└─ data/
//	   ├─ events/           按日 JSONL 用量流水
//	   ├─ reports/          静态报告
//	   └─ logs/             运行日志
//
// ═══════════════════════════════════════════════════════════════════
// 解析优先级
// ═══════════════════════════════════════════════════════════════════
//
//  1. **环境变量 `WBAPI_DATA_DIR`** —— 最高优先，测试与多实例隔离靠它。
//     ⚠️ 它指定的是**根目录**，本包再派生出 auths/ 等子目录。
//  2. **exe 所在目录** —— 正常使用路径（双击 exe 就地在旁边建数据）。
//  3. 兜底：当前工作目录。
//
// ⚠️ 为什么不做"exe 目录不可写就回退到 ~/.wbapi"：
//
//	那种"智能回退"会让用户**不知道自己的数据到底在哪** ——
//	排查时最怕这个。宁可明确失败（写不进去就报错），
//	也不要静默把凭据放到别的盘。委托方已确认"不会有人把 exe
//	放共享目录"，所以按"就地读写"处理即可。
package datadir

import (
	"os"
	"path/filepath"
	"sync"
)

const (
	// EnvVar 是指定数据根目录的环境变量名。
	EnvVar = "WBAPI_DATA_DIR"

	// LegacyDirName 是改造前的数据目录名（`~/.wbapi`）。
	//
	// 保留它只为**一次性自动迁移**：首次运行发现旧目录有数据、
	// 而新位置没有时，把数据搬过来。迁移后不再读写旧目录。
	LegacyDirName = ".wbapi"
)

// 子目录 / 文件名（集中定义，避免各处硬编码字符串写歪）
const (
	AuthsSubdir   = "auths"
	DataSubdir    = "data"
	EventsSubdir  = "events"
	ReportsSubdir = "reports"
	LogsSubdir    = "logs"

	AccountsFile = "accounts.json"
	ConfigFile   = "config.json"
)

var (
	mu sync.Mutex
	// cacheKey 是上次解析时的"环境变量取值"。
	// 一旦取值变了（例如测试里 t.Setenv 换成另一个临时目录），
	// 就必须重新解析 —— 否则会拿到上一次的陈旧路径。
	cacheKey  string
	cacheRoot string
)

// Root 返回数据根目录（= exe 所在目录，或 WBAPI_DATA_DIR）。
//
// ⚠️ 结果**按环境变量取值缓存**，不是无条件缓存一次：
//
//	同一进程内若 `WBAPI_DATA_DIR` 变了（**测试常见**），必须重新解析 ——
//	否则第二个测试会拿到第一个测试的目录，表现为
//	"单跑通过、整包失败"的诡异干扰（我第一版用 sync.Once
//	无条件缓存，就踩了这个：两个 import 测试整包必红、单跑必绿）。
//
//	生产路径下环境变量不会中途变化，所以缓存依然有效
//	（保住"同一进程内结果稳定"这个性质）。
func Root() string {
	key := os.Getenv(EnvVar)
	mu.Lock()
	defer mu.Unlock()
	if cacheRoot != "" && cacheKey == key {
		return cacheRoot
	}
	cacheRoot = resolveRoot()
	cacheKey = key
	return cacheRoot
}

// resolveRoot 实现优先级解析（见包注释）。
func resolveRoot() string {
	// ① 环境变量最高优先
	if d := os.Getenv(EnvVar); d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			return abs
		}
		return d
	}

	// ② exe 所在目录
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}

	// ③ 兜底：当前工作目录
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// ConfigPath 返回配置文件路径（根目录下的 config.json）。
func ConfigPath() string {
	return filepath.Join(Root(), ConfigFile)
}

// AccountsPath 返回账号凭据文件路径（auths/accounts.json）。
func AccountsPath() string {
	return filepath.Join(Root(), AuthsSubdir, AccountsFile)
}

// AuthsDir 返回账号凭据目录。
func AuthsDir() string {
	return filepath.Join(Root(), AuthsSubdir)
}

// DataDir 返回数据目录（流水、报告、日志的父目录）。
func DataDir() string {
	return filepath.Join(Root(), DataSubdir)
}

// EventsDir 返回用量事件目录（data/events）。
func EventsDir() string {
	return filepath.Join(DataDir(), EventsSubdir)
}

// ReportsDir 返回报告目录（data/reports）。
func ReportsDir() string {
	return filepath.Join(DataDir(), ReportsSubdir)
}

// LogsDir 返回日志目录（data/logs）。
func LogsDir() string {
	return filepath.Join(DataDir(), LogsSubdir)
}

// LegacyRoot 返回改造前的数据目录（`~/.wbapi`）；取不到用户目录时返回空串。
func LegacyRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, LegacyDirName)
}

// ResetForTest 清掉缓存，让下一次 Root() 重新解析。
//
// ⚠️ 仅测试用：生产代码不该中途换数据目录。
// （正常测试不必调用 —— Root 已按环境变量取值自动失效；
//
//	本函数只用于"改了 exe 路径/工作目录"这类非常规场景。）
func ResetForTest() {
	mu.Lock()
	cacheRoot = ""
	cacheKey = ""
	mu.Unlock()
}
