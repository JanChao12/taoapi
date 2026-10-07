package datadir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 数据目录解析与一次性迁移的测试（2026-10-07 数据目录改为 exe 同目录）
// ═══════════════════════════════════════════════════════════════════
//
// 🔴 这组测试的分量：迁移动的是**唯一凭据副本**。
//	历史上本项目因误写账号文件丢过全部凭据（见 AGENTS.md），
//	所以"不覆盖、不解析、失败不影响源"这几条必须有测试守着。

// TestEnvVarWinsOverExeDir 守：`WBAPI_DATA_DIR` 优先级最高。
//
// 测试隔离全靠它 —— 若这条失效，测试会把数据写进源码树。
func TestEnvVarWinsOverExeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)
	ResetForTest()
	t.Cleanup(ResetForTest)

	if got := Root(); got != dir {
		t.Fatalf("Root() = %q，期望环境变量指定的 %q", got, dir)
	}
}

// TestRootFollowsEnvChange 守：**同一进程内**改环境变量后必须重新解析。
//
// 🔴 这是我在本包上踩过的真实坑：
//
//	第一版用 `sync.Once` 无条件缓存 ⇒ 第二个测试拿到第一个测试的目录
//	⇒ `internal/app` 的两个 import 测试**整包必红、单跑必绿**。
//	这种"单跑通过、整包失败"最容易被误判成随机 flake 而放过。
func TestRootFollowsEnvChange(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Cleanup(ResetForTest)

	t.Setenv(EnvVar, a)
	ResetForTest()
	if got := Root(); got != a {
		t.Fatalf("第一次 Root() = %q，期望 %q", got, a)
	}

	t.Setenv(EnvVar, b)
	if got := Root(); got != b {
		t.Fatalf("改环境变量后 Root() = %q，期望 %q —— "+
			"缓存必须按环境变量取值失效，否则测试会互相污染", got, b)
	}
}

// TestPathsFollowWildWorkLayout 守：目录布局照 wild-work
// （auths/ + data/ + config.json 都在根目录下）。
func TestPathsFollowWildWorkLayout(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvVar, root)
	ResetForTest()
	t.Cleanup(ResetForTest)

	cases := []struct{ name, got, want string }{
		{"ConfigPath", ConfigPath(), filepath.Join(root, "config.json")},
		{"AccountsPath", AccountsPath(), filepath.Join(root, "auths", "accounts.json")},
		{"AuthsDir", AuthsDir(), filepath.Join(root, "auths")},
		{"EventsDir", EventsDir(), filepath.Join(root, "data", "events")},
		{"LogsDir", LogsDir(), filepath.Join(root, "data", "logs")},
		{"ReportsDir", ReportsDir(), filepath.Join(root, "data", "reports")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q，期望 %q", c.name, c.got, c.want)
		}
	}
}

// TestMigrateCopiesAndKeepsSource 守：迁移是**复制**，源目录必须保留。
//
// 🔴 凭据只此一份，搬家途中出错就没有第二份了 ⇒ 绝不删源。
func TestMigrateCopiesAndKeepsSource(t *testing.T) {
	legacy := t.TempDir()
	newRoot := t.TempDir()

	t.Setenv("USERPROFILE", legacy)
	t.Setenv("HOME", legacy)
	t.Setenv(EnvVar, newRoot)
	ResetForTest()
	t.Cleanup(ResetForTest)

	// 造旧数据（内容任意 —— 迁移按**字节**搬，不解析）
	mustWrite(t, filepath.Join(legacy, LegacyDirName, "accounts.json"), "CREDENTIAL-BYTES")
	mustWrite(t, filepath.Join(legacy, LegacyDirName, "config.json"), `{"port":8787}`)
	mustWrite(t, filepath.Join(legacy, LegacyDirName, "events", "2026-10-07.jsonl"), "EVENT")

	if !MigrationNeeded() {
		t.Fatal("前置条件不成立：应当判定为需要迁移")
	}
	res := Migrate()
	if !res.Performed {
		t.Fatal("Migrate 应执行迁移")
	}
	if len(res.Errors) > 0 {
		t.Fatalf("迁移不应有错误: %v", res.Errors)
	}

	// ① 新位置文件存在且**内容逐字节相同**
	assertSameBytes(t,
		filepath.Join(legacy, LegacyDirName, "accounts.json"),
		filepath.Join(newRoot, "auths", "accounts.json"))
	assertSameBytes(t,
		filepath.Join(legacy, LegacyDirName, "config.json"),
		filepath.Join(newRoot, "config.json"))
	assertSameBytes(t,
		filepath.Join(legacy, LegacyDirName, "events", "2026-10-07.jsonl"),
		filepath.Join(newRoot, "data", "events", "2026-10-07.jsonl"))

	// ② 源目录**必须还在**
	if _, err := os.Stat(filepath.Join(legacy, LegacyDirName, "accounts.json")); err != nil {
		t.Fatal("🔴 源文件被删了 —— 迁移必须是复制而不是移动")
	}
}

// TestMigrateNeverOverwritesExisting 守：目标已有文件时**跳过**而不是覆盖。
//
// 理由：目标有数据说明用户已在新位置用过，那些才是**更新的**数据。
func TestMigrateNeverOverwritesExisting(t *testing.T) {
	legacy := t.TempDir()
	newRoot := t.TempDir()

	t.Setenv("USERPROFILE", legacy)
	t.Setenv("HOME", legacy)
	t.Setenv(EnvVar, newRoot)
	ResetForTest()
	t.Cleanup(ResetForTest)

	mustWrite(t, filepath.Join(legacy, LegacyDirName, "config.json"), "OLD")

	// 先让新位置"已有账号文件"会把 MigrationNeeded 变成 false，
	// 所以这里直接调 Migrate 前先建 config.json（不是账号文件）
	mustWrite(t, filepath.Join(newRoot, "config.json"), "NEWER")

	res := Migrate()
	if !res.Performed {
		t.Fatal("应执行迁移（账号文件尚未存在）")
	}
	got := readFile(t, filepath.Join(newRoot, "config.json"))
	if got != "NEWER" {
		t.Fatalf("🔴 目标被覆盖了：%q，期望保留 NEWER", got)
	}
	if len(res.Skipped) == 0 {
		t.Error("被跳过的文件应记录在 Skipped 里（不能静默）")
	}
}

// TestMigrateIsIdempotent 守：迁移过一次后不再重复迁移。
//
// 靠"新位置已有账号文件"判定。若不幂等，每次启动都会去翻旧目录。
func TestMigrateIsIdempotent(t *testing.T) {
	legacy := t.TempDir()
	newRoot := t.TempDir()

	t.Setenv("USERPROFILE", legacy)
	t.Setenv("HOME", legacy)
	t.Setenv(EnvVar, newRoot)
	ResetForTest()
	t.Cleanup(ResetForTest)

	mustWrite(t, filepath.Join(legacy, LegacyDirName, "accounts.json"), "C")

	if !MigrationNeeded() {
		t.Fatal("首次应需要迁移")
	}
	_ = Migrate()

	if MigrationNeeded() {
		t.Fatal("🔴 迁移后仍判定需要迁移 —— 会每次启动都翻旧目录")
	}
}

// TestMigrateSkipsWhenAlreadyUsingNewLocation 守：新位置已在用时不迁移。
func TestMigrateSkipsWhenAlreadyUsingNewLocation(t *testing.T) {
	legacy := t.TempDir()
	newRoot := t.TempDir()

	t.Setenv("USERPROFILE", legacy)
	t.Setenv("HOME", legacy)
	t.Setenv(EnvVar, newRoot)
	ResetForTest()
	t.Cleanup(ResetForTest)

	mustWrite(t, filepath.Join(legacy, LegacyDirName, "accounts.json"), "OLD")
	mustWrite(t, filepath.Join(newRoot, "auths", "accounts.json"), "NEW")

	if MigrationNeeded() {
		t.Fatal("新位置已有账号文件 ⇒ 不该再迁移")
	}
	res := Migrate()
	if res.Performed {
		t.Error("不该执行迁移")
	}
}

// TestMigrateHandlesMissingLegacyDir 守：旧目录不存在时安静返回（不是错误）。
func TestMigrateHandlesMissingLegacyDir(t *testing.T) {
	legacy := t.TempDir() // 里面没有 .wbapi
	newRoot := t.TempDir()

	t.Setenv("USERPROFILE", legacy)
	t.Setenv("HOME", legacy)
	t.Setenv(EnvVar, newRoot)
	ResetForTest()
	t.Cleanup(ResetForTest)

	if MigrationNeeded() {
		t.Fatal("旧目录不存在 ⇒ 不该需要迁移")
	}
	res := Migrate()
	if res.Performed || len(res.Errors) > 0 {
		t.Errorf("应为空操作，实际 Performed=%v Errors=%v", res.Performed, res.Errors)
	}
}

// TestMigrateBringsBackups 守：`.bak-*` 备份文件也一起搬。
//
// 它们是账号文件的最后一道保险，漏掉等于削弱恢复能力。
func TestMigrateBringsBackups(t *testing.T) {
	legacy := t.TempDir()
	newRoot := t.TempDir()

	t.Setenv("USERPROFILE", legacy)
	t.Setenv("HOME", legacy)
	t.Setenv(EnvVar, newRoot)
	ResetForTest()
	t.Cleanup(ResetForTest)

	mustWrite(t, filepath.Join(legacy, LegacyDirName, "accounts.json"), "C")
	mustWrite(t, filepath.Join(legacy, LegacyDirName, "accounts.json.bak-20261001-120000"), "BACKUP1")
	mustWrite(t, filepath.Join(legacy, LegacyDirName, "config.json.bak-before-test"), "BACKUP2")

	_ = Migrate()

	for _, f := range []string{
		"accounts.json.bak-20261001-120000",
		"config.json.bak-before-test",
	} {
		p := filepath.Join(newRoot, "auths", f)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("备份文件未被迁移: %s", f)
		}
	}
}

// ── 测试辅助 ──────────────────────────────────────────────

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// assertSameBytes 断言两个文件内容逐字节相同（迁移必须是纯字节复制）。
func assertSameBytes(t *testing.T, src, dst string) {
	t.Helper()
	a, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源失败 %s: %v", src, err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读目标失败 %s: %v", dst, err)
	}
	if string(a) != string(b) {
		t.Fatalf("内容不一致：\n源  %s = %q\n目标 %s = %q\n"+
			"（迁移必须是纯字节复制，不得解析后回写）",
			src, truncate(a), dst, truncate(b))
	}
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// 断言 strings 被使用（truncate 之外留一个引用，避免未来删函数时报 unused）
var _ = strings.TrimSpace
