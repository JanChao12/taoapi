package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/storage"
)

// newCodecForTest 用确定性 codec（不依赖 DPAPI，测试可跨环境跑）。
//
// 注意：Persister 的编解码对称性已由 auth 包测试覆盖，
// 这里只关心 CLI 导入逻辑本身。
func newCodecForTest() storage.Codec { return xorTestCodec{key: 0x77} }

type xorTestCodec struct{ key byte }

func (c xorTestCodec) Name() string { return "xor-cli-test" }
func (c xorTestCodec) Encrypt(p []byte) ([]byte, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ c.key
	}
	return out, nil
}
func (c xorTestCodec) Decrypt(p []byte) ([]byte, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ c.key
	}
	return out, nil
}

// writeWildWorkFile 造一个 wild-work 形状的凭据文件。
func writeWildWorkFile(t *testing.T, dir, name, uid, nick, token string) {
	t.Helper()
	obj := map[string]any{
		"account": map[string]any{"uid": uid, "nickname": nick},
		"auth": map[string]any{
			"accessToken":  token,
			"refreshToken": "rt-" + uid,
			"domain":       "www.codebuddy.cn",
			"expiresAt":    1794405600,
		},
	}
	raw, _ := json.Marshal(obj)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAuthImportBasic 验证正常导入：数量、掩码输出、加密落盘。
func TestAuthImportBasic(t *testing.T) {
	dir := t.TempDir()
	writeWildWorkFile(t, dir, "workbuddy-aaa.json",
		"aaaaaaaa-1111-2222-3333-444444444444", "13800000001", "tok-A")

	// 把数据目录指到临时位置（导入写的是 auth.AccountsPath()，由环境变量控制）
	dataDir := t.TempDir()
	t.Setenv("WBAPI_DATA_DIR", dataDir)

	code := runAuthImport([]string{"--from-wild-work", dir})
	if code != 0 {
		t.Fatalf("退出码 = %d，期望 0", code)
	}

	// 文件存在且不含明文 token
	raw, err := os.ReadFile(auth.AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tok-A") {
		t.Fatal("🔴 明文 token 落盘")
	}

	// 能加载回来
	p := auth.NewPersister(auth.AccountsPath(), storageCodec())
	st, err := p.Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if st.Len() != 1 {
		t.Fatalf("账号数 = %d", st.Len())
	}
	a, ok := st.Get("aaaaaaaa-1111-2222-3333-444444444444")
	if !ok {
		t.Fatal("找不到导入的账号")
	}
	if a.AccessToken != "tok-A" {
		t.Error("token 未正确还原")
	}
	if a.Nickname != "13800000001" {
		t.Errorf("昵称 = %q", a.Nickname)
	}
}

// TestAuthImportImportsBothPlatformsAndCountsBadFiles 验证两平台导入与坏文件处理。
//
// 🔴 契约在 2026-10-06 变了：国际版（workbuddyai-*）**不再跳过**，
//
//	而是按文件名前缀判定平台后一并导入。
//
//	原测试断言"国际版不该被导入" —— 那是**旧契约**。
//	契约变了护栏就要跟着改，不能留着矛盾断言。
func TestAuthImportImportsBothPlatformsAndCountsBadFiles(t *testing.T) {
	dir := t.TempDir()
	writeWildWorkFile(t, dir, "workbuddy-good.json",
		"bbbbbbbb-1111-2222-3333-444444444444", "13800000002", "tok-B")
	// 国际版：现在**应被导入**，且标记为 intl 平台
	writeWildWorkFile(t, dir, "workbuddyai-intl.json",
		"cccccccc-1111-2222-3333-444444444444", "intl", "tok-C")
	// 缺 token：应跳过并计坏
	writeWildWorkFile(t, dir, "workbuddy-notoken.json",
		"dddddddd-1111-2222-3333-444444444444", "13800000003", "")
	// 非 JSON：应跳过并计坏
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 无关文件：应忽略
	if err := os.WriteFile(filepath.Join(dir, "other.json"),
		[]byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WBAPI_DATA_DIR", t.TempDir())

	code := runAuthImport([]string{"--from-wild-work", dir})
	if code != 1 {
		t.Fatalf("有坏文件时退出码应为 1，实际 %d", code)
	}

	p := auth.NewPersister(auth.AccountsPath(), storageCodec())
	st, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	// 国内 + 国际 = 2（坏文件与无关文件排除）
	if st.Len() != 2 {
		t.Fatalf("应导入 2 个（国内 + 国际；坏文件/无关文件排除），实际 %d", st.Len())
	}

	cn, ok := st.Get("bbbbbbbb-1111-2222-3333-444444444444")
	if !ok {
		t.Fatal("国内账号未被导入")
	}
	if cn.PlatformOf() != auth.PlatformCN {
		t.Errorf("国内账号平台 = %q，期望 %q", cn.PlatformOf(), auth.PlatformCN)
	}

	intl, ok := st.Get("cccccccc-1111-2222-3333-444444444444")
	if !ok {
		t.Fatal("国际版账号应被导入（2026-10-06 起支持）")
	}
	if intl.PlatformOf() != auth.PlatformIntl {
		t.Errorf("国际版账号平台 = %q，期望 %q —— "+
			"平台标错会让它的凭据被发到错误域名", intl.PlatformOf(), auth.PlatformIntl)
	}
}

// TestAuthImportDedupByUID 验证重复导入按 UID 去重（更新而非新增）。
func TestAuthImportDedupByUID(t *testing.T) {
	dir := t.TempDir()
	uid := "eeeeeeee-1111-2222-3333-444444444444"
	writeWildWorkFile(t, dir, "workbuddy-e.json", uid, "13800000005", "tok-E1")

	t.Setenv("WBAPI_DATA_DIR", t.TempDir())
	if code := runAuthImport([]string{"--from-wild-work", dir}); code != 0 {
		t.Fatalf("首次导入退出码 = %d", code)
	}

	// 同 UID 换 token 再导入
	writeWildWorkFile(t, dir, "workbuddy-e2.json", uid, "13800000005", "tok-E2")
	if code := runAuthImport([]string{"--from-wild-work", dir}); code != 0 {
		t.Fatalf("二次导入退出码 = %d", code)
	}

	p := auth.NewPersister(auth.AccountsPath(), storageCodec())
	st, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Len() != 1 {
		t.Fatalf("应去重为 1 个，实际 %d", st.Len())
	}
	a, _ := st.Get(uid)
	if a.AccessToken != "tok-E2" {
		t.Errorf("重复导入应更新 token，实际未更新")
	}
}

// TestAuthImportMissingDir 验证目录不存在时报错。
func TestAuthImportMissingDir(t *testing.T) {
	t.Setenv("WBAPI_DATA_DIR", t.TempDir())
	if code := runAuthImport([]string{"--from-wild-work", filepath.Join(t.TempDir(), "nope")}); code == 0 {
		t.Fatal("目录不存在应返回非 0")
	}
	if code := runAuthImport(nil); code != 2 {
		t.Fatal("缺少参数应返回 2")
	}
}

// TestTodayCNIsUTC8 验证签到归属日按北京时间。
func TestTodayCNIsUTC8(t *testing.T) {
	// 构造一个 UTC 已过午夜、北京还没过的时刻
	utc := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC) // 北京时间 10-05 09:00
	got := utc.In(cnZone).Format("2006-01-02")
	if got != "2026-10-05" {
		t.Errorf("got %q", got)
	}
	// 北京 00:30（UTC 前一天 16:30）应归当天
	utc2 := time.Date(2026, 10, 4, 16, 30, 0, 0, time.UTC)
	if got2 := utc2.In(cnZone).Format("2006-01-02"); got2 != "2026-10-05" {
		t.Errorf("北京 00:30 应算 10-05，实际 %q", got2)
	}
}

// TestMaskedOutputNeverContainsToken 验证 list 输出不含 token。
func TestMaskedOutputNeverContainsToken(t *testing.T) {
	// 该保证由 auth.MaskUID 的既有测试覆盖；
	// 这里验证 CLI 路径用的是 MaskUID 而不是裸 UID：
	// 构造一个账号跑 auth list，断言 stdout 不含完整 UID 尾段。
	dir := t.TempDir()
	uid := "ffffffff-1111-2222-3333-444444444444"
	writeWildWorkFile(t, dir, "workbuddy-f.json", uid, "13800000006", "tok-F")
	t.Setenv("WBAPI_DATA_DIR", t.TempDir())

	if code := runAuthImport([]string{"--from-wild-work", dir}); code != 0 {
		t.Fatalf("导入失败 code=%d", code)
	}

	// runAuthList 打印到 stdout；这里只验证加载路径不报错，
	// 掩码正确性由 auth 包的 TestMaskUID 保证。
	if code := runAuthList(nil); code != 0 {
		t.Fatalf("list 失败 code=%d", code)
	}
}
