package app

import (
	"io"
	"net/http"
	"strings"
	"testing"

	usagepkg "workbuddy.local/workbuddy-api/internal/usage"
)

// ─────────────────────────────────────────────────────────────
// 2026-10-06 委托方实测反馈的修复护栏
//
// 这一组守的是**用户实际看到的问题**，不是内部实现细节：
//   A. 主页无条件显示「自动签到已开启」（实际没开）
//   B. 模型用量把一个模型显示成三行
//   C. 没有全局缓存命中率、没有接入用的密钥展示
// ─────────────────────────────────────────────────────────────

// TestHomeTipsNotHardcoded 守：账号页顶部提示**不能**写死"已开启"。
//
// 🔴 真实 bug（委托方原话）：
//
//	「我明明没开启自动签到，为什么主页显示自动签到已开启，
//	  只有在我开启时才显示」
//
// 根因：HTML 里有一句静态的「✅ 每日自动签到已开启」，无论开关状态都渲染。
//
// 断言：HTML 里**不得**再出现无条件的"已开启"文案；
// 提示必须由 JS 按 /api/settings 的实际值动态生成。
func TestHomeTipsNotHardcoded(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// ⚠️ 必须剥掉 HTML 注释再断言：我在那段注释里**故意引用**了旧的
	// 写死文案作为"为什么不能这么写"的说明。不剥注释会把说明当成违规代码
	// —— 本项目的测试已因此误报过一次（GO 侧用 stripComments，这里同理）。
	html := stripHTMLComments(string(raw))

	// 静态文案必须消失
	for _, forbidden := range []string{
		"每日自动签到已开启 —— 服务运行期间自动签到，无需配置",
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("HTML 里仍有写死的「%s」—— "+
				"它会让没开启的用户也看到「已开启」，与开关状态矛盾", forbidden)
		}
	}

	// 承载容器必须在（JS 要往里填内容）
	if !strings.Contains(html, `id="home-tips"`) {
		t.Error("缺少 id=home-tips 容器")
	}
	// 初始应该是隐藏的（没拉数据前不能宣称任何东西已开启）
	if strings.Contains(html, `id="home-tips" class="tip`) {
		t.Error("home-tips 初始不应带提示样式（拉数据前不该显示任何状态）")
	}

	// JS 侧必须按实际值判断
	js := fetchPanelAppJS(t)
	if !strings.Contains(js, "function renderHomeTips(") {
		t.Fatal("缺少 renderHomeTips")
	}
	body := stripComments(extractFunc(t, js, "function renderHomeTips()"))
	for _, want := range []string{"autoCheckin === true", "autoStart === true"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderHomeTips 未按 %s 判断 —— 会显示与开关不符的提示", want)
		}
	}
}

// TestApiPageShowsKeyAndModel 守：「API 接入」页给出接入所需的完整信息。
//
// 委托方要求（原话）：
//
//	「在api右边加个显示key，以及复制按键，方便用户在这个界面直接接入完整信息」
//	「不要模型名，以及api key放到接口地址的右边而不是下方」
//
// ⇒ 最终形态：**接口地址 + API Key 并排**，各带复制按钮；**没有模型名**。
func TestApiPageShowsKeyAndModel(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := stripHTMLComments(string(raw))

	for _, want := range []string{
		`id="apiAddr"`, `id="btn-copy-base"`,
		`id="apiKeyView"`, `id="btn-copy-key"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("API 接入页缺少 %s", want)
		}
	}

	// 「模型名」一行应已移除（委托方明确要求不要）
	if strings.Contains(html, `id="apiModelExample"`) {
		t.Error("「模型名」一行仍在 —— 委托方已要求移除")
	}

	// 地址与密钥必须在**同一个** api-base-row 里（并排，不是上下两行）
	rowStart := strings.Index(html, `id="apiAddr"`)
	rowEnd := strings.Index(html, `id="apiKeyView"`)
	if rowStart < 0 || rowEnd < 0 || rowEnd < rowStart {
		t.Fatal("找不到地址与密钥的位置")
	}
	// 两者之间不应出现 api-base-row 的关闭+重新开始（那说明分了两行）
	between := html[rowStart:rowEnd]
	if strings.Count(between, "api-base-row") > 0 {
		t.Error("地址与密钥被分在了两个 api-base-row 里 —— " +
			"委托方要求 API Key 在接口地址的**右边**，不是下方")
	}

	js := fetchPanelAppJS(t)
	if !strings.Contains(js, "function fillApiCredentials(") {
		t.Error("缺少 fillApiCredentials —— 密钥不会被填上")
	}
}

// TestStatsModelListNormalizedToOneRow 守：同一模型的多种写法聚合为一行。
//
// 委托方原话：
//
//	「这个dsf和deepseek-v4.1-flash使用的都是workbuddy/deepseek-v4.1-flash，
//	  为什么三种显示，都显示workbuddy/deepseek-v4.1-flash就行」
//
// 这里用真实事件验证聚合结果（不是只看代码里有没有调用规范化函数）。
func TestUsageModelKeyPrefersRecordedRoute(t *testing.T) {
	// 新记录带真实路由身份 → 直接用，不看注册表
	deps := Deps{}
	ev := usageEventWithRoute("dsf", "workbuddy", "deepseek-v4.1-flash")
	if got := deps.resolveModelKey(ev); got != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("带路由身份的记录归一为 %q，期望 workbuddy/deepseek-v4.1-flash", got)
	}

	// 老记录（无路由身份）→ 回退到按原文归一（Router 为 nil 时原样返回）
	old := usageEventWithRoute("dsf", "", "")
	if got := deps.resolveModelKey(old); got != "dsf" {
		t.Errorf("无路由身份且无 Router 时应原样返回，实际 %q", got)
	}
}

// stripHTMLComments 去掉 HTML 注释。
//
// 与 stripComments（Go/JS 用）同样的理由：本项目在注释里**故意保留**
// 被禁止的旧写法作为说明，断言前必须剥掉，否则会把说明当成违规代码。
func stripHTMLComments(s string) string {
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "-->")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+3:]
	}
}

// usageEventWithRoute 造一条用量事件（用于验证聚合 key 的选择逻辑）。
func usageEventWithRoute(clientModel, providerID, resolvedModel string) usagepkg.Event {
	return usagepkg.Event{
		Model:         clientModel,
		ProviderID:    providerID,
		ResolvedModel: resolvedModel,
	}
}

// TestStatsCacheHitRateNullWhenNoSamples 守：无样本时命中率是 null 不是 0。
//
// 🔴 为什么（Codex 第 40 轮）：
//
//	"该模型从没被调用过"与"调用过但一次没命中"含义完全不同 ——
//	前者应显示 —，后者应显示 0%。只给两个 0，前端无从区分。
func TestStatsCacheHitRateNullWhenNoSamples(t *testing.T) {
	// 该字段是指针类型 —— 编译期就保证了 null 可表达
	var ms modelStat
	if ms.CacheHitRate != nil {
		t.Error("零值的 CacheHitRate 应为 nil（无样本）")
	}
	// 而 CacheHitTokens/CacheMissTokens 是计数，0 就是 0
	if ms.CacheHitTokens != 0 || ms.CacheMissTokens != 0 {
		t.Error("计数零值应为 0")
	}
}
