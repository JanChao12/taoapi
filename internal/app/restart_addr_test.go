package app

import (
	"os"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 「改端口后重启不生效」的回归护栏（2026-10-07 修，本轮补测试）
// ═══════════════════════════════════════════════════════════════════
//
// 缺陷（实测复现，日志为证）：
//
//	面板把端口从 8787 改成 4567 → 点「立即重启」：
//	  03:48:13 新进程启动成功，监听 127.0.0.1:4567   ← 子进程一切正常
//	  03:48:28 重启失败: 新进程在 15s 内未通过健康检查
//	  03:48:28 已恢复到 127.0.0.1:8787               ← 好进程被杀、回滚
//
// 根因：`serve.go` 把 **listenAddr（本进程正在监听的【旧】地址）** 传给了
// `relaunchAndHandshake` 做握手探测，而 handler 已经算好的 **newAddr 被 `_` 丢弃**。
//
//	子进程**不带 --addr**（见 relaunchAndHandshake 说明）⇒ 它按**新配置**
//	监听 4567；父进程却去 **8787** 轮询 /healthz ⇒ 永远匹配不到 ⇒
//	15 秒后误判失败 ⇒ **端口改动永不生效**。
//
// ⚠️ 为什么既有的端到端测试没抓到它：
//
//	它们验的是"能不能重启"，**没验"改端口之后能否生效"**。
//	端口不变时新旧地址相同，握手照常成功 —— 缺陷完全被掩盖。
//
// 🔴 本缺陷**不是理论风险**，而是委托人真实遇到的：
//
//	「4567修改不生效」「又没有改成功」—— 面板显示"端口已保存，
//	需重启生效"，点重启后却仍连不上。

// TestHandshakeUsesPlannedNewAddr 守：握手必须探测**新**地址
// （= plannedListenAddr，以配置为准），不是本进程的旧监听地址。
//
// ⚠️ 用源码文本审查而不是真实拉起进程：
//
//	`relaunchAndHandshake` 会真的 exec 自己并做网络握手，
//	在单测里调用会拉起子进程、监听端口、污染测试环境
//	（本仓库既有 `TestRelaunchNeverPassesAddrFlag` 同样选择审查源码）。
func TestHandshakeUsesPlannedNewAddr(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatalf("读取 serve.go 失败: %v", err)
	}
	text := string(src)

	// ① 必须真的**取用** newAddr，而不是丢弃
	if !strings.Contains(text, "pendingID, clientOrigin, newAddr := deps.restart.takePending()") {
		t.Error("serve.go 应取用 takePending 的第三个返回值 newAddr（" +
			"原先写成 `_` 丢弃它，正是本缺陷的根因）")
	}

	// ② 调用 relaunchAndHandshake 时必须传 handshakeAddr（由 newAddr 派生），
	//    **不得**直接传 listenAddr
	if strings.Contains(text, "relaunchAndHandshake(listenAddr,") {
		t.Error("🔴 回归：relaunchAndHandshake 又收到了 listenAddr（旧地址）。" +
			"改端口时子进程监听新端口、而握手探测旧端口 ⇒ 必然超时，" +
			"端口改动永不生效")
	}
	if !strings.Contains(text, "relaunchAndHandshake(handshakeAddr,") {
		t.Error("relaunchAndHandshake 应收到 handshakeAddr（= 本次交接的新地址）")
	}

	// ③ handshakeAddr 必须来源于 newAddr（而不是又被赋值成 listenAddr）
	idx := strings.Index(text, "handshakeAddr := newAddr")
	if idx < 0 {
		t.Error("handshakeAddr 应初始化为 newAddr（handler 已按配置算好，" +
			"与子进程的解析规则同源）")
	}
}

// TestTakePendingCarriesNewAddr 守承载新地址的管线本身没被改坏。
//
// 这是 `TestHandshakeUsesPlannedNewAddr` 的**行为侧**补充：
// 前者审"调用点写得对不对"，本条验"值真的能传过去"。
func TestTakePendingCarriesNewAddr(t *testing.T) {
	st := newRestartState()

	st.setPending("inst-abc-rev-1", "http://127.0.0.1:8787", "127.0.0.1:4567")
	id, origin, newAddr := st.takePending()

	if id != "inst-abc-rev-1" {
		t.Errorf("restartID = %q", id)
	}
	if origin != "http://127.0.0.1:8787" {
		t.Errorf("origin = %q", origin)
	}
	// 🔴 这一条是本轮缺陷的核心：新地址必须**原样取回**
	if newAddr != "127.0.0.1:4567" {
		t.Fatalf("newAddr = %q，期望 127.0.0.1:4567 —— "+
			"新地址在交接链路上丢失，握手就会退回探测旧地址，"+
			"导致「改端口不生效」", newAddr)
	}
}

// TestPlannedListenAddrDiffersFromOldOnPortChange 用**行为**证明
// 「新旧地址不同」这件事确实会发生（也就解释了为何必须区分二者）。
//
// 若这条不成立（两者恒等），那本缺陷就无从暴露 ——
// 它能通过，说明缺陷的前提在真实配置下成立。
func TestPlannedListenAddrDiffersFromOldOnPortChange(t *testing.T) {
	settings := newSettingsWithPort(t, 4567)
	deps := Deps{Settings: settings}

	newAddr, err := plannedListenAddr(deps)
	if err != nil {
		t.Fatalf("plannedListenAddr 失败: %v", err)
	}
	oldAddr := "127.0.0.1:8787" // 本进程启动时监听的地址

	if newAddr == oldAddr {
		t.Fatal("前提不成立：改端口后新旧地址应当不同，" +
			"否则本缺陷不可能发生（测试本身失效）")
	}
	if newAddr != "127.0.0.1:4567" {
		t.Fatalf("plannedListenAddr = %q，期望 127.0.0.1:4567", newAddr)
	}
}
