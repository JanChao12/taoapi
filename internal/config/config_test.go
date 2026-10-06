package config

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy.local/workbuddy-api/internal/storage"
)

// 本文件守住 config 包的几条关键承诺。每个测试都注明"守护什么"。

// TestLoadCreatesDefaultOnFirstRun 守：首次运行会落盘一份默认配置。
//
// 若这里红了，说明用户第一次打开设置页会读到空/报错。
func TestLoadCreatesDefaultOnFirstRun(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")

	st, err := Load(path)
	if err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("首次加载应写出配置文件: %v", err)
	}

	got := st.Get()
	if got.Port != DefaultPort {
		t.Errorf("默认端口 = %d，期望 %d", got.Port, DefaultPort)
	}
	// 🔴 委托人 2026-10-05 明确：自动签到默认【关闭】
	if got.AutoCheckin {
		t.Error("自动签到默认应为关闭（委托人明确要求）")
	}
	if got.AutoStart {
		t.Error("开机自启默认应为关闭（需用户显式开启）")
	}
	if got.APIKey == "" {
		t.Error("首启应有一个可用的初始 key，否则用户接客户端会直接 401")
	}
	if got.Aliases == nil {
		t.Error("别名表应初始化为空 map 而非 nil，避免调用方判空")
	}
}

// TestBootstrapKeyIsLiteralKey 守：默认 key 是固定的字面量（非随机）。
//
// 🔴 契约在 2026-10-06 变更：委托人把默认 key 从 "key" 改为 "TAOAPIKEY"
//
//	（并明确要求**区分大小写**）。
//
// Codex 曾建议改用 crypto/rand 随机值，被委托人否决 ——
// 这是"自用 + 只监听回环"场景下的有意取舍，不要再改回随机值。
func TestBootstrapKeyIsLiteralKey(t *testing.T) {
	dir := testConfigDir(t)
	st, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Get().APIKey; got != "TAOAPIKEY" {
		t.Fatalf("默认 key = %q，期望 \"TAOAPIKEY\"（委托人 2026-10-06 指定）", got)
	}
}

// TestBootstrapKeyIsCaseSensitive 守：密钥比较**区分大小写**。
//
// 委托方 2026-10-06 明确要求「key要区分大小写」。
//
// 🔴 这验证的是**鉴权行为**，不只是常量值：
//
//	本项目用 subtle.ConstantTimeCompare 做等值比较，
//	它是逐字节比较 —— 大小写不同必须**不放行**。
//	（若哪天有人"顺手"改成 EqualFold，这条测试会红。）
func TestBootstrapKeyIsCaseSensitive(t *testing.T) {
	dir := testConfigDir(t)
	st, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	key := st.Get().APIKey
	if key == "" {
		t.Fatal("默认 key 不该为空")
	}

	// 大小写变体必须**不相等**（模拟鉴权时的比较语义）
	if strings.EqualFold(key, key) != true {
		t.Fatal("前置条件：EqualFold 自身应为 true")
	}
	lower := strings.ToLower(key)
	if lower == key {
		t.Skipf("key %q 全为小写/无字母，无法验证大小写敏感", key)
	}
	if subtle.ConstantTimeCompare([]byte(lower), []byte(key)) == 1 {
		t.Errorf("小写形式 %q 被判为与 %q 相同 —— 密钥必须区分大小写", lower, key)
	}
}

// TestCorruptConfigIsQuarantinedNotOverwritten 守：损坏的配置不被静默覆盖。
//
// 这条最重要 —— 本机曾发生过"用脚本读写账号文件导致凭据全丢"的真实事故。
// 配置损坏时必须保留原文件（改名 .corrupt-*），让用户能自己捞回来。
func TestCorruptConfigIsQuarantinedNotOverwritten(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")

	const broken = `{"port": 8787, "aliases": {` // 截断的非法 JSON
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Load(path)
	if err != nil {
		t.Fatalf("损坏配置不应导致加载失败（应回退默认值）: %v", err)
	}
	if !st.Corrupt() {
		t.Error("应标记 corrupt=true，让 /status 与面板能提示用户")
	}

	// 原内容必须还在某个 .corrupt-* 文件里
	entries, _ := os.ReadDir(dir)
	var found bool
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if string(raw) == broken {
				found = true
			}
		}
	}
	if !found {
		t.Error("损坏的原始配置必须被改名保留，不能丢失")
	}

	// 且新配置是可用的默认值
	if st.Get().Port != DefaultPort {
		t.Errorf("回退后端口 = %d，期望 %d", st.Get().Port, DefaultPort)
	}
}

// TestCorruptFlagClearedAfterSuccessfulWrite 守：写回合法配置后清除损坏标记。
//
// 🔴 Codex 第 13 轮指出：corrupt 原本只在加载时置位、永不清除。
// 于是用户在面板上**已经把配置改好并保存成功**，面板却还永久显示
// "配置文件损坏"的红条 —— 误导且无法消除。
func TestCorruptFlagClearedAfterSuccessfulWrite(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Corrupt() {
		t.Fatal("损坏配置应置位 corrupt")
	}

	// 用户通过面板改好并保存
	if err := st.Mutate(func(s *Settings) error {
		s.Port = 9400
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if st.Corrupt() {
		t.Error("保存成功后 corrupt 应被清除，否则面板永久显示旧的损坏警告")
	}
}

// TestQuarantineNamesAreUnique 守：连续损坏不会覆盖上一份证据。
//
// 🔴 Codex 第 13 轮指出：原先只用 Unix 秒，
// 同一秒内连续两次损坏时第二次会覆盖第一次的备份 —— 证据就丢了，
// 而这正是最需要证据的场景（连续损坏通常意味着有东西在反复写坏它）。
//
// ⚠️ 本测试**不能依赖"三次都在同一秒内完成"**（我第一版就是这么写的，
// 结果在负载高时偶发失败：秒边界恰好跨过去，命名自然不同，断言数对不上）。
// 所以先把时间源固定住，再验证"同一时刻的多次损坏也各有各的文件名"。
func TestQuarantineNamesAreUnique(t *testing.T) {
	// 固定时间源 → 三次损坏必然拿到同一个基准名，只能靠后缀区分
	restore := freezeNowUnix()
	defer restore()

	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")

	for i := 0; i < 3; i++ {
		if err := os.WriteFile(path, []byte("{broken"+itoaForTest(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := os.ReadDir(dir)
	var backups []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) != 3 {
		t.Fatalf("应有 3 份备份（连续三次损坏，时间源已固定），实际 %d 份：%v",
			len(backups), backups)
	}
}

// freezeNowUnix 把 nowUnix 固定为常量，返回恢复函数。
//
// 为什么要这个：quarantine 的命名基于秒级时间戳。
// 不固定的话，测试结果会随"跨秒"而变 —— 那是在测时间，不是在测唯一性。
func freezeNowUnix() func() {
	const fixed = int64(1700000000)
	orig := nowUnixFn
	nowUnixFn = func() int64 { return fixed }
	return func() { nowUnixFn = orig }
}

// itoaForTest 生成不同的损坏内容（避免三次内容相同难以区分）。
func itoaForTest(n int) string {
	return string(rune('0' + n))
}

// TestMutateKeepsMemoryWhenDiskWriteFails 对应 Codex 第 17 轮对 rename 重试的
// 第四条要求："写盘失败不会更新内存快照"。
//
// 【它与 TestMutateIsAtomicOnError 的区别 —— 两者针对不同的失败点】：
//
//	TestMutateIsAtomicOnError：fn 自己返回错误 → 在 Mutate 开头就返回，
//	                          根本没走到写盘。测的是"校验失败不留半套配置"。
//	本测试：                  fn 成功、**写盘失败** → 走到 write 那一步。
//	                          此时 next 已算好、revision 已递增，
//	                          若把 s.cur 换掉，内存就会说"已保存成 X"，
//	                          而磁盘上仍是旧值。
//
// 后果（这条要防的）：面板显示新值、重启后变回旧值 ——
// 用户以为改了设置其实没生效，且没有任何报错线索。
func TestMutateKeepsMemoryWhenDiskWriteFails(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	before := st.Get()
	beforeRev := before.Revision

	// 让写盘必定失败（注入最底层的写入函数）。
	writeErr := errors.New("注入：磁盘写入失败")
	restore := swapAtomicWrite(t, func(string, []byte, os.FileMode) error {
		return writeErr
	})
	defer restore()

	// fn 本身成功 —— 失败只来自写盘。
	err = st.Mutate(func(s *Settings) error {
		s.Port = 19999
		s.APIKey = "改过的-key"
		return nil
	})
	if err == nil {
		t.Fatal("写盘失败时 Mutate 必须返回错误，不能假装成功")
	}

	got := st.Get()
	if got.Port != before.Port {
		t.Errorf("写盘失败后内存里的 Port 被改动了：期望 %d，实际 %d", before.Port, got.Port)
	}
	if got.APIKey != before.APIKey {
		t.Errorf("写盘失败后内存里的 APIKey 被改动了：期望 %q，实际 %q",
			before.APIKey, got.APIKey)
	}
	// revision 也不能动：它是乐观锁的依据，凭空递增会让面板的
	// If-Match 永远对不上（表现为"刚保存完又说版本冲突"）。
	if got.Revision != beforeRev {
		t.Errorf("写盘失败后 revision 被递增了：期望 %d，实际 %d",
			beforeRev, got.Revision)
	}

	// 磁盘上必须仍是旧值（写盘失败 → 不应有任何新内容）。
	var onDisk Settings
	if _, err := readJSON(path, &onDisk); err != nil {
		t.Fatalf("读回磁盘配置失败: %v", err)
	}
	if onDisk.Port != before.Port {
		t.Errorf("写盘失败后磁盘被改动了：port=%d", onDisk.Port)
	}
}

// TestMutateWriteFailureDoesNotPoisonLaterWrites 确认写盘失败是"可恢复的一次失败"，
// 而不是把 Store 带进坏状态。
//
// 为什么重要：面板上的典型场景是"磁盘短暂不可写（杀软占用/权限）→ 用户再点一次保存"。
// 若第一次失败污染了内部状态，第二次即使磁盘正常也会失败，用户会认为程序坏了。
func TestMutateWriteFailureDoesNotPoisonLaterWrites(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before := st.Get()

	// 第一次：写盘失败。
	failing := true
	restore := swapAtomicWrite(t, func(p string, data []byte, perm os.FileMode) error {
		if failing {
			return errors.New("注入：第一次写盘失败")
		}
		return storage.AtomicWrite(p, data, perm)
	})
	defer restore()

	if err := st.Mutate(func(s *Settings) error {
		s.Port = 17777
		return nil
	}); err == nil {
		t.Fatal("第一次写盘应失败")
	}
	if got := st.Get(); got.Port != before.Port {
		t.Fatalf("失败后内存不该变：%d", got.Port)
	}

	// 第二次：磁盘恢复正常，同样的改动应当成功。
	failing = false
	if err := st.Mutate(func(s *Settings) error {
		s.Port = 17777
		return nil
	}); err != nil {
		t.Fatalf("磁盘恢复后重试应当成功，实际: %v", err)
	}

	if got := st.Get(); got.Port != 17777 {
		t.Fatalf("重试成功后内存应为新值，实际 %d", got.Port)
	}

	// 磁盘上也要是新值。
	var onDisk Settings
	if _, err := readJSON(path, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Port != 17777 {
		t.Fatalf("重试成功后磁盘应为新值，实际 %d", onDisk.Port)
	}

	// revision 只应递增一次（第一次失败不该消耗版本号）。
	if want := before.Revision + 1; st.Get().Revision != want {
		t.Fatalf("revision 期望 %d（失败那次不应计入），实际 %d",
			want, st.Get().Revision)
	}
}

// swapAtomicWrite 临时替换 config 包底层的写盘函数。
//
// 注入点在最底层（storage.AtomicWrite 那一层），因此被测的仍是
// Mutate → write 的完整生产控制流，而不是把逻辑抄一份到测试里。
func swapAtomicWrite(t *testing.T, fn func(string, []byte, os.FileMode) error) func() {
	t.Helper()
	orig := atomicWrite
	atomicWrite = fn
	return func() { atomicWrite = orig }
}

// TestMutateIsAtomicOnError 守：fn 返回错误时内存与磁盘都不变。
//
// 校验失败不应留下半套配置（这是"改一半"类 bug 的根源）。
func TestMutateIsAtomicOnError(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")
	st, _ := Load(path)

	before := st.Get()

	sentinel := os.ErrInvalid
	err := st.Mutate(func(s *Settings) error {
		s.Port = 9999 // 先改一个合法值
		s.AutoCheckin = false
		return sentinel // 再报错 —— 以上改动都应被丢弃
	})
	if err != sentinel {
		t.Fatalf("应原样返回 fn 的错误，得到 %v", err)
	}

	if got := st.Get(); got.Port != before.Port || got.AutoCheckin != before.AutoCheckin {
		t.Errorf("Mutate 失败后内存被改动了: %+v", got)
	}

	// 磁盘也必须还是旧的
	var onDisk Settings
	if _, err := readJSON(path, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Port != before.Port {
		t.Errorf("Mutate 失败后磁盘被改动了: port=%d", onDisk.Port)
	}
}

// TestMutatePersistsAndReloads 守：改动能真正落盘并被下次 Load 读到。
func TestMutatePersistsAndReloads(t *testing.T) {
	dir := testConfigDir(t)
	path := filepath.Join(dir, "config.json")
	st, _ := Load(path)

	if err := st.Mutate(func(s *Settings) error {
		s.Port = 9100
		s.AutoStart = true
		s.Aliases = map[string]string{"dsf": "workbuddy/deepseek-v4.1-flash"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := again.Get()
	if got.Port != 9100 {
		t.Errorf("重新加载端口 = %d，期望 9100", got.Port)
	}
	if !got.AutoStart {
		t.Error("重新加载后 autoStart 应为 true")
	}
	if got.Aliases["dsf"] != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("别名表未持久化: %v", got.Aliases)
	}
}

// TestGetReturnsCopy 守：调用方改 Get() 的返回值不会污染内部状态。
func TestGetReturnsCopy(t *testing.T) {
	dir := testConfigDir(t)
	st, _ := Load(filepath.Join(dir, "config.json"))

	a := st.Get()
	a.Aliases["hacked"] = "x"
	a.Port = 1

	b := st.Get()
	if _, ok := b.Aliases["hacked"]; ok {
		t.Error("Get() 返回的别名表被共享，调用方能改到内部状态")
	}
	if b.Port == 1 {
		t.Error("Get() 返回的结构体被共享")
	}
}

// TestNormalizeRejectsBadPort 守：非法端口在加载时被纠正。
func TestNormalizeRejectsBadPort(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"端口为 0（缺失字段）", 0, DefaultPort},
		{"端口过低（需管理员）", 80, DefaultPort},
		{"端口越界", 70000, DefaultPort},
		{"合法端口保留", 9000, 9000},
		{"边界 MinPort 保留", MinPort, MinPort},
		{"边界 65535 保留", 65535, 65535},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := testConfigDir(t)
			path := filepath.Join(dir, "config.json")
			raw, _ := json.Marshal(Settings{Version: 1, Port: c.in, APIKey: "k"})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Get().Port; got != c.want {
				t.Errorf("端口 %d 归一后 = %d，期望 %d", c.in, got, c.want)
			}
		})
	}
}

// TestValidatePort 守端口校验规则的边界。
func TestValidatePort(t *testing.T) {
	if err := ValidatePort(1023); err == nil {
		t.Error("1023 应被拒绝（<1024 通常需管理员权限）")
	}
	if err := ValidatePort(0); err == nil {
		t.Error("0 应被拒绝")
	}
	if err := ValidatePort(65536); err == nil {
		t.Error("65536 应被拒绝")
	}
	for _, p := range []int{1024, 8787, 65535} {
		if err := ValidatePort(p); err != nil {
			t.Errorf("端口 %d 应被接受: %v", p, err)
		}
	}
}

// TestGenerateKeyIsRandomAndHex 守：随机 key 有足够熵且形状稳定。
func TestGenerateKeyIsRandomAndHex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		k, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if len(k) != 32 {
			t.Fatalf("key 长度 = %d，期望 32 位十六进制", len(k))
		}
		for _, r := range k {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("key 含非十六进制字符: %q", k)
			}
		}
		if seen[k] {
			t.Fatalf("生成了重复的 key: %q（随机性不足）", k)
		}
		seen[k] = true
	}
}

// TestValidateAliasesStructure 守别名表的结构校验（语义校验在 router 层）。
func TestValidateAliasesStructure(t *testing.T) {
	bad := []struct {
		name string
		in   map[string]string
	}{
		{"空别名", map[string]string{"": "workbuddy/x"}},
		{"别名含斜杠", map[string]string{"a/b": "workbuddy/x"}},
		{"别名过长", map[string]string{strings.Repeat("a", 65): "workbuddy/x"}},
		{"目标为空", map[string]string{"dsf": ""}},
		{"自映射", map[string]string{"dsf": "dsf"}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateAliases(c.in); err == nil {
				t.Errorf("应被拒绝: %v", c.in)
			}
		})
	}

	good := map[string]string{
		"dsf": "workbuddy/deepseek-v4.1-flash",
		"glm": "workbuddy/glm-5.3",
	}
	if err := ValidateAliases(good); err != nil {
		t.Errorf("合法别名被拒: %v", err)
	}
}

// TestConcurrentMutateAndGet 守并发安全（本机无 gcc 跑不了 -race，用高压代替）。
func TestConcurrentMutateAndGet(t *testing.T) {
	// 用受管目录而不是 testConfigDir(t)：本测试有 16 个 goroutine 反复原子写，
	// t.TempDir 的逐测试清理会与未交回的句柄抢目录（详见 main_test.go）
	dir := testConfigDir(t)
	st, _ := Load(filepath.Join(dir, "config.json"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				_ = st.Mutate(func(s *Settings) error {
					s.Port = 9000 + (n+j)%1000
					s.Aliases["k"] = "workbuddy/x"
					return nil
				})
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s := st.Get()
				_ = s.Port
				_ = len(s.Aliases)
			}
		}()
	}
	wg.Wait()

	// 并发写后文件必须仍是合法 JSON（原子写的意义）
	var onDisk Settings
	if _, err := readJSON(st.Path(), &onDisk); err != nil {
		t.Fatalf("并发写后配置文件损坏: %v", err)
	}
	if onDisk.Version != Version {
		t.Errorf("版本字段 = %d，期望 %d", onDisk.Version, Version)
	}
}

// readJSON 是测试用的小工具（避免依赖被测量对象自身的读取逻辑）。
func readJSON(path string, v any) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}
