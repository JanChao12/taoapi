package pool

import (
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────
// 多平台账号隔离的护栏测试（2026-10-06 接入国际版时加）
//
// 🔴 为什么这是**安全级**的约束，不是普通功能：
//
//	国内版与国际版是两套独立的账号体系，凭据不能跨域名使用。
//	一旦把国内 token 选给国际请求（或反之）：
//	  · 请求会失败（轻）
//	  · 更糟：凭据被发到了**不该去的域名**（可能触发风控）
//	所以过滤必须放在**选号**这一步 —— 那是唯一绕不过去的地方。
// ─────────────────────────────────────────────────────────────

// acctOf 造一个指定平台的可用账号。
func acctOf(id, plat string) Account {
	return Account{
		ID:           id,
		Platform:     plat,
		Credits:      100,
		CreditsKnown: true,
		Status:       StatusNormal,
	}
}

// TestPickForPlatformSelectsOnlyThatPlatform 守：只选指定平台的账号。
func TestPickForPlatformSelectsOnlyThatPlatform(t *testing.T) {
	accounts := []Account{
		acctOf("cn-1", "cn"),
		acctOf("intl-1", "intl"),
	}

	got, err := NewSelector().PickForPlatform(accounts, "intl")
	if err != nil {
		t.Fatalf("选号失败: %v", err)
	}
	if got.ID != "intl-1" {
		t.Errorf("选了 %q，期望 intl-1 —— 平台过滤失效，"+
			"会把国内账号用于国际请求（凭据跨站）", got.ID)
	}

	gotCN, err := NewSelector().PickForPlatform(accounts, "cn")
	if err != nil {
		t.Fatalf("选号失败: %v", err)
	}
	if gotCN.ID != "cn-1" {
		t.Errorf("选了 %q，期望 cn-1", gotCN.ID)
	}
}

// TestPickForPlatformNoCrossFallback 守：本平台没号时**绝不**回退到另一平台。
//
// 🔴 这是最危险的失败模式：如果国际版没号就"借用"国内号，
// 请求会把国内凭据发到 www.workbuddy.ai。必须直接报无号可用。
func TestPickForPlatformNoCrossFallback(t *testing.T) {
	// 只有国内账号，却请求国际版
	accounts := []Account{acctOf("cn-1", "cn")}

	got, err := NewSelector().PickForPlatform(accounts, "intl")
	if err == nil {
		t.Fatalf("国际版无号时不该成功，却选出了 %q —— "+
			"把国内账号用于国际请求会让凭据发到错误域名", got.ID)
	}
	if err != ErrNoAvailable {
		t.Errorf("错误 = %v，期望 ErrNoAvailable", err)
	}
}

// TestPickWithoutPlatformStillWorks 守：空平台不过滤（兼容既有调用）。
func TestPickWithoutPlatformStillWorks(t *testing.T) {
	accounts := []Account{
		acctOf("cn-1", "cn"),
		acctOf("intl-1", "intl"),
	}
	got, err := NewSelector().PickForPlatform(accounts, "")
	if err != nil {
		t.Fatalf("空平台应不过滤，却失败: %v", err)
	}
	if got.ID == "" {
		t.Error("应选出某个账号")
	}
	// 旧 API 也要保持可用
	if _, err := NewSelector().Pick(accounts); err != nil {
		t.Errorf("Pick 应仍可用: %v", err)
	}
}

// TestPickForPlatformRespectsOtherRules 守：平台过滤不与既有规则冲突。
//
// 即"平台对了但不可调度"仍要排除（禁用/冷却/无额度）。
func TestPickForPlatformRespectsOtherRules(t *testing.T) {
	accounts := []Account{
		acctOf("intl-disabled", "intl"),
		acctOf("intl-nocredit", "intl"),
		acctOf("intl-ok", "intl"),
	}
	accounts[0].Disabled = true
	accounts[1].Credits = 0

	got, err := NewSelector().PickForPlatform(accounts, "intl")
	if err != nil {
		t.Fatalf("应选出 intl-ok，却失败: %v", err)
	}
	if got.ID != "intl-ok" {
		t.Errorf("选了 %q，期望 intl-ok（禁用/无额度的应被排除）", got.ID)
	}
}

// TestPickForPlatformSkipsCooling 守：冷却中的账号即使平台对也不选。
//
// 与「上游限流」那个显示问题同源：冷却中的号不该被调度。
func TestPickForPlatformSkipsCooling(t *testing.T) {
	cooling := acctOf("intl-cooling", "intl")
	cooling.Status = StatusRateLimited
	cooling.CooldownUntil = time.Now().Add(time.Hour)

	accounts := []Account{cooling, acctOf("intl-ok", "intl")}

	got, err := NewSelector().PickForPlatform(accounts, "intl")
	if err != nil {
		t.Fatalf("应选出 intl-ok: %v", err)
	}
	if got.ID == "intl-cooling" {
		t.Error("冷却中的账号被选中了 —— 会给用户一个必然失败的请求")
	}
}
