package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────
// 面板「全部签到 → 自动刷新额度」的护栏测试（2026-10-06）
//
// 🔴 委托方要求（原话）：
//
//	「我点击全部签到后你签到完成应该自动刷新一次额度」
//
// 这条需求的实现**全在前端**（app.js 里串行发第二个 refresh_all 请求），
// 后端不需要改动 —— 但正因如此，**没有后端测试能覆盖它**。
// 所以下面用"读 go:embed 下发的前端脚本"的方式，把关键约束钉住。
//
// ⚠️ 局限（必须说清，别把它当成"功能已验收"）：
// 这些测试只断言**源码里的调用顺序与写法**，不执行浏览器、不验证真实
// 网络往返。真正的行为验收要靠真实服务 + 浏览器（见交接文档 §3.2）。
// 且它们跑在源码上：若改了 panel/ 却没重编 exe，测试仍会通过而
// 真实服务仍是旧的 —— 这点无法用 Go 测试发现。
// ─────────────────────────────────────────────────────────────

// TestPanelCheckinTriggersAutoRefresh 守：签到动作之后确实会再发一次刷新。
//
// 守住的具体写法：runBulk 里签到分支必须 postBulk('refresh_all')。
// 若有人把这段"优化"掉（觉得多余、或改成并发），本测试失败。
func TestPanelCheckinTriggersAutoRefresh(t *testing.T) {
	js := fetchPanelJS(t)

	// 签到后必须补一次 refresh_all
	if !strings.Contains(js, "postBulk('refresh_all')") {
		t.Error("app.js 里没有在签到后补发 refresh_all —— " +
			"委托方要求「签到完成后自动刷新一次额度」")
	}

	// 串行约束：刷新必须在签到的 .then 回调内（即签到返回后才发），
	// 不能在同一个请求链里并发发出 —— 并发会让刷新拿到旧额度。
	idxCheckin := strings.Index(js, "postBulk(action).then")
	idxRefresh := strings.Index(js, "postBulk('refresh_all')")
	if idxCheckin < 0 || idxRefresh < 0 || idxRefresh < idxCheckin {
		t.Error("额度刷新的调用点不在签到回调之后 —— " +
			"并发发出会让刷新先于签到完成，用户看到的是旧额度")
	}
}

// TestPanelBulkFailureReportedSeparately 守：签到与刷新的失败**分开报告**。
//
// 🔴 这条是委托方特别敏感的纪律（本项目「只报告真实发生的事」）：
// 签到成功但刷新失败时，必须如实说"签到成功了，只是刷新失败"，
// 绝不能整体报失败 —— 那会让用户以为没签到、再点一次。
func TestPanelBulkFailureReportedSeparately(t *testing.T) {
	js := fetchPanelJS(t)

	// 刷新失败时必须明确"签到是成功的"这一前提（文案里保留 msg.text）
	if !strings.Contains(js, "但额度刷新失败") {
		t.Error("缺少「签到成功但刷新失败」的独立提示 —— " +
			"部分失败不能被整体报成失败")
	}

	// 批量结果必须给用户可见提示，不能只 console.error
	// （原实现就是只 console.error，用户看到的是"点了没反应"）
	if !strings.Contains(js, "showAcctNotice") {
		t.Error("批量操作结果没有可见提示（只有 console.error）")
	}
	if !strings.Contains(js, "'acct-notice'") {
		t.Error("缺少承载批量结果的 DOM 节点 acct-notice")
	}
}

// TestPanelBulkNoticeNodeExists 守：index.html 里确实有承载提示的节点。
//
// 为什么要单独测：showAcctNotice 找不到元素时是**静默 return**，
// 若 HTML 里漏了这个 id，JS 一切正常但用户什么都看不到 —— 静默失效。
func TestPanelBulkNoticeNodeExists(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `id="acct-notice"`) {
		t.Error("index.html 缺少 id=acct-notice 的提示节点 —— " +
			"showAcctNotice 会静默失效，用户看不到任何结果")
	}
}

// fetchPanelJS 取回面板下发的 app.js 内容。
func fetchPanelJS(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(panelServerURL(t) + "/panel/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// panelServerURL 起一个最小面板服务并返回其 URL。
//
// 复用 panel_test.go 的 newPanelServer（它已装配 newMux）；用量存储传 nil
// 即可 —— 本组测试只读面板静态资源，不碰统计接口。
func panelServerURL(t *testing.T) string {
	t.Helper()
	return newPanelServer(t, nil).URL
}
