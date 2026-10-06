package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 面板导入：形状兼容性护栏
//
// 背景（委托方 2026-10-05 的真实痛点）：
//
//	他说"不知道凭据从哪来"，面板只有一个空粘贴框。
//	补上引导后，最省事且最不易错的做法是**整份凭据文件直接粘贴**。
//	而官方客户端存的凭据是**嵌套**的（形如 {account:{…}, auth:{…}}），
//	我们的接口要的是**扁平**的 —— 前端 flattenCredential 负责转换。
//
// 这些测试守住"扁平化后的结果能被后端接受"，以及"必需字段不能松"。
// ═══════════════════════════════════════════════════════════════════

// importShapeEnv 起一个可导入账号的测试服务。
//
// 复用既有的 newServeTestEnv（与其它 import 测试同一套装配，避免出现
// 第二套测试基建 —— 那样两个 helper 会各自漂移）。
func importShapeEnv(t *testing.T) *httptest.Server {
	t.Helper()
	_, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")
	return srv
}

// TestImportAPIAcceptsFlattenedCredentialShape 守：**扁平化后**的嵌套凭据能被接受。
//
// 说明：扁平化在前端做（app.js 的 flattenCredential），后端只认扁平形状。
// 本测试验证"扁平化的结果确实可用"—— 这是前端那步转换的落地断言。
//
// ⚠️ 前端 flattenCredential 本身是纯 JS，Go 测不到它的行为；
//
//	这里测的是**它产出什么形状后端才认**（即两端契约）。
//	更强的验证在真实浏览器里（面板导入一次）。
func TestImportAPIAcceptsFlattenedCredentialShape(t *testing.T) {
	srv := importShapeEnv(t)

	// 这是 flattenCredential({account:{…}, auth:{…}}) 处理**之后**应得到的形状
	flat := `{"accounts":[{"uid":"nested-uid-1","nickname":"嵌套号",` +
		`"accessToken":"tok-nested","refreshToken":"rt-nested",` +
		`"enterpriseId":"ent-1","domain":"www.codebuddy.cn"}]}`

	code, body := postJSON(t, srv.URL+"/api/accounts/import", flat)
	if code != http.StatusOK {
		t.Fatalf("扁平化后的凭据应被接受，状态码 = %d；body=%s", code, body)
	}
	if !strings.Contains(body, `"ok":1`) {
		t.Errorf("应成功导入 1 个账号；body=%s", body)
	}
	// 红线：绝不回显 token
	if strings.Contains(body, "tok-nested") || strings.Contains(body, "rt-nested") {
		t.Error("🔴 导入响应回显了 token")
	}
}

// TestImportAPIRejectsMissingRequiredFields 守：缺 uid/accessToken 必须计为失败。
//
// 与"接受嵌套"配对：**形状可以宽松，必需字段不能松**。
// 引导里让用户"复制 accessToken"，若他只复制了 nickname，
// 必须得到明确失败，而不是静默入库一个永远不可用的账号。
func TestImportAPIRejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缺 accessToken", `{"accounts":[{"uid":"only-uid"}]}`},
		{"缺 uid", `{"accounts":[{"accessToken":"only-token"}]}`},
		{"两者都缺", `{"accounts":[{"nickname":"n"}]}`},
	}
	for _, c := range cases {
		srv := importShapeEnv(t)
		code, body := postJSON(t, srv.URL+"/api/accounts/import", c.body)
		if code != http.StatusOK {
			t.Errorf("%s：期望 200（逐项计失败），实际 %d", c.name, code)
			continue
		}
		if !strings.Contains(body, `"failed":1`) {
			t.Errorf("%s：应计 1 个失败，实际 body=%s", c.name, body)
		}
		if !strings.Contains(body, `"ok":0`) {
			t.Errorf("%s：不应有成功项，实际 body=%s", c.name, body)
		}
	}
}

// TestImportAPIRequiresAccountsWrapper 守：后端只接受 {accounts:[…]}，**不接受裸数组**。
//
// ⚠️ 这条是我写测试时**实测发现**的契约细节（原本以为后端两种都收）：
//
//	裸数组会让 json.Unmarshal 报
//	  "cannot unmarshal array into Go value of type app.importBody"
//	→ 400 invalid_json。
//
// **这不是缺陷**：面板前端（app.js 的 doImport）负责把「数组」「单对象」
// 两种用户输入都**包成 {accounts:[…]}** 再发。后端维持单一形状更简单，
// 也让"用户输入形状的兼容"集中在一处（前端），不会两边各做一半。
//
// 本测试把这个分工钉住：
//   - 后端契约 = 只认 {accounts:[…]}
//   - 前端职责 = 把各种用户粘贴形状归一成它
//
// 若哪天要让后端也收裸数组，这里会变红提醒同步改前端与文档。
func TestImportAPIRequiresAccountsWrapper(t *testing.T) {
	srv := importShapeEnv(t)
	code, body := postJSON(t, srv.URL+"/api/accounts/import",
		`[{"uid":"shape-a","accessToken":"t-a"}]`)
	if code != http.StatusBadRequest {
		t.Errorf("裸数组应被拒绝（前端负责包成 accounts 对象），实际状态码 %d；body=%s", code, body)
	}
	if !strings.Contains(body, "invalid_json") {
		t.Errorf("应返回 invalid_json 说明形状不对；body=%s", body)
	}

	// 同一份数据包成正确形状后必须成功 —— 证明失败原因是**形状**不是数据
	code2, body2 := postJSON(t, srv.URL+"/api/accounts/import",
		`{"accounts":[{"uid":"shape-a","accessToken":"t-a"}]}`)
	if code2 != http.StatusOK || !strings.Contains(body2, `"ok":1`) {
		t.Errorf("包成 accounts 后应成功（证明上面失败只因形状）；code=%d body=%s", code2, body2)
	}
}
