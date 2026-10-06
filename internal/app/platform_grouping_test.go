package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
)

// ─────────────────────────────────────────────────────────────
// 平台分组 + 账号用量昵称的护栏测试（2026-10-06）
//
// 委托方两条反馈：
//  1.「接口列表我说过要有不同平台的分组……做两个这样的分组可以选，
//      将模型数也显示在这里面，选中哪个分组下方的模型列表就只显示对应平台的」
//  2.「账号用量为什么不用我账号管理里面的4个账号名字，
//      不然我都分不清到底是哪个账号的用量」
// ─────────────────────────────────────────────────────────────

// TestModelsResponseHasPlatformGroups 守：/api/models 返回平台分组。
//
// 🔴 为什么分组由**后端**给而不是前端按 ID 前缀切：
//
//	前端切前缀等于把"前缀规则"复制一份到前端，
//	规则一变两处就会不一致（本项目的模型前缀已经改过一次：
//	`workbuddy/` → 追加 `workbuddyai/`）。
func TestModelsResponseHasPlatformGroups(t *testing.T) {
	srv, _ := newAliasEnv(t)

	body := fetchBody(t, srv.URL+"/api/models", "")
	var got struct {
		Groups []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Prefix string `json:"prefix"`
			Count  int    `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, body)
	}

	if len(got.Groups) == 0 {
		t.Fatal("缺少 groups 字段 —— 面板无法做平台分组")
	}
	// 该测试环境只注册了 workbuddy（国内版），所以只应有一个分组 ——
	// 空分组标签会让人以为"另一个平台坏了"。
	if len(got.Groups) != 1 {
		t.Fatalf("只注册一个渠道时应只有 1 个分组，实际 %d", len(got.Groups))
	}
	g := got.Groups[0]
	if g.ID != auth.PlatformCN {
		t.Errorf("分组 ID = %q，期望 %q（要与账号的 platform 同源）",
			g.ID, auth.PlatformCN)
	}
	if g.Label != "国内版" {
		t.Errorf("分组名 = %q，期望 国内版", g.Label)
	}
	if g.Prefix != "workbuddy/" {
		t.Errorf("分组前缀 = %q，期望 workbuddy/", g.Prefix)
	}
	if g.Count == 0 {
		t.Error("分组 Count 为 0 —— 委托方要求把模型数显示在分组上")
	}
}

// TestModelGroupCountsMatchPrefixes 守：分组里的 Count 与实际模型数一致。
//
// 数错会直接误导（标签写"国内版 16"，列表里却只有 10 个）。
func TestModelGroupCountsMatchPrefixes(t *testing.T) {
	srv, _ := newAliasEnv(t)

	body := fetchBody(t, srv.URL+"/api/models", "")
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Groups []struct {
			Prefix string `json:"prefix"`
			Count  int    `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}

	for _, g := range got.Groups {
		actual := 0
		for _, m := range got.Data {
			if len(m.ID) >= len(g.Prefix) && m.ID[:len(g.Prefix)] == g.Prefix {
				actual++
			}
		}
		if actual != g.Count {
			t.Errorf("分组 %s 声称 %d 个，实际前缀匹配 %d 个",
				g.Prefix, g.Count, actual)
		}
	}
}

// TestNoAllPlatformTab 守：平台分组里**没有**「全部」标签。
//
// 委托方 2026-10-06：「模型列表里不需要全部这个分组」。
//
// 🔴 为什么这个删除是合理的（不是随手删）：
//
//	平台之间没有"混合视图"的实际需求；而"全部"会让**前缀提示**
//	无法给出一个确定值（得并列两个前缀），反而更容易让人搞错。
func TestNoAllPlatformTab(t *testing.T) {
	js := fetchPanelAppJS(t)

	body := stripComments(extractFunc(t, js, "function renderPlatformTabs(groups)"))
	if strings.Contains(body, "'全部'") || strings.Contains(body, `"全部"`) {
		t.Error("renderPlatformTabs 仍在渲染「全部」标签 —— 委托方已要求去掉")
	}
}

// TestNoModelCountUnderApiBase 守：「OpenAI 兼容接口」下方不再显示模型数。
//
// 委托方 2026-10-06：「OpenAI 兼容接口下方的模型数不需要显示了」。
//
// ⚠️ 模型数**没有丢失** —— 它现在只在平台分组标签上
//
//	（那个位置更有用：能看出各平台各有多少）。
func TestNoModelCountUnderApiBase(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := stripHTMLComments(string(raw))

	if strings.Contains(html, `id="api-model-count"`) {
		t.Error("「OpenAI 兼容接口」下方仍有模型数 —— 委托方已要求移除")
	}

	js := fetchPanelAppJS(t)
	if strings.Contains(js, "'api-model-count'") {
		t.Error("app.js 仍在写 api-model-count（该元素已删除）")
	}
	// 分组标签上的数量必须还在（那是保留的位置）
	if !strings.Contains(js, "plat-count") {
		t.Error("平台分组标签上的模型数不见了 —— 委托方只要求删「接口下方」那个")
	}
}

// TestStatsAccountsIncludeNickname 守：账号用量带昵称（不是只有脱敏 UID）。
//
// 🔴 委托方原话：
//
//	「为什么不用我账号管理里面的4个账号名字，
//	  不然我都分不清到底是哪个账号的用量」
//
// 脱敏 UID（`eeeeeeee…`）对人类不可读。
func TestStatsAccountsIncludeNickname(t *testing.T) {
	st := auth.NewStore()
	st.Put(&auth.Account{UID: "11111111-2222-3333-4444-555555555555",
		Nickname: "13800000001"})
	st.Put(&auth.Account{UID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Nickname: "intl@example.com", Platform: auth.PlatformIntl})

	// 直接验证反查逻辑（无需求真跑统计管线）
	got, ok := lookupAccountByMasked(Deps{Accounts: st}, auth.MaskUID("11111111-2222-3333-4444-555555555555"))
	if !ok {
		t.Fatal("反查失败 —— 用量页只会显示脱敏 UID，用户分不清是哪个账号")
	}
	if got.Nickname != "13800000001" {
		t.Errorf("昵称 = %q，期望 13800000001", got.Nickname)
	}
	if got.PlatformOf() != auth.PlatformCN {
		t.Errorf("平台 = %q，期望 %q", got.PlatformOf(), auth.PlatformCN)
	}

	// 国际账号
	got2, ok := lookupAccountByMasked(Deps{Accounts: st}, auth.MaskUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"))
	if !ok {
		t.Fatal("国际账号反查失败")
	}
	if got2.PlatformOf() != auth.PlatformIntl {
		t.Errorf("平台 = %q，期望 %q", got2.PlatformOf(), auth.PlatformIntl)
	}
}

// TestLookupAccountByMaskedMissing 守：账号已删除时**如实返回 false**。
//
// 🔴 不能用"猜"给一个已删账号补名字 ——
//
//	那会让统计页显示一个已经不存在的账号的昵称，
//	用户会以为它还在。
func TestLookupAccountByMaskedMissing(t *testing.T) {
	st := auth.NewStore()
	if _, ok := lookupAccountByMasked(Deps{Accounts: st}, "deadbeef…"); ok {
		t.Error("账号不存在时应返回 false（调用方退回显示脱敏 UID）")
	}
	// Accounts 为 nil 不该 panic
	if _, ok := lookupAccountByMasked(Deps{}, "x"); ok {
		t.Error("Accounts 为 nil 时应返回 false")
	}
	// 空串不该匹配到任何东西
	st.Put(&auth.Account{UID: "11111111-2222-3333-4444-555555555555", Nickname: "n"})
	if _, ok := lookupAccountByMasked(Deps{Accounts: st}, ""); ok {
		t.Error("空串不该匹配到账号")
	}
}

// TestPlatformTabsRendered 守：面板有分组标签容器与切换逻辑。
func TestPlatformTabsRendered(t *testing.T) {
	resp, err := http.Get(panelServerURL(t) + "/panel/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := stripHTMLComments(string(raw))

	if !strings.Contains(html, `id="api-plat-tabs"`) {
		t.Error("缺少平台分组容器 api-plat-tabs —— 委托方要求顶部可切换平台")
	}

	js := fetchPanelAppJS(t)
	for _, fn := range []string{
		"function renderPlatformTabs(",
		"function selectPlatform(",
		"function filterByPlatform(",
	} {
		if !strings.Contains(js, fn) {
			t.Errorf("缺少 %s", fn)
		}
	}
	// 分组必须取自后端（不自己猜前缀）
	body := stripComments(extractFunc(t, js, "function filterByPlatform(list, platID)"))
	if !strings.Contains(body, "apiGroups") {
		t.Error("filterByPlatform 没使用后端给的分组 —— " +
			"前端自己猜前缀会在前缀规则变化时静默失配")
	}
}
