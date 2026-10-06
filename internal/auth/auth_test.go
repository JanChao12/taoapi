package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/pool"
)

// xorCodec 是确定性测试 codec（不用真 DPAPI，测试要能在 CI/其他地方跑）。
type xorCodec struct {
	key byte
	// fail 为 true 时加密失败，用于验证"绝不退回明文"。
	fail bool
}

var errCodecFail = errors.New("测试 codec 故意失败")

func (c xorCodec) Name() string { return "xor-test" }

func (c xorCodec) Encrypt(p []byte) ([]byte, error) {
	if c.fail {
		return nil, errCodecFail
	}
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ c.key
	}
	return out, nil
}

func (c xorCodec) Decrypt(p []byte) ([]byte, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ c.key
	}
	return out, nil
}

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "accounts.json")
}

func sampleAccount() *Account {
	return &Account{
		UID:            "aaaaaaaa-1111-2222-3333-444444444444",
		Nickname:       "13800000001",
		EnterpriseID:   "ent-123",
		Domain:         "www.codebuddy.cn",
		AccessToken:    "access-token-SECRET-value",
		RefreshToken:   "refresh-token-SECRET-value",
		TokenExpiresAt: 1794405600,
		Status:         pool.StatusNormal,
		Credit: CreditSnapshot{
			Known:     true,
			Remaining: 3124,
			At:        time.Date(2026, 10, 4, 23, 28, 0, 0, time.UTC),
			Packages: []PackageSnapshot{
				{Name: "个人版国内运营裂变包", Remain: 70, ExpireAt: "2026-10-05"},
				{Name: "个人版国内运营裂变包", Remain: 100, ExpireAt: "2026-10-06"},
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────
// 🔴 安全：凭据必须加密，且失败时绝不退回明文
// ─────────────────────────────────────────────────────────────

// TestTokensAreEncryptedOnDisk 断言 accessToken / refreshToken 不会明文落盘。
func TestTokensAreEncryptedOnDisk(t *testing.T) {
	path := tmpPath(t)
	p := NewPersister(path, xorCodec{key: 0x5A})

	st := NewStore()
	st.Put(sampleAccount())
	if err := p.Save(st); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// ⚠️ 关键断言：明文 token 不能出现在文件里
	for _, secret := range []string{
		"access-token-SECRET-value",
		"refresh-token-SECRET-value",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("🔴 凭据明文落盘了！文件里出现了 %q", secret)
		}
	}

	// 非秘密字段应该能明文读到（面板需要）
	if !strings.Contains(string(raw), "aaaaaaaa") {
		t.Error("UID 应以明文存储，供面板与诊断读取")
	}
	if !strings.Contains(string(raw), "13800000001") {
		t.Error("昵称应以明文存储")
	}
}

// TestSaveNeverFallsBackToPlaintext 断言加密失败时【不写文件】而不是写明文。
func TestSaveNeverFallsBackToPlaintext(t *testing.T) {
	path := tmpPath(t)
	p := NewPersister(path, xorCodec{fail: true})

	st := NewStore()
	st.Put(sampleAccount())

	err := p.Save(st)
	if err == nil {
		t.Fatal("加密失败时 Save 应返回错误")
	}
	if !errors.Is(err, errCodecFail) {
		t.Errorf("错误应包裹底层原因，实际 %v", err)
	}

	// 文件必须不存在（绝不能留下明文）
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("🔴 加密失败却写了文件！这就是明文泄露路径")
	}
}

// TestRoundTrip 验证存取一致。
func TestRoundTrip(t *testing.T) {
	path := tmpPath(t)
	p := NewPersister(path, xorCodec{key: 0x33})

	in := sampleAccount()
	in.Status = pool.StatusRateLimited
	in.StatusReason = "上游 429"
	in.StatusUntil = time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC)
	in.ManualDisabled = false
	in.CheckinDay = "2026-10-04"

	st := NewStore()
	st.Put(in)
	if err := p.Save(st); err != nil {
		t.Fatal(err)
	}

	got, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != 1 {
		t.Fatalf("账号数 = %d，期望 1", got.Len())
	}

	a, ok := got.Get(in.UID)
	if !ok {
		t.Fatal("找不到账号")
	}

	if a.AccessToken != in.AccessToken {
		t.Error("AccessToken 未正确还原")
	}
	if a.RefreshToken != in.RefreshToken {
		t.Error("RefreshToken 未正确还原")
	}
	if a.EnterpriseID != in.EnterpriseID {
		t.Error("EnterpriseID 未正确还原")
	}
	if a.Nickname != in.Nickname {
		t.Error("Nickname 未正确还原")
	}
	if a.Status != pool.StatusRateLimited {
		t.Errorf("Status = %q，期望 rate_limited", a.Status)
	}
	if a.StatusReason != "上游 429" {
		t.Errorf("StatusReason = %q", a.StatusReason)
	}
	if !a.StatusUntil.Equal(in.StatusUntil) {
		t.Errorf("StatusUntil = %v，期望 %v", a.StatusUntil, in.StatusUntil)
	}
	if a.CheckinDay != "2026-10-04" {
		t.Errorf("CheckinDay = %q", a.CheckinDay)
	}
	if !a.Credit.Known || a.Credit.Remaining != 3124 {
		t.Errorf("额度快照未还原: %+v", a.Credit)
	}
	if len(a.Credit.Packages) != 2 {
		t.Fatalf("额度包数 = %d，期望 2", len(a.Credit.Packages))
	}
	if a.Credit.Packages[0].ExpireAt != "2026-10-05" {
		t.Errorf("到期日未还原: %q", a.Credit.Packages[0].ExpireAt)
	}
}

// TestLoadMissingFileReturnsEmptyStore 验证首次运行（无文件）不算错误。
func TestLoadMissingFileReturnsEmptyStore(t *testing.T) {
	p := NewPersister(tmpPath(t), xorCodec{})
	st, err := p.Load()
	if err != nil {
		t.Fatalf("文件不存在不应报错，实际 %v", err)
	}
	if st.Len() != 0 {
		t.Errorf("应为空仓库，实际 %d 个账号", st.Len())
	}
}

// TestLoadWrongCodecGivesClearError 验证换了 codec 时给出可操作的错误。
func TestLoadWrongCodecGivesClearError(t *testing.T) {
	path := tmpPath(t)

	st := NewStore()
	st.Put(sampleAccount())
	if err := NewPersister(path, xorCodec{key: 1}).Save(st); err != nil {
		t.Fatal(err)
	}

	// 用另一个 codec 名读
	other := NewPersister(path, namedCodec{xorCodec{key: 1}, "other"})
	_, err := other.Load()
	if err == nil {
		t.Fatal("codec 不匹配应报错")
	}
	if !strings.Contains(err.Error(), "重新导入") {
		t.Errorf("错误信息应提示怎么办（重新导入），实际: %v", err)
	}
}

type namedCodec struct {
	xorCodec
	name string
}

func (c namedCodec) Name() string { return c.name }

// TestOneCorruptAccountDoesNotKillOthers 验证单个坏账号不影响其余。
func TestOneCorruptAccountDoesNotKillOthers(t *testing.T) {
	path := tmpPath(t)
	codec := xorCodec{key: 0x11}

	good := sampleAccount()
	good.UID = "aaaaaaaa-1111-2222-3333-444444444444"

	df := diskFile{
		Version: accountsFileVersion,
		Accounts: []diskAcct{
			{UID: "bbbbbbbb-1111-2222-3333-444444444444", Secrets: "{不是合法 JSON"},
		},
	}
	// 用 persister 正常编码一个号
	p := NewPersister(path, codec)
	enc, err := p.encode(good)
	if err != nil {
		t.Fatal(err)
	}
	df.Accounts = append(df.Accounts, enc)

	raw, _ := json.Marshal(df)
	// 手写文件（绕过 Save 的加密，直接放明文 JSON —— 解码时会失败）
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := p.Load()
	// 期望：好账号被加载，同时返回第一条错误
	if st.Len() != 1 {
		t.Fatalf("应加载 1 个好账号，实际 %d", st.Len())
	}
	if _, ok := st.Get(good.UID); !ok {
		t.Error("好账号未被加载")
	}
	if err == nil {
		t.Error("有坏账号时应返回非 nil 错误供调用方告警")
	}
}

// ─────────────────────────────────────────────────────────────
// 状态机
// ─────────────────────────────────────────────────────────────

// TestEffectiveStatusCooldownExpiry 验证冷却到期自动恢复。
func TestEffectiveStatusCooldownExpiry(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		acct Account
		want pool.Status
	}{
		{
			"限流冷却中",
			Account{Status: pool.StatusRateLimited, StatusUntil: now.Add(time.Hour)},
			pool.StatusRateLimited,
		},
		{
			"限流冷却已过 → 恢复",
			Account{Status: pool.StatusRateLimited, StatusUntil: now.Add(-time.Hour)},
			pool.StatusNormal,
		},
		{
			"临时故障冷却已过 → 恢复",
			Account{Status: pool.StatusTransient, StatusUntil: now.Add(-time.Minute)},
			pool.StatusNormal,
		},
		{
			"未知冷却已过 → 恢复（不永久排斥）",
			Account{Status: pool.StatusUnknown, StatusUntil: now.Add(-time.Minute)},
			pool.StatusNormal,
		},
		{
			"封号不会被时间治愈",
			Account{Status: pool.StatusBanned, StatusUntil: now.Add(-time.Hour)},
			pool.StatusBanned,
		},
		{
			"凭证失效不会被时间治愈",
			Account{Status: pool.StatusAuthExpired, StatusUntil: now.Add(-time.Hour)},
			pool.StatusAuthExpired,
		},
		{
			"人工禁用优先于一切",
			Account{Status: pool.StatusNormal, ManualDisabled: true},
			pool.StatusDisabled,
		},
		{
			"零值状态视为正常",
			Account{},
			pool.StatusNormal,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.acct.EffectiveStatus(now); got != c.want {
				t.Errorf("EffectiveStatus = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestToPoolPreservesSchedulingInputs 验证转成调度视图时字段不丢。
func TestToPoolPreservesSchedulingInputs(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	a := sampleAccount().ToPool(now)

	if a.ID != "aaaaaaaa-1111-2222-3333-444444444444" {
		t.Errorf("ID = %q", a.ID)
	}
	if !a.CreditsKnown {
		t.Error("CreditsKnown 丢了 —— 会导致调度把'没查过'当成'没额度'")
	}
	if a.Credits != 3124 {
		t.Errorf("Credits = %d", a.Credits)
	}
	if len(a.Packages) != 2 {
		t.Fatalf("包数 = %d", len(a.Packages))
	}
	if got := a.EarliestExpiry(); got != "2026-10-05" {
		t.Errorf("EarliestExpiry = %q，期望 2026-10-05", got)
	}
}

// TestCreditSnapshotEarliestExpiryFilters 验证快照的最早到期日也过滤无效包。
func TestCreditSnapshotEarliestExpiryFilters(t *testing.T) {
	s := CreditSnapshot{Packages: []PackageSnapshot{
		{Name: "零额度", Remain: 0, ExpireAt: "2026-10-01"},
		{Name: "非法日期", Remain: 5, ExpireAt: "oops"},
		{Name: "空日期", Remain: 5, ExpireAt: ""},
		{Name: "有效", Remain: 5, ExpireAt: "2026-10-20"},
		{Name: "有效更早", Remain: 5, ExpireAt: "2026-10-10"},
	}}
	if got := s.EarliestExpiry(); got != "2026-10-10" {
		t.Errorf("EarliestExpiry = %q，期望 2026-10-10", got)
	}
}

// TestStoreCRUD 验证仓库基本操作。
func TestStoreCRUD(t *testing.T) {
	st := NewStore()
	a := sampleAccount()

	if _, ok := st.Get(a.UID); ok {
		t.Error("空仓库不该有账号")
	}

	st.Put(a)
	if _, ok := st.Get(a.UID); !ok {
		t.Error("Put 后应能取到")
	}
	if st.Len() != 1 {
		t.Errorf("Len = %d", st.Len())
	}

	// 空 UID 不应入仓
	st.Put(&Account{UID: ""})
	st.Put(nil)
	if st.Len() != 1 {
		t.Errorf("空 UID / nil 不该改变仓库大小，实际 %d", st.Len())
	}

	if !st.Remove(a.UID) {
		t.Error("Remove 应返回 true")
	}
	if st.Remove(a.UID) {
		t.Error("重复 Remove 应返回 false")
	}
}

// TestListIsSorted 验证 List 顺序稳定（面板渲染需要）。
func TestListIsSorted(t *testing.T) {
	st := NewStore()
	st.Put(&Account{UID: "ccc"})
	st.Put(&Account{UID: "aaa"})
	st.Put(&Account{UID: "bbb"})

	list := st.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].UID > list[i].UID {
			t.Fatalf("List 未排序: %s 在 %s 之前", list[i-1].UID, list[i].UID)
		}
	}
}

// TestMaskUID 验证掩码不泄露完整 UID。
func TestMaskUID(t *testing.T) {
	full := "aaaaaaaa-1111-2222-3333-444444444444"
	masked := MaskUID(full)
	if masked == full {
		t.Error("掩码不应等于原文")
	}
	if strings.Contains(masked, "1111") {
		t.Errorf("掩码泄露了后半段: %q", masked)
	}
	if got := MaskUID("short"); got != "***" {
		t.Errorf("短 UID 应全掩码，实际 %q", got)
	}
}
