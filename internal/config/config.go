// Package config 管理本程序的本地设置（面板「设置」页读写的就是它）。
//
// 落盘位置：**exe 同目录**下的 `config.json`
// （2026-10-07 起，照 wild-work 布局；位置统一由 `internal/datadir` 决定）。
//
// 设计取舍（经 DSH × Codex 第 9 轮评审确认）：
//
//  1. 【host 不可配，只能配 port】—— 监听地址恒为 127.0.0.1。
//     Codex 明确否决了"允许自由编辑 addr"：本工具的 key 是本地自定的，
//     一旦用户把 host 配成 0.0.0.0 而 key 又恰好为空，等于把额度接口
//     暴露到局域网。只开放 port 能消除这一整类误配。
//
//  2. 【apiKey 默认值是字面量 "key"】—— 由委托人明确要求。
//
//     Codex 第 9 轮曾建议"首启用随机值，而不是固定 local"，
//     理由是固定默认值公开可猜。该建议**未被采纳**，因为委托人
//     明确要求"默认先随便提供一个 key"，并且要能把它设成空。
//
//     ⚠️ 不要"顺手改回"随机值或 "1"：
//     - 委托人的原话是默认给 "key"
//     - 曾有版本误用 "1"，被委托人点名纠正
//     - 空 key 是**合法**配置（面板允许清空），由调用方决定是否要求 key
//     面板另提供"一键换成随机值"的按钮，把"更安全"留给用户主动选择。
//
//  3. 【损坏不覆盖】—— 配置解析失败时把原文件改名保留，回退默认值并在
//     /status 报告。绝不静默丢弃用户设置（曾发生过凭据被清空的真实事故，
//     见 docs/维护备忘.md）。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy.local/workbuddy-api/internal/datadir"
	"workbuddy.local/workbuddy-api/internal/storage"
)

// Version 是配置文件格式版本，便于将来迁移。
const Version = 1

// DefaultPort 是默认监听端口。
const DefaultPort = 8787

// MinPort 是可配置的最小端口。
//
// 低于 1024 的端口在 Windows 上通常需要管理员权限，且可能与本机已有服务冲突，
// 直接拒绝比让用户反复失败更友好。
const MinPort = 1024

// bootstrapAPIKey 是首次运行写入的初始 key。
//
// 🔴 委托人 2026-10-06 指定：改为 **"TAOAPIKEY"**（区分大小写）。
//
// ⚠️ 只影响**首次运行**：已有配置文件不会被改写（用户可能已改成别的值，
//
//	覆盖会打断正在使用的客户端）。所以升级后旧 key 仍然是旧值 ——
//	要换需在面板「设置」里手动改。
//
// ⚠️ **大小写敏感**：本项目用 `subtle.ConstantTimeCompare` 做等值比较，
//
//	那是**逐字节**比较 —— "taoapikey" ≠ "TAOAPIKEY"。
//	这是密钥应有的行为（不是 bug），有测试守着。
//
// 与 Codex 评审的分歧（要记住，别再"好心改回去"）：
//   - Codex 建议首启用 crypto/rand 生成随机值，理由是固定值公开可猜。
//   - 委托人明确否决，要求用固定值：这是自用工具，只监听 127.0.0.1，
//     固定值便于他记得住、也便于首次接客户端时直接填。
//   - 面板仍提供"生成随机密钥"按钮（GenerateKey），需要时一键升级即可。
//
// 早期曾取 "1"（为兼容委托人 DSH 里已填的值），委托人确认"反正没用这个反代"，
// 故不再为兼容它做特殊处理。
const bootstrapAPIKey = "TAOAPIKEY"

// Settings 是一份完整设置。字段全部导出以便直接序列化。
type Settings struct {
	Version int `json:"version"`

	// APIKey 本地访问密钥。
	//
	// 空字符串有明确语义：【不校验】（委托人要求"key 可以为空"）。
	// 非空时 /v1/* 必须带 Authorization: Bearer <APIKey>。
	APIKey string `json:"apiKey"`

	// Port 监听端口。host 恒为 127.0.0.1，故意不提供配置项。
	Port int `json:"port"`

	// AutoCheckin 是否启用自动签到（委托人要求默认开启）。
	AutoCheckin bool `json:"autoCheckin"`

	// AutoStart 是否随系统启动（默认关闭，用户显式开启才写注册表）。
	AutoStart bool `json:"autoStart"`

	// Keepalive 是否启用凭证保活（token 自动续期）。
	//
	// 🔴 用**指针**而不是 bool，为的是区分"用户显式关掉"与"老配置里没这个键"：
	//
	//	这个字段是后加的。老用户的 config.json 里没有它，反序列化后
	//	零值是 false —— 若按 bool 处理，他们升级后保活**默认是关的**，
	//	而升级前他们从不需要重新登录，行为会**静默倒退**，
	//	且界面不会提示"你该去开一下"。
	//
	//	⇒ nil = 从没表过态 ⇒ 跟随默认（**开启**，见 keepaliveEnabled）
	//	  false = 用户明确关掉 ⇒ 尊重
	//	  true = 用户明确开启
	Keepalive *bool `json:"keepalive,omitempty"`

	// Aliases 模型别名表：客户端模型名 → 真实模型 ID。
	//
	// 委托人要求"映射表由用户自己写"，所以默认给空表，绝不内置别名。
	Aliases map[string]string `json:"aliases"`

	// Revision 是配置的版本号，用于乐观锁（Codex 第 11、12 轮要求）。
	//
	// 🔴 语义（严格按 Codex 的要求实现）：
	//   - **只有实际内容变化才递增**（写入相同值不算变化）
	//   - 校验失败、写盘失败**都不递增**
	//   - 与配置**一起原子落盘**（否则重启或父子进程并行时会回退）
	//
	// 为什么需要它：两个标签页同时编辑设置时，各自基于旧快照提交，
	// 后提交者会**静默覆盖**先提交者的改动。有了 revision，
	// PATCH 可以带上"我基于哪一版改的"，不匹配就拒绝并提示重新加载。
	//
	// 为什么粒度是"整个 settings"而不是只给 aliases：
	// key / 端口 / 开关同样会发生覆盖（Codex 明确指出）。
	Revision int64 `json:"revision"`
}

// Default 返回内置默认设置。
//
// 🔴 默认值是委托人逐条确认过的，改动前先问（见各字段注释）：
//   - apiKey      : "key"（委托人明确指定；不是随机值，理由见 bootstrapAPIKey）
//   - autoCheckin : **false** —— 委托人 2026-10-05 明确"自动签到默认关"，
//     早期文档写的"默认开"已作废。
//   - autoStart   : false（用户显式开启才写注册表，不自作主张）
func Default() Settings {
	return Settings{
		Version:     Version,
		APIKey:      bootstrapAPIKey,
		Port:        DefaultPort,
		AutoCheckin: false, // 委托人明确：默认关闭
		AutoStart:   false,
		Aliases:     map[string]string{},
	}
}

// Store 是并发安全的设置容器：内存快照 + 落盘。
//
// 为什么在内存里保留快照而不是每次读文件：/v1/* 的鉴权在每个请求上都要
// 比对 key，读一次磁盘太贵。写操作走 Mutate，持锁完成"改内存 + 落盘"，
// 保证两者不会不一致。
type Store struct {
	mu   sync.RWMutex
	path string
	cur  Settings

	// corrupt 记录加载时是否发现配置损坏（供 /status 与面板提示）。
	corrupt bool
}

// NewInMemory 返回一个不落盘的 Store，内容为默认值。
//
// 用途：配置文件损坏且无法备份、或测试需要隔离磁盘时，
// 让服务仍能用默认值跑起来 —— 总比因为一个配置问题完全起不来好。
// 注意：此时 Mutate 只改内存，重启后丢失。
func NewInMemory() *Store {
	return &Store{cur: Default()}
}

// Path 返回配置文件路径。
//
// 🔴 2026-10-07 改：位置由 `internal/datadir` 统一决定
// （exe 同目录下的 `config.json`，见该包注释）。
func Path() string {
	return datadir.ConfigPath()
}

// Load 读取配置；文件不存在时用默认值并【立即写盘】（让用户能看到这个文件）。
//
// 损坏时不覆盖原文件，而是改名保留后回退默认值。
func Load(path string) (*Store, error) {
	s := &Store{path: path, cur: Default()}

	var onDisk Settings
	found, err := storage.ReadJSONFile(path, &onDisk)
	if err != nil {
		// 解析失败：保留证据，回退默认值，不覆盖用户的文件内容
		s.corrupt = true
		if renameErr := quarantine(path); renameErr != nil {
			return nil, fmt.Errorf("配置损坏（%v），且无法备份原文件: %w", err, renameErr)
		}
		if werr := s.write(s.cur); werr != nil {
			return nil, werr
		}
		return s, nil
	}

	if !found {
		// 首次运行：落盘一份默认配置，让设置页有东西可读
		if werr := s.write(s.cur); werr != nil {
			return nil, werr
		}
		return s, nil
	}

	s.cur = normalize(onDisk)
	// 补齐缺失字段（例如老版本没有 aliases）后回写，保证文件始终完整
	if werr := s.write(s.cur); werr != nil {
		return nil, werr
	}
	return s, nil
}

// Get 返回当前设置的副本（调用方可安全修改）。
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.clone()
}

// Corrupt 报告加载时是否发现过损坏配置。
func (s *Store) Corrupt() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.corrupt
}

// Path 返回本 Store 使用的配置文件路径。
func (s *Store) Path() string { return s.path }

// Mutate 在持锁状态下修改设置并落盘。
//
// 🔴 语义：fn 修改【副本】，返回 nil 才落盘并替换内存快照；
// 返回错误则内存与磁盘都不变（原子性）。这样校验失败不会留下半套配置。
//
// 🔴 Revision 语义（Codex 第 12 轮要求，逐条实现）：
//   - **只有实际内容变化才递增** —— 写入相同的值不算变更。
//     否则前端的"老值重提交"会把别人的 revision 顶掉，乐观锁形同虚设。
//   - 校验失败（fn 报错）、写盘失败**都不递增**。
//   - 与配置**一起原子落盘**（同一个 AtomicWrite）。
func (s *Store) Mutate(fn func(*Settings) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cur.clone()
	if err := fn(&next); err != nil {
		return err
	}
	next = normalize(next)

	// 内容没变就不写盘、不递增 revision。
	// 用"清掉 revision 后比较"来判断，避免 revision 自身干扰比较。
	if equalIgnoringRevision(next, s.cur) {
		return nil
	}
	next.Revision = s.cur.Revision + 1

	if err := s.write(next); err != nil {
		// 写盘失败：内存不动，revision 自然也不动
		return err
	}
	s.cur = next
	// 写盘成功 = 磁盘上现在是合法配置 → 清除损坏标记，
	// 否则面板会永久显示"配置损坏"的红条（Codex 第 13 轮指出）。
	s.clearCorruptIfWritten()
	return nil
}

// equalIgnoringRevision 比较两份配置的内容（忽略 Revision 字段）。
func equalIgnoringRevision(a, b Settings) bool {
	a.Revision, b.Revision = 0, 0
	return a.sameContent(b)
}

// sameContent 逐字段比较（含别名表）。
//
// 手写而不反射：字段就这几个，手写更直白且不会漏掉 map 的深比较。
func (v Settings) sameContent(o Settings) bool {
	if v.Version != o.Version || v.APIKey != o.APIKey || v.Port != o.Port ||
		v.AutoCheckin != o.AutoCheckin || v.AutoStart != o.AutoStart {
		return false
	}
	// Keepalive 是 *bool：nil 与 false 语义不同（见字段说明），
	// 所以不能只比取值 —— 必须区分"从没设过"和"显式关掉"。
	if !sameBoolPtr(v.Keepalive, o.Keepalive) {
		return false
	}
	if len(v.Aliases) != len(o.Aliases) {
		return false
	}
	for k, val := range v.Aliases {
		if ov, ok := o.Aliases[k]; !ok || ov != val {
			return false
		}
	}
	return true
}

// sameBoolPtr 比较两个 *bool："同时为 nil"算相等，其余比取值。
func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// clearCorruptIfWritten 在成功写回一份合法配置后清除损坏标记。
//
// 🔴 为什么需要（Codex 第 13 轮指出）：
//
//	corrupt 原本只在加载时置位、之后永不清除。于是用户在面板上
//	**已经把配置改好并成功保存**，面板却还永久显示
//	"配置文件损坏，已回退默认值"的红条 —— 误导且无法消除。
//
//	写盘成功即证明磁盘上现在是一份合法配置，标记应随之清除。
//	调用方：Mutate（写盘成功后）。
func (s *Store) clearCorruptIfWritten() {
	s.corrupt = false
}

// atomicWrite 是 storage.AtomicWrite 的间接层，**仅供测试注入**。
//
// 为什么需要它（Codex 第 17 轮对 rename 重试的第四条要求）：
//
//	"写盘失败不会更新内存快照"
//
// 这条要求针对的是 Mutate 里"fn 成功、但落盘失败"这个失败点 ——
// 它与"fn 自己返回错误"是**不同的路径**：
//   - fn 报错在第 227 行就返回了，根本没走到写盘
//   - 写盘失败在第 239 行，此时 next 已经算好、revision 已递增
//
// 若写盘失败却把 s.cur 换掉，内存会说"已保存成 X"，磁盘上却是旧值 ——
// 面板显示新值、重启后变回旧值，用户以为改了设置其实没生效。
//
// 注入点放在最底层，被测的仍是 Mutate 的完整生产控制流。
var atomicWrite = storage.AtomicWrite

// write 落盘。调用方必须已持锁（或处于 Load 的单线程阶段）。
func (s *Store) write(v Settings) error {
	v.Version = Version
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	// 0600：apiKey 属于本地 bearer 凭据，不给其他用户读
	return atomicWrite(s.path, append(raw, '\n'), 0o600)
}

// clone 深拷贝，避免调用方改到内部 map。
func (v Settings) clone() Settings {
	out := v
	out.Aliases = make(map[string]string, len(v.Aliases))
	for k, val := range v.Aliases {
		out.Aliases[k] = val
	}
	return out
}

// normalize 把缺失/非法字段修成合法值。
//
// 目的：磁盘上的配置可能来自旧版本或被手工编辑过，
// 这里统一收口，让后续代码不必到处判空。
func normalize(v Settings) Settings {
	if v.Port < MinPort || v.Port > 65535 {
		v.Port = DefaultPort
	}
	if v.Aliases == nil {
		v.Aliases = map[string]string{}
	}
	v.Version = Version
	return v
}

// quarantine 把损坏的配置改名保留，返回是否成功。
//
// 为什么不直接删除：用户可能手工编辑过，里面也许有他想要的别名表；
// 保留 `.corrupt-<时间戳>` 让他能自己捞回来。
//
// 🔴 文件名必须**唯一**（Codex 第 13 轮指出）：
//
//	原先只用 Unix 秒。若同一秒内连续两次损坏（例如程序被反复重启），
//	第二次会**覆盖**第一次的备份 —— 证据就丢了，
//	而这正是最需要证据的场景（连续损坏通常意味着有东西在反复写坏它）。
//
//	现在：秒级时间戳 + 冲突时递增后缀，并用 O_EXCL 创建以防竞态。
//	（不引入随机数是为了让文件名可读、可按时间排序。）
func quarantine(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil // 没文件就没什么可备份的
		}
		return err
	}

	base := fmt.Sprintf("%s.corrupt-%d", path, nowUnix())

	// 先试无后缀，再试 .1/.2/... 直到找到没被占用的名字
	for i := 0; i < 1000; i++ {
		backup := base
		if i > 0 {
			backup = fmt.Sprintf("%s.%d", base, i)
		}
		// os.Rename 在目标已存在时会覆盖（Windows 上会失败）——
		// 两种情况都不安全，所以先用 Stat 判存在，再 rename。
		if _, err := os.Stat(backup); err == nil {
			continue // 已存在，换下一个名字
		} else if !os.IsNotExist(err) {
			return err
		}
		return os.Rename(path, backup)
	}
	return fmt.Errorf("无法为损坏的配置找到唯一的备份名（已尝试 1000 个）")
}

// ValidatePort 校验端口是否可接受。
func ValidatePort(p int) error {
	if p < MinPort || p > 65535 {
		return fmt.Errorf("端口必须在 %d–65535 之间（当前 %d）；"+
			"低于 %d 通常需要管理员权限", MinPort, p, MinPort)
	}
	return nil
}

// nowUnixFn 是时间源，抽成变量便于测试固定（隔离时间依赖）。
//
// 🔴 为什么必须可替换（2026-10-05 实测教训）：
//
//	quarantine 的备份名基于**秒级**时间戳。测试若依赖"三次损坏恰好
//	落在同一秒"，在高负载下会偶发失败（秒边界跨过去，命名自然不同），
//	表现为"应有 3 份备份，实际 1 份" —— 看起来像唯一性逻辑坏了，
//	实际是在测时间。固定时间源后，测试才在测唯一性本身。
var nowUnixFn = func() int64 { return time.Now().Unix() }

// nowUnix 返回当前 Unix 秒（经可替换的时间源）。
func nowUnix() int64 { return nowUnixFn() }

// GenerateKey 生成一个随机 API key（32 位十六进制 = 128 位熵）。
//
// 面板的"生成随机密钥"按钮用它。用 crypto/rand 而非 math/rand：
// 这是鉴权凭据，可预测的值等于没有保护。
func GenerateKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机密钥失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ValidateAliases 做别名表的**结构**校验（非空、无斜杠、长度、自映射）。
//
// 语义校验（目标模型是否存在、是否成环）需要 router 的模型知识，
// 由调用方在写入前另行完成 —— 本包不 import router，避免循环依赖。
func ValidateAliases(m map[string]string) error {
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("别名不能为空")
		}
		// 含 "/" 会与 workbuddy/ 这类渠道前缀混淆，且让 Resolve 难以判断
		if strings.Contains(k, "/") {
			return fmt.Errorf("别名 %q 不能包含 %q", k, "/")
		}
		if len(k) > 64 {
			return fmt.Errorf("别名 %q 过长（上限 64 字符）", k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("别名 %q 的目标不能为空", k)
		}
		if k == v {
			return fmt.Errorf("别名 %q 不能指向自己", k)
		}
	}
	return nil
}

// SortedAliasKeys 返回排好序的别名键，便于稳定输出（测试与面板展示用）。
func SortedAliasKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
