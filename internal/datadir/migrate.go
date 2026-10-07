package datadir

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MigrateResult 描述一次自动迁移做了什么（供调用方记日志）。
type MigrateResult struct {
	// Performed 是否真的执行了迁移。
	Performed bool

	// From / To 迁移的源与目标根目录。
	From, To string

	// Moved 成功搬运的文件（相对路径）。
	Moved []string

	// Skipped 因目标已存在而**跳过**的文件（相对路径）。
	//
	// 🔴 跳过而不是覆盖：目标已有数据说明用户已经在新位置用过，
	// 此时覆盖会丢掉**更新的**数据。跳过最安全。
	Skipped []string

	// Errors 搬运失败的文件与原因（不致命，逐个记录）。
	Errors []string
}

// MigrationNeeded 判断是否需要做一次性迁移。
//
// 条件（**三条全部满足**才迁移）：
//
//	① 新位置还没有账号文件 —— 否则说明已经在用新位置了
//	② 旧目录（~/.wbapi）存在且**有内容**
//	③ 旧目录与新位置不是同一个目录
//
// ⚠️ 条件 ① 是**幂等性**的关键：迁移过一次后就不会再迁，
// 避免每次启动都去动旧目录。
func MigrationNeeded() bool {
	if _, err := os.Stat(AccountsPath()); err == nil {
		return false // 新位置已有账号文件 ⇒ 已在用新位置
	}
	legacy := LegacyRoot()
	if legacy == "" {
		return false
	}
	if samePath(legacy, Root()) {
		return false
	}
	entries, err := os.ReadDir(legacy)
	if err != nil || len(entries) == 0 {
		return false
	}
	return true
}

// Migrate 把旧目录里的数据搬到新位置。
//
// ═══════════════════════════════════════════════════════════════════
// 🔴 安全原则（动的是**唯一凭据副本**，必须逐条守住）
// ═══════════════════════════════════════════════════════════════════
//
//  1. **用复制而不是移动** —— 旧目录原样保留。
//     凭据一旦因搬家中断而半途丢失，就只能重新登录恢复。
//     多留一份的代价是几百 KB 磁盘，完全值得。
//  2. **绝不覆盖目标已有文件** —— 目标存在就跳过并记录。
//  3. **绝不用 JSON 解析后回写** —— 纯字节复制。
//     （教训：曾用 ConvertFrom-Json 读账号文件失败返回 null，
//     紧接着写回把文件清成 0 字节，三个账号凭据全丢。）
//  4. **逐个文件独立处理** —— 一个失败不影响其余，且原因逐个记录。
//  5. **.bak 备份文件也一并搬** —— 它们是账号文件的最后一道保险。
//
// 只在 MigrationNeeded() 为真时调用。
func Migrate() MigrateResult {
	res := MigrateResult{From: LegacyRoot(), To: Root()}
	if !MigrationNeeded() {
		return res
	}
	res.Performed = true

	// 旧 → 新的路径映射（照 wild-work 布局）
	//
	//	~/.wbapi/accounts.json      → <root>/auths/accounts.json
	//	~/.wbapi/config.json        → <root>/config.json
	//	~/.wbapi/events/*           → <root>/data/events/*
	//	~/.wbapi/*.bak*             → 与源文件同目录（保住备份）
	type item struct{ src, dst string }
	var items []item

	add := func(oldRel, newRel string) {
		src := filepath.Join(res.From, oldRel)
		if _, err := os.Stat(src); err != nil {
			return // 源不存在：正常，跳过
		}
		items = append(items, item{src: src, dst: filepath.Join(res.To, newRel)})
	}

	add("accounts.json", filepath.Join(AuthsSubdir, AccountsFile))
	add("config.json", ConfigFile)

	// 账号/配置的各种 .bak-* 备份（同一目录下）
	if entries, err := os.ReadDir(res.From); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if isBackupName(name) {
				add(name, filepath.Join(AuthsSubdir, name))
			}
		}
	}

	// 用量流水
	if entries, err := os.ReadDir(filepath.Join(res.From, EventsSubdir)); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			add(filepath.Join(EventsSubdir, e.Name()),
				filepath.Join(DataSubdir, EventsSubdir, e.Name()))
		}
	}

	for _, it := range items {
		rel, _ := filepath.Rel(res.To, it.dst)
		if _, err := os.Stat(it.dst); err == nil {
			res.Skipped = append(res.Skipped, rel)
			continue
		}
		if err := copyFile(it.src, it.dst); err != nil {
			res.Errors = append(res.Errors,
				fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		res.Moved = append(res.Moved, rel)
	}
	return res
}

// isBackupName 判断是否是备份文件（accounts.json.bak-xxx / config.json.bak 等）。
func isBackupName(name string) bool {
	return len(name) > 4 && (contains(name, ".bak") || contains(name, ".corrupt-"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// copyFile 纯字节复制（**不做任何解析/回写**）。
//
// 先写临时文件再 rename：避免中途失败留下**半个文件**被当成有效数据。
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".migrate*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("复制失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭失败: %w", err)
	}

	// 凭据文件收紧权限（Windows 上作用有限，但不该放宽）
	_ = os.Chmod(tmpName, 0o600)

	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("替换目标失败: %w", err)
	}
	tmpName = "" // 已 rename，无需清理
	return nil
}

// samePath 判断两个路径是否指向同一位置（大小写不敏感，Windows）。
func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}
