package login

import (
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 平台化登录的护栏（2026-10-08：支持国际版账号添加）
// ═══════════════════════════════════════════════════════════════════
//
// 背景：改造前登录页与凭据捕获**都写死国内版** www.codebuddy.cn，
// 导致国际版账号无法通过「网页登录」添加（委托人实测反馈）。
//
// 🔴 本次改动的**最大风险**是"两处平台不一致"：
//
//	登录页用国际版、凭据白名单还是国内版（或反过来）
//	⇒ 用户在自己账号的站点登录成功，程序却在等另一个域名的响应
//	⇒ **凭据永远抓不到**，界面表现为"一直转圈"，
//	   且没有任何错误线索指向真正的原因。
//
// 所以下面同时守三件事：
//  1. 登录页 URL 按平台正确切换
//  2. 凭据白名单按平台正确切换
//  3. 白名单**只有两个固定域名**，绝不通配（安全边界）

// TestLoginURLForSwitchesHost 守登录页按平台切换。
func TestLoginURLForSwitchesHost(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"

	cn := LoginURLFor(PlatformCN, st)
	if !strings.HasPrefix(cn, "https://www.codebuddy.cn/login/") {
		t.Errorf("国内版登录页 = %q，应指向 www.codebuddy.cn", cn)
	}
	if !strings.Contains(cn, st) {
		t.Errorf("国内版登录页应含 state: %q", cn)
	}

	intl := LoginURLFor(PlatformIntl, st)
	if !strings.HasPrefix(intl, "https://www.workbuddy.ai/login/") {
		t.Errorf("国际版登录页 = %q，应指向 www.workbuddy.ai", intl)
	}
	if !strings.Contains(intl, st) {
		t.Errorf("国际版登录页应含 state: %q", intl)
	}

	// 两者必须是**不同**的 URL —— 否则"选平台"就没意义了。
	if cn == intl {
		t.Fatal("🔴 两个平台的登录页 URL 相同 —— 平台切换没生效")
	}
}

// TestCredentialMatcherIsPlatformSpecific 是本次改造的**核心安全断言**。
func TestCredentialMatcherIsPlatformSpecific(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"

	cnURL := "https://www.codebuddy.cn/console/login/enterprise?state=" + st
	intlURL := "https://www.workbuddy.ai/console/login/enterprise?state=" + st

	// 国内版匹配器：认国内版，不认国际版。
	if !newProdCredMatcher(PlatformCN).matches(cnURL, credentialEndpointMethod, st) {
		t.Error("国内版匹配器应接受 www.codebuddy.cn")
	}
	if newProdCredMatcher(PlatformCN).matches(intlURL, credentialEndpointMethod, st) {
		t.Error("🔴 国内版匹配器**不该**接受 www.workbuddy.ai —— " +
			"那意味着白名单被放宽（任意站点都能喂凭据给我们）")
	}

	// 国际版匹配器：认国际版，不认国内版。
	if !newProdCredMatcher(PlatformIntl).matches(intlURL, credentialEndpointMethod, st) {
		t.Error("国际版匹配器应接受 www.workbuddy.ai —— " +
			"否则国际版登录会「登录成功但抓不到凭据」")
	}
	if newProdCredMatcher(PlatformIntl).matches(cnURL, credentialEndpointMethod, st) {
		t.Error("🔴 国际版匹配器**不该**接受 www.codebuddy.cn")
	}

	// 零值（未指定平台）保守回退国内版 —— 绝不能变成"任意 host 都接受"。
	if !newProdCredMatcher().matches(cnURL, credentialEndpointMethod, st) {
		t.Error("未指定平台时应保守按国内版处理")
	}
	if newProdCredMatcher().matches(intlURL, credentialEndpointMethod, st) {
		t.Error("🔴 未指定平台时不该接受国际版")
	}
}

// TestCredentialWhitelistRejectsLookalikes 守：白名单是**精确匹配**，
// 不能被相似域名、子域名、后缀伪装穿透。
//
// ⚠️ 国际版加入后，必须再走一遍这组负例 —— 因为
// "多一个白名单域名"很容易被顺手写成后缀/包含匹配。
func TestCredentialWhitelistRejectsLookalikes(t *testing.T) {
	const st = "11111111-2222-4333-8444-555555555555"

	for _, platform := range []Platform{PlatformCN, PlatformIntl} {
		for _, bad := range []string{
			// 国内版的仿冒
			"https://evil.example.com/console/login/enterprise?state=" + st,
			"https://www.codebuddy.cn.evil.com/console/login/enterprise?state=" + st,
			"https://evilcodebuddy.cn/console/login/enterprise?state=" + st,
			"https://www.codebuddy.com.cn/console/login/enterprise?state=" + st,
			// 国际版的仿冒
			"https://www.workbuddy.ai.evil.com/console/login/enterprise?state=" + st,
			"https://evilworkbuddy.ai/console/login/enterprise?state=" + st,
			"https://www.workbuddy.com/console/login/enterprise?state=" + st,
			"https://workbuddy.ai/console/login/enterprise?state=" + st, // 少了 www
			// 协议/路径变体
			"http://www.workbuddy.ai/console/login/enterprise?state=" + st,
			"https://www.workbuddy.ai/console/login/enterpriseX?state=" + st,
			"https://www.workbuddy.ai/console/login/?state=" + st,
			"https://www.workbuddy.ai/console/login/enterprise/extra?state=" + st,
		} {
			m := newProdCredMatcher(platform)
			if m.matches(bad, credentialEndpointMethod, st) {
				t.Errorf("🔴 platform=%s 的匹配器接受了不该接受的 URL: %s", platform, bad)
			}
		}
	}
}

// TestCredentialHostFor 守 host 映射正确且**只有两个**。
func TestCredentialHostFor(t *testing.T) {
	if h := credentialHostFor(PlatformCN); h != "www.codebuddy.cn" {
		t.Errorf("国内版 host = %q", h)
	}
	if h := credentialHostFor(PlatformIntl); h != "www.workbuddy.ai" {
		t.Errorf("国际版 host = %q", h)
	}
	// 未知平台保守回退国内版（不回退成"任意"）。
	if h := credentialHostFor(Platform("bogus")); h != "www.codebuddy.cn" {
		t.Errorf("未知平台应回退国内版，实际 %q", h)
	}
}

// TestPlatformValuesMatchAuth 守 login.Platform 的字符串值与 auth 包一致。
//
// login 刻意不 import auth（底层工具包不该依赖领域模型），
// 所以两边是各自定义的字符串常量 —— 必须用测试钉住一致性，
// 否则 app 层做类型转换时会静默对不上（表现为"国际版登成了国内版"）。
func TestPlatformValuesMatchAuth(t *testing.T) {
	// 这些字面量必须与 internal/auth/account.go 的 PlatformCN/PlatformIntl 相同。
	if string(PlatformCN) != "cn" {
		t.Errorf("PlatformCN = %q，必须与 auth.PlatformCN 一致（cn）", string(PlatformCN))
	}
	if string(PlatformIntl) != "intl" {
		t.Errorf("PlatformIntl = %q，必须与 auth.PlatformIntl 一致（intl）", string(PlatformIntl))
	}
}

// TestNormalizeAndIsKnownPlatform 守平台名归一与校验。
func TestNormalizeAndIsKnownPlatform(t *testing.T) {
	// 归一：别名都指向国际版。
	for _, alias := range []string{"intl", "INTL", " intl ", "international", "ai", "workbuddyai"} {
		if got := NormalizePlatform(alias); got != PlatformIntl {
			t.Errorf("NormalizePlatform(%q) = %q，期望 intl", alias, got)
		}
		if !IsKnownPlatform(alias) {
			t.Errorf("IsKnownPlatform(%q) 应为 true", alias)
		}
	}
	// 国内版与未知值。
	for _, s := range []string{"cn", "CN", " cn "} {
		if got := NormalizePlatform(s); got != PlatformCN {
			t.Errorf("NormalizePlatform(%q) = %q，期望 cn", s, got)
		}
	}
	if NormalizePlatform("bogus") != PlatformCN {
		t.Error("未知平台应保守回退国内版")
	}
	if IsKnownPlatform("bogus") {
		t.Error("IsKnownPlatform 对未知值应为 false（调用方据此给明确错误）")
	}
}
