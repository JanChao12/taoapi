package workbuddy

import (
	"context"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/testutil"
)

// ─────────────────────────────────────────────────────────────
// 平台（国内版 / 国际版）差异的护栏测试（2026-10-06）
//
// 这些差异全部来自**实测**，不是推断。详见
// codex-consult/43-国际版全项实测.md
// ─────────────────────────────────────────────────────────────

// TestPlatformProviderIDs 守：两版前缀不同（否则同名模型互相覆盖）。
//
// 🔴 委托方 2026-10-06 定：国际版用 `workbuddyai/`。
//
// 为什么必须不同：两版模型集**部分重叠**（`glm-5.3` 两边都有），
// 用同一个前缀会让路由表里只剩一个，另一个永远调不到。
func TestPlatformProviderIDs(t *testing.T) {
	if PlatformCN.ProviderID() != "workbuddy" {
		t.Errorf("国内版前缀 = %q，期望 workbuddy", PlatformCN.ProviderID())
	}
	if PlatformIntl.ProviderID() != "workbuddyai" {
		t.Errorf("国际版前缀 = %q，期望 workbuddyai", PlatformIntl.ProviderID())
	}
	if PlatformCN.ProviderID() == PlatformIntl.ProviderID() {
		t.Fatal("两版前缀相同 —— 同名模型会互相覆盖")
	}
}

// TestPlatformBaseURLs 守：两版基址（实测值）。
func TestPlatformBaseURLs(t *testing.T) {
	cnChat, cnBilling := PlatformCN.BaseURLs()
	if cnChat != "https://copilot.tencent.com" || cnBilling != "https://www.codebuddy.cn" {
		t.Errorf("国内版基址 = (%q, %q)，与实测不符", cnChat, cnBilling)
	}

	intlChat, intlBilling := PlatformIntl.BaseURLs()
	if intlChat != "https://www.workbuddy.ai" {
		t.Errorf("国际版 chat 基址 = %q", intlChat)
	}
	// 🔴 国际版 chat 与 billing **同域名**（与国内版不同）—— 实测确认
	if intlChat != intlBilling {
		t.Errorf("国际版 chat 与 billing 应同域名，实际 %q / %q", intlChat, intlBilling)
	}
}

// TestPlatformNeedsSystemMessage 守：国际版要求首条消息是 system。
//
// 🔴 实测（2026-10-06）：
//
//	国际版无 system 首条 → HTTP 400
//	{"code":11-128,"msg":"first message is not system prompt"}
//	加上 system 后同一个请求立刻成功（回复 PONG）。
//	国内版**没有**这个约束。
func TestPlatformNeedsSystemMessage(t *testing.T) {
	if !PlatformIntl.NeedsSystemMessage() {
		t.Error("国际版应要求 system 首条消息（实测 400）")
	}
	if PlatformCN.NeedsSystemMessage() {
		t.Error("国内版不应要求 system 首条消息")
	}
}

// TestPlatformHasCheckin 守：国际版没有签到活动。
//
// 🔴 实测：国际版签到端点**调用不报错**，但返回
//
//	"签到活动未开启或已过期"
//
// ⇒ 面板若仍显示签到按钮，用户点了会看到"成功"却毫无效果 —— 误导。
func TestPlatformHasCheckin(t *testing.T) {
	if !PlatformCN.HasCheckin() {
		t.Error("国内版应有签到")
	}
	if PlatformIntl.HasCheckin() {
		t.Error("国际版实测没有签到活动（返回「活动未开启或已过期」）")
	}
}

// TestPlatformDropsAbstractModels 守：抽象档位只在国际版被砍。
func TestPlatformDropsAbstractModels(t *testing.T) {
	if !PlatformIntl.DropsAbstractModels() {
		t.Error("国际版应砍掉抽象档位")
	}
	if PlatformCN.DropsAbstractModels() {
		t.Error("国内版目录里本来就没有抽象档位，不该启用这个开关")
	}
	// 5 个抽象档位都要识别出来
	for _, id := range []string{
		"default-model", "fast-model", "balanced-model",
		"primary-model", "deep-model",
	} {
		if !isAbstractModel(id) {
			t.Errorf("%q 应被识别为抽象档位（委托方明确要求砍掉）", id)
		}
	}
	// 正常模型不能被误判
	for _, id := range []string{"glm-5.3", "gpt-5.6-luna", "auto"} {
		if isAbstractModel(id) {
			t.Errorf("%q 不是抽象档位，被误判了", id)
		}
	}
}

// TestNewProviderForDoesNotOverrideBases 守：构造函数**不得**改基址。
//
// 🔴 这是一个真实踩过的坑（别改回去）：
//
//	第一版我在 NewProviderFor 里顺手 SetBases(平台基址)，
//	结果测试的写法是 `SetBases(fake)` 之后才 `NewProvider` ——
//	构造函数把基址**覆盖回生产地址**，测试于是**打到了真实上游**，
//	表现为大规模 401，且测试**从隔离变成了联网**。
//
// 这条测试直接验证"构造函数不碰基址"。
func TestNewProviderForDoesNotOverrideBases(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	c := NewClient()
	c.SetBases(fake.URL, fake.URL)

	// 构造国际版 provider —— 基址必须**保持** fake，不能被改成 www.workbuddy.ai
	p := NewProviderFor(c, testCred(), PlatformIntl)

	if got := p.Client().ChatURL(); !strings.HasPrefix(got, fake.URL) {
		t.Errorf("构造后 ChatURL = %q，应仍指向假上游 %q —— "+
			"构造函数覆盖基址会让测试打到真实上游", got, fake.URL)
	}
	if got := p.Client().ModelsURL(); !strings.HasPrefix(got, fake.URL) {
		t.Errorf("构造后 ModelsURL = %q，应仍指向假上游", got)
	}
	// 但平台标识应生效
	if p.ID() != ProviderIDIntl {
		t.Errorf("ID() = %q，期望 %q", p.ID(), ProviderIDIntl)
	}
}

// TestModelsUsesPlatformPrefix 守：模型 ID 用**平台**前缀，不是写死国内版。
func TestModelsUsesPlatformPrefix(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")

	c := NewClient()
	c.SetBases(fake.URL, fake.URL)

	intl := NewProviderFor(c, testCred(), PlatformIntl)
	models, err := intl.Models(context.Background())
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("假上游没返回模型")
	}
	for _, m := range models {
		if !strings.HasPrefix(m.ID, ProviderIDIntl+"/") {
			t.Errorf("国际版模型 ID = %q，应以 %q 开头", m.ID, ProviderIDIntl)
		}
	}
}
