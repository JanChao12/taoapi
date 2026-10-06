// api_accounts_test.go：账号管理 API 的验收测试。
//
// 覆盖：
//   - GET /api/accounts 返回视图且【绝不】泄露 token（结构性保证的回归网）
//   - POST action=disable/enable 真实修改 Store 并持久化生效
//   - 未知 uid → 404；未知 action → 400；缺 uid → 400
package app

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
)

// newAccountsTestServer 构造带两个账号（normal + disabled）的测试服务。
// 返回 store 供断言"操作是否真实写入 Store"。
func newAccountsTestServer(t *testing.T) (*httptest.Server, *auth.Store) {
	t.Helper()

	store := auth.NewStore()
	store.Put(&auth.Account{
		UID:          "acct-normal-0001",
		Nickname:     "13800000001",
		AccessToken:  "SECRET-ACCESS-TOKEN-1", // 必须不出现在任何响应里
		RefreshToken: "SECRET-REFRESH-TOKEN-1",
		Status:       pool.StatusNormal,
		EnterpriseID: "ent-1",
		Domain:       "d1",
		Credit: auth.CreditSnapshot{
			Known:     true,
			Remaining: 500,
			Packages: []auth.PackageSnapshot{
				{Name: "基础包", Remain: 300, ExpireAt: "2030-01-01"},
				{Name: "赠送包", Remain: 200, ExpireAt: "2020-01-01"}, // 已过期
			},
		},
	})
	store.Put(&auth.Account{
		UID:            "acct-disabled-01",
		Nickname:       "13800000002",
		AccessToken:    "SECRET-ACCESS-TOKEN-2",
		RefreshToken:   "SECRET-REFRESH-TOKEN-2",
		ManualDisabled: true,
	})

	srv := httptest.NewServer(newMux(Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: store,
	}))
	t.Cleanup(srv.Close)
	return srv, store
}

// getJSON GET 并解码 JSON 响应。
func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\nbody: %s", err, raw)
	}
	return resp.StatusCode, m
}

// postAction POST /api/accounts/action。
func postAction(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

// accountsOf 从响应里取 accounts 数组。
func accountsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	list, ok := m["accounts"].([]any)
	if !ok {
		t.Fatalf("响应缺少 accounts 数组: %v", m)
	}
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		out = append(out, v.(map[string]any))
	}
	return out
}

// TestAccountsListNoTokens 核心红线：响应里绝不能出现 token。
func TestAccountsListNoTokens(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	code, m := getJSON(t, srv.URL+"/api/accounts")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", code)
	}

	// 双重断言：原始字节里也不能出现（防字段名换写法绕过）
	resp, err := http.Get(srv.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(rawBody)
	for _, banned := range []string{
		"SECRET-ACCESS-TOKEN-1", "SECRET-REFRESH-TOKEN-1",
		"SECRET-ACCESS-TOKEN-2", "SECRET-REFRESH-TOKEN-2",
		"access_token", "refresh_token", "accessToken", "refreshToken",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("原始响应泄露了 %q（红线！）", banned)
		}
	}

	// 解码后的 JSON 同样断言（覆盖 marshal 路径）
	raw, _ := json.Marshal(m)
	for _, banned := range []string{"access_token", "refresh_token", "accessToken", "refreshToken"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("解码后 JSON 泄露了 %q（红线！）", banned)
		}
	}
}

// TestAccountsListShape 验证字段值：状态、禁用标志、额度、过期标记。
func TestAccountsListShape(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	_, m := getJSON(t, srv.URL+"/api/accounts")
	accts := accountsOf(t, m)
	if len(accts) != 2 {
		t.Fatalf("accounts 数 = %d，期望 2", len(accts))
	}

	var normal, disabled map[string]any
	for _, a := range accts {
		switch a["uid"] {
		case "acct-normal-0001":
			normal = a
		case "acct-disabled-01":
			disabled = a
		}
	}
	if normal == nil || disabled == nil {
		t.Fatalf("缺少期望的账号: %v", accts)
	}

	if disabled["manual_disabled"] != true {
		t.Errorf("disabled 账号 manual_disabled = %v，期望 true", disabled["manual_disabled"])
	}
	if disabled["status"] != "disabled" {
		t.Errorf("disabled 账号 status = %v，期望 disabled", disabled["status"])
	}
	if disabled["status_label"] != "已禁用" {
		t.Errorf("disabled 账号 status_label = %v，期望 已禁用", disabled["status_label"])
	}
	if normal["manual_disabled"] != false {
		t.Errorf("normal 账号 manual_disabled = %v，期望 false", normal["manual_disabled"])
	}
	if normal["status_label"] != "正常" {
		t.Errorf("normal 账号 status_label = %v，期望 正常", normal["status_label"])
	}

	// 额度：Known=true 时 credits 应为数值 500
	if v, ok := normal["credits"].(float64); !ok || v != 500 {
		t.Errorf("normal credits = %v，期望 500", normal["credits"])
	}
	// 从未查过额度的账号 credits 必须是 null（disabled 号没给快照）
	if disabled["credits"] != nil {
		t.Errorf("disabled credits = %v，期望 null", disabled["credits"])
	}

	// 包明细与过期标记
	pkgs, ok := normal["packages"].([]any)
	if !ok || len(pkgs) != 2 {
		t.Fatalf("normal packages = %v，期望 2 个", normal["packages"])
	}
	p1 := pkgs[0].(map[string]any)
	p2 := pkgs[1].(map[string]any)
	if p1["expired"] != false {
		t.Errorf("2030-01-01 的包 expired = %v，期望 false", p1["expired"])
	}
	if p2["expired"] != true {
		t.Errorf("2020-01-01 且剩余>0 的包 expired = %v，期望 true", p2["expired"])
	}

	// 最早到期日：两个包额度都为正，取更早的 2020-01-01
	if v, _ := normal["earliest_expiry"].(string); v != "2020-01-01" {
		t.Errorf("earliest_expiry = %v，期望 2020-01-01", normal["earliest_expiry"])
	}
}

// TestAccountsActionDisablePersistsToStore disable 必须真实改 Store，而不是只改响应。
func TestAccountsActionDisablePersistsToStore(t *testing.T) {
	srv, store := newAccountsTestServer(t)

	code, m := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-normal-0001","action":"disable"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，body = %v", code, m)
	}

	// Store 里的对象必须真的被改（操作直接改指针，无须重新 Put）
	if a, _ := store.Get("acct-normal-0001"); !a.ManualDisabled {
		t.Error("Store 中 normal 账号的 ManualDisabled 仍为 false，disable 未生效")
	}

	// 通过列表接口确认生效（同一 store 实例）
	_, list := getJSON(t, srv.URL+"/api/accounts")
	for _, a := range accountsOf(t, list) {
		if a["uid"] == "acct-normal-0001" && a["manual_disabled"] != true {
			t.Errorf("disable 后 manual_disabled = %v，期望 true", a["manual_disabled"])
		}
	}

	// 再 enable 回来
	code, _ = postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-normal-0001","action":"enable"}`)
	if code != http.StatusOK {
		t.Fatalf("enable 状态码 = %d", code)
	}
	if a, _ := store.Get("acct-normal-0001"); a.ManualDisabled {
		t.Error("Store 中 normal 账号的 ManualDisabled 仍为 true，enable 未生效")
	}
	_, list = getJSON(t, srv.URL+"/api/accounts")
	for _, a := range accountsOf(t, list) {
		if a["uid"] == "acct-normal-0001" && a["manual_disabled"] != false {
			t.Errorf("enable 后 manual_disabled = %v，期望 false", a["manual_disabled"])
		}
	}
}

// TestAccountsActionUnknownUID404 未知账号必须 404。
func TestAccountsActionUnknownUID404(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	code, _ := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"no-such-uid","action":"disable"}`)
	if code != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404", code)
	}
}

// TestAccountsActionUnknownAction400 未知 action 必须 400。
func TestAccountsActionUnknownAction400(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	code, _ := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-normal-0001","action":"nuke"}`)
	if code != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", code)
	}
}

// TestAccountsActionMissingUID400 缺 uid 必须 400。
func TestAccountsActionMissingUID400(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	code, _ := postAction(t, srv.URL+"/api/accounts/action", `{"action":"disable"}`)
	if code != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", code)
	}
}

// TestAccountsActionBodyTooLarge413 超大请求体必须 413。
func TestAccountsActionBodyTooLarge413(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	big := `{"uid":"acct-normal-0001","action":"disable","pad":"` + strings.Repeat("x", 8192) + `"}`
	code, _ := postAction(t, srv.URL+"/api/accounts/action", big)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("状态码 = %d，期望 413", code)
	}
}

// TestAccountsNoStoreNilSafe 未配置账号仓库时不崩溃，返回空列表。
func TestAccountsNoStoreNilSafe(t *testing.T) {
	srv := httptest.NewServer(newMux(Deps{Logger: log.New(io.Discard, "", 0)}))
	defer srv.Close()

	code, m := getJSON(t, srv.URL+"/api/accounts")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", code)
	}
	if accts := accountsOf(t, m); len(accts) != 0 {
		t.Errorf("accounts 数 = %d，期望 0", len(accts))
	}
}

// ─────────────────────────────────────────────────────────────
// refresh / checkin 端到端（假上游）
// ─────────────────────────────────────────────────────────────

// fakeBillingUpstream 假的 codebuddy 计费上游：可编程的额度/签到响应。
type fakeBillingUpstream struct {
	mu         sync.Mutex
	creditBody []byte
	checkinSeq []fakeCheckinResp // 依次返回；耗尽后重复最后一个
	checkinN   int
}

type fakeCheckinResp struct {
	code int
	body string
}

func (f *fakeBillingUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "get-user-resource"):
			f.mu.Lock()
			body := f.creditBody
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case strings.HasSuffix(r.URL.Path, "daily-checkin"):
			f.mu.Lock()
			i := f.checkinN
			if i >= len(f.checkinSeq) {
				i = len(f.checkinSeq) - 1
			}
			f.checkinN++
			resp := f.checkinSeq[i]
			f.mu.Unlock()
			w.WriteHeader(resp.code)
			_, _ = w.Write([]byte(resp.body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestAccountsActionRefresh 端到端：refresh 走真实 provider → 假上游，
// 快照写进 Store 并在响应里返回视图（依旧无 token）。
func TestAccountsActionRefresh(t *testing.T) {
	fake := &fakeBillingUpstream{
		creditBody: []byte(`{"code":0,"data":{"response":{"data":{"TotalDosage":1234,"Accounts":[
			{"PackageName":"月度包","CycleCapacitySize":1000,"CycleCapacityUsed":400,"CycleCapacityRemain":600,"CycleEndTime":"2030-06-01 23:59:59"},
			{"PackageName":"赠送包","CycleCapacitySize":700,"CycleCapacityUsed":100,"CycleCapacityRemain":600,"CycleEndTime":"2020-01-01 00:00:00"}
		]}}}}`),
	}
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)

	store := auth.NewStore()
	store.Put(&auth.Account{
		UID:         "acct-refresh-001",
		Nickname:    "13900000001",
		AccessToken: "SECRET-ACCESS-TOKEN-R1",
	})
	client := newWorkbuddyClientForTest(t, up.URL)

	srv := httptest.NewServer(newMux(Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: store,
		WBClient: client,
	}))
	t.Cleanup(srv.Close)

	code, m := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-refresh-001","action":"refresh"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，body = %v", code, m)
	}

	// Store 里快照生效
	a, _ := store.Get("acct-refresh-001")
	if !a.Credit.Known {
		t.Fatal("refresh 后 Credit.Known = false")
	}
	if a.Credit.Remaining != 1200 {
		t.Errorf("Remaining = %d，期望 1200（600+600）", a.Credit.Remaining)
	}
	if len(a.Credit.Packages) != 2 {
		t.Fatalf("Packages 数 = %d，期望 2", len(a.Credit.Packages))
	}
	// provider.Credit 已按到期日升序排序（2020 在前）
	if a.Credit.Packages[0].ExpireAt != "2020-01-01" || a.Credit.Packages[0].Remain != 600 {
		t.Errorf("Package[0] = %+v，期望 2020-01-01/600", a.Credit.Packages[0])
	}
	if a.Credit.Packages[1].ExpireAt != "2030-06-01" || a.Credit.Packages[1].Remain != 600 {
		t.Errorf("Package[1] = %+v，期望 2030-06-01/600", a.Credit.Packages[1])
	}

	// 响应视图无 token
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "SECRET-ACCESS-TOKEN-R1") {
		t.Error("refresh 响应泄露了 token（红线！）")
	}
	acct, _ := m["account"].(map[string]any)
	if acct == nil {
		t.Fatalf("响应缺少 account 视图: %v", m)
	}
	if v, ok := acct["credits"].(float64); !ok || v != 1200 {
		t.Errorf("响应 credits = %v，期望 1200", acct["credits"])
	}
}

// TestAccountsActionCheckin 端到端：签到成功与"已签到"两条路。
func TestAccountsActionCheckin(t *testing.T) {
	fake := &fakeBillingUpstream{
		checkinSeq: []fakeCheckinResp{
			{code: 200, body: `{"code":0,"msg":"ok"}`}, // 第一次：成功
			{code: 400, body: ""},                      // 第二次：HTTP 400 + 空 body = 已签到
		},
	}
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)

	store := auth.NewStore()
	store.Put(&auth.Account{
		UID:         "acct-checkin-001",
		Nickname:    "13900000002",
		AccessToken: "SECRET-ACCESS-TOKEN-C1",
	})
	client := newWorkbuddyClientForTest(t, up.URL)

	srv := httptest.NewServer(newMux(Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: store,
		WBClient: client,
	}))
	t.Cleanup(srv.Close)

	// 第一次：签到成功
	code, m := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-checkin-001","action":"checkin"}`)
	if code != http.StatusOK {
		t.Fatalf("checkin 状态码 = %d，body = %v", code, m)
	}
	if m["ok"] != true {
		t.Errorf("ok = %v，期望 true", m["ok"])
	}
	if m["already"] == true {
		t.Errorf("already = %v，期望 false（第一次签到）", m["already"])
	}
	a, _ := store.Get("acct-checkin-001")
	if a.CheckinDay == "" {
		t.Error("签到成功后 CheckinDay 为空")
	}
	if a.CheckinAt.IsZero() {
		t.Error("签到成功后 CheckinAt 为零值")
	}

	// 第二次：上游返回"已签到"（HTTP 400 + 空 body）
	code, m = postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-checkin-001","action":"checkin"}`)
	if code != http.StatusOK {
		t.Fatalf("重复 checkin 状态码 = %d，body = %v", code, m)
	}
	if m["already"] != true {
		t.Errorf("already = %v，期望 true", m["already"])
	}

	// checkin 响应同样绝不能含 token
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "SECRET-ACCESS-TOKEN-C1") {
		t.Error("checkin 响应泄露了 token（红线！）")
	}
}

// TestAccountsActionRefreshUpstreamError 上游额度查询失败时必须 502，且不污染快照。
func TestAccountsActionRefreshUpstreamError(t *testing.T) {
	fake := &fakeBillingUpstream{
		creditBody: []byte(`{"code":1,"msg":"boom"}`),
	}
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)

	store := auth.NewStore()
	store.Put(&auth.Account{
		UID:         "acct-refresh-err1",
		AccessToken: "SECRET-ACCESS-TOKEN-E1",
		Credit: auth.CreditSnapshot{
			Known:     true,
			Remaining: 42,
		},
	})
	client := newWorkbuddyClientForTest(t, up.URL)

	srv := httptest.NewServer(newMux(Deps{
		Logger:   log.New(io.Discard, "", 0),
		Accounts: store,
		WBClient: client,
	}))
	t.Cleanup(srv.Close)

	code, _ := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-refresh-err1","action":"refresh"}`)
	if code != http.StatusBadGateway {
		t.Errorf("状态码 = %d，期望 502", code)
	}
	// 失败时不得破坏旧快照
	a, _ := store.Get("acct-refresh-err1")
	if !a.Credit.Known || a.Credit.Remaining != 42 {
		t.Errorf("失败后快照被污染: known=%v remaining=%d", a.Credit.Known, a.Credit.Remaining)
	}
}

// TestAccountsActionRefreshNoClient 未注入 WBClient 时 refresh 必须 503 而不是 panic。
func TestAccountsActionRefreshNoClient(t *testing.T) {
	srv, _ := newAccountsTestServer(t)

	code, _ := postAction(t, srv.URL+"/api/accounts/action",
		`{"uid":"acct-normal-0001","action":"refresh"}`)
	if code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", code)
	}
}
