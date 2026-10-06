package app

import (
	"io"
	"log"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
)

// ─────────────────────────────────────────────────────────────
// 国际版「没有签到活动」的护栏测试（2026-10-06 实测）
//
// 🔴 实测证据：
//
//	对国际版账号调 Checkin **不报错**，但返回
//	"签到活动未开启或已过期" —— 端点存在、活动不存在。
//
// ⇒ 若照常显示签到按钮/返回"成功"，用户会看到成功却毫无效果。
//	这是**误导**，比报错更糟（报错至少用户知道没成）。
// ─────────────────────────────────────────────────────────────

// TestPlatformHasCheckinForAccount 守：按平台判定是否有签到。
func TestPlatformHasCheckinForAccount(t *testing.T) {
	cn := &auth.Account{UID: "cn-1"}
	if !platformHasCheckin(cn) {
		t.Error("国内账号应有签到（实测国内版有签到活动）")
	}

	intl := &auth.Account{UID: "intl-1", Platform: auth.PlatformIntl}
	if platformHasCheckin(intl) {
		t.Error("国际账号不该有签到 —— 实测返回「签到活动未开启或已过期」，" +
			"继续显示签到按钮会让用户看到「成功」却毫无效果")
	}

	// nil 不该 panic，且按"没有签到"处理（保守）
	if platformHasCheckin(nil) {
		t.Error("nil 账号应按无签到处理")
	}
	// 平台字段为空的老账号 ⇒ 国内版 ⇒ 有签到
	legacy := &auth.Account{UID: "legacy-1"}
	if !platformHasCheckin(legacy) {
		t.Error("老账号（无 platform 字段）应归一为国内版，有签到")
	}
}

// TestCheckinDaemonSkipsIntlEntirely 守：守护进程**完全不计入**国际账号。
//
// 🔴 关键点：不能只"跳过调用"，还要**不计入 any**。
//
//	若只有国际版账号时 any 仍为 true，调用方会认为"有账号可签"，
//	于是走完整流程却什么都没做 —— 语义就错了。
func TestCheckinDaemonSkipsIntlEntirely(t *testing.T) {
	st := auth.NewStore()
	// 只有一个国际版账号
	st.Put(&auth.Account{UID: "intl-only", Platform: auth.PlatformIntl})

	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: st,
		// WBClient 故意留 nil：若实现错误地尝试调用，
		// 会走到 providerForAccount 的分支 —— 但 any 的判断才是本测试重点
	}

	stop := make(chan struct{})
	defer close(stop)

	ok, failed, skippedNoAccounts := runDueCheckins(deps, stop)

	if ok != 0 || failed != 0 {
		t.Errorf("国际版账号不该产生签到结果，实际 ok=%d failed=%d", ok, failed)
	}
	if !skippedNoAccounts {
		t.Error("只有国际版账号时应报「没有可签账号」—— " +
			"否则调用方以为有号可签，走完整流程却什么都不做")
	}
}

// TestCheckinDaemonStillHandlesCN 守：国内账号仍正常处理（别误伤）。
func TestCheckinDaemonStillHandlesCN(t *testing.T) {
	st := auth.NewStore()
	// 国内账号 + 今天已签 ⇒ 应被计入 ok（走的是"已签跳过"分支，无网络开销）
	a := &auth.Account{UID: "cn-signed"}
	a.CheckinDay = todayCN()
	st.Put(a)

	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: st,
		// ⚠️ 必须给非 nil 的 WBClient：runDueCheckins 在 WBClient==nil 时
		//    **直接早退**（生产的必要保护：没有上游客户端就没法签到）。
		//    本测试走"已签跳过"分支，不会真发请求，假地址即可。
		WBClient: newWorkbuddyClientForTest(t, "http://127.0.0.1:1"),
	}
	stop := make(chan struct{})
	defer close(stop)

	ok, failed, skippedNoAccounts := runDueCheckins(deps, stop)

	if skippedNoAccounts {
		t.Error("国内账号存在时不该报「没有可签账号」")
	}
	if ok != 1 || failed != 0 {
		t.Errorf("已签的国内账号应计入 ok=1，实际 ok=%d failed=%d", ok, failed)
	}
}

// TestCheckinMixedPlatforms 守：混合场景下只处理国内的。
func TestCheckinMixedPlatforms(t *testing.T) {
	st := auth.NewStore()
	cn := &auth.Account{UID: "cn-1"}
	cn.CheckinDay = todayCN()
	st.Put(cn)
	st.Put(&auth.Account{UID: "intl-1", Platform: auth.PlatformIntl})

	deps := Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: st,
		WBClient: newWorkbuddyClientForTest(t, "http://127.0.0.1:1"),
	}
	stop := make(chan struct{})
	defer close(stop)

	ok, failed, skippedNoAccounts := runDueCheckins(deps, stop)

	if skippedNoAccounts {
		t.Error("有国内账号时不该报「没有可签账号」")
	}
	if ok != 1 {
		t.Errorf("只应处理国内那 1 个（已签 ⇒ ok=1），实际 ok=%d failed=%d", ok, failed)
	}
}
