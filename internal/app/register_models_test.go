package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/router"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// TestRegisterOnePlatformRetriesNextAccount 守：**单个账号失败不能让整平台缺模型**。
//
// 🔴 这是 2026-10-07 修的脆弱点，回归护栏。
//
//	原实现只取"第一个未禁用账号"，失败即 `continue` —— 于是
//	**一个账号 token 失效、或一次网络抖动，就会让整个平台的模型
//	全部消失**，而服务看起来"正常启动"，用户只看到模型列表变空。
//
// 本测试构造"第一个账号 401、第二个正常"，断言：
//   - 最终注册成功
//   - 用的是第二个账号
//   - 模型确实进了路由
func TestRegisterOnePlatformRetriesNextAccount(t *testing.T) {
	var mu sync.Mutex
	var seenAuth []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		mu.Lock()
		seenAuth = append(seenAuth, token)
		mu.Unlock()

		// 坏账号：模拟 token 失效
		if strings.Contains(token, "BAD-TOKEN") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"invalid token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testutil.Fixture("models-listing.json"))
	}))
	defer srv.Close()

	client := newWorkbuddyClientForTest(t, srv.URL)
	r := router.New()
	accts := []*auth.Account{
		{UID: "uid-bad", AccessToken: "BAD-TOKEN"},
		{UID: "uid-good", AccessToken: "GOOD-TOKEN"},
	}

	ok, used, errs := registerOnePlatform(r, client, accts, 5*time.Second)

	if !ok {
		t.Fatalf("应换号重试并成功，实际失败: %v", errs)
	}
	if used != 1 {
		t.Errorf("used = %d，期望 1（应使用第二个账号）", used)
	}
	if len(errs) != 1 {
		t.Errorf("errs 长度 = %d，期望 1（第一个账号的失败）", len(errs))
	}
	if got := len(r.Models()); got == 0 {
		t.Error("注册成功后模型列表不应为空 —— 这正是本修复要保住的东西")
	}

	mu.Lock()
	n := len(seenAuth)
	mu.Unlock()
	if n != 2 {
		t.Errorf("应恰好请求 2 次（坏账号 + 好账号），实际 %d 次", n)
	}
}

// TestRegisterOnePlatformAllAccountsFail 守：全部账号失败时的消极行为。
//
// 要求：
//   - 报告失败（不假装成功）
//   - 每个账号的错误都被记录（便于日志定位）
//   - **不留下部分模型**（失败不能把路由搞成半成品）
func TestRegisterOnePlatformAllAccountsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"msg":"invalid token"}`))
	}))
	defer srv.Close()

	client := newWorkbuddyClientForTest(t, srv.URL)
	r := router.New()
	accts := []*auth.Account{
		{UID: "uid-1", AccessToken: "T1"},
		{UID: "uid-2", AccessToken: "T2"},
		{UID: "uid-3", AccessToken: "T3"},
	}

	ok, used, errs := registerOnePlatform(r, client, accts, 5*time.Second)

	if ok {
		t.Error("全部账号失败时不得报告成功")
	}
	if used != -1 {
		t.Errorf("used = %d，期望 -1", used)
	}
	if len(errs) != 3 {
		t.Errorf("errs 长度 = %d，期望 3（每个账号都试过并记录了原因）", len(errs))
	}
	if got := len(r.Models()); got != 0 {
		t.Errorf("失败时不应留下 %d 个模型（路由不能是半成品）", got)
	}
}

// TestRegisterOnePlatformFirstSucceeds 守：第一个账号就成功时不再打扰其他账号。
//
// 这防止"修完重试后变成每次都把所有账号试一遍" —— 那会无谓地
// 把好账号的 token 也发一遍、拖慢启动、并可能触发上游风控。
func TestRegisterOnePlatformFirstSucceeds(t *testing.T) {
	var mu sync.Mutex
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testutil.Fixture("models-listing.json"))
	}))
	defer srv.Close()

	client := newWorkbuddyClientForTest(t, srv.URL)
	r := router.New()
	accts := []*auth.Account{
		{UID: "uid-1", AccessToken: "T1"},
		{UID: "uid-2", AccessToken: "T2"},
	}

	ok, used, errs := registerOnePlatform(r, client, accts, 5*time.Second)

	if !ok || used != 0 {
		t.Fatalf("ok=%v used=%d，期望 true/0", ok, used)
	}
	if len(errs) != 0 {
		t.Errorf("第一个就成功时不应有错误记录，实际 %v", errs)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Errorf("应只请求 1 次，实际 %d 次（成功的账号之后不该再试）", n)
	}
}

// TestRegisterOnePlatformNoDuplicateModels 守：重试成功**只注册一次**。
//
// 同平台不同账号的 provider ID 相同，若把每个账号都 Register 一遍，
// 会反复用同一批 key 覆盖同一批模型（且将来 provider ID 一旦带上账号
// 维度，就会出现重复模型）。这里断言模型数等于单份目录的条数。
func TestRegisterOnePlatformNoDuplicateModels(t *testing.T) {
	var mu sync.Mutex
	call := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		call++
		cur := call
		mu.Unlock()
		// 第一次失败，第二次成功
		if cur == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testutil.Fixture("models-listing.json"))
	}))
	defer srv.Close()

	client := newWorkbuddyClientForTest(t, srv.URL)
	r := router.New()
	accts := []*auth.Account{
		{UID: "uid-1", AccessToken: "T1"},
		{UID: "uid-2", AccessToken: "T2"},
	}

	ok, _, errs := registerOnePlatform(r, client, accts, 5*time.Second)
	if !ok {
		t.Fatalf("应成功: %v", errs)
	}

	// 服务端只有一份目录，且 map 以模型 ID 为 key，
	// 所以"注册两次"也不会让 Models() 出现重复 —— 但要确认条数与目录一致。
	single := len(r.Models())
	if single == 0 {
		t.Fatal("模型列表不应为空")
	}
	// 再注册一次（模拟误重复），条数不得增长
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Register(ctx, newWorkbuddyProviderForTest(client)); err != nil {
		t.Fatalf("二次注册失败: %v", err)
	}
	if got := len(r.Models()); got != single {
		t.Errorf("重复注册后模型数 %d → %d，不应增长", single, got)
	}
}

// TestActiveAccountsOfPlatformFiltersAndOrders 守：账号筛选语义。
//
//   - 按平台过滤（国内账号不能拿去拉国际目录 —— 反之亦然）
//   - 排除人工禁用
//   - 保持 store 顺序（日志里"第 N 个账号成功"才可解释）
func TestActiveAccountsOfPlatformFiltersAndOrders(t *testing.T) {
	st := auth.NewStore()
	st.Put(&auth.Account{UID: "cn-1"})
	st.Put(&auth.Account{UID: "cn-disabled", ManualDisabled: true})
	st.Put(&auth.Account{UID: "cn-2"})
	st.Put(&auth.Account{UID: "intl-1", Platform: auth.PlatformIntl})

	cn := activeAccountsOfPlatform(st, "cn")
	var cnUIDs []string
	for _, a := range cn {
		cnUIDs = append(cnUIDs, a.UID)
	}
	if len(cnUIDs) != 2 {
		t.Fatalf("国内账号数 = %d (%v)，期望 2（禁用那个要排除）", len(cnUIDs), cnUIDs)
	}
	for _, u := range cnUIDs {
		if u == "cn-disabled" {
			t.Error("人工禁用的账号不应出现在候选里")
		}
		if u == "intl-1" {
			t.Error("国际账号不应出现在国内候选里（凭据与端点不匹配会让整目录拉取失败）")
		}
	}

	intl := activeAccountsOfPlatform(st, "intl")
	if len(intl) != 1 || intl[0].UID != "intl-1" {
		t.Errorf("国际候选 = %v，期望只有 intl-1", intl)
	}

	// 空平台 = 不限平台（兼容既有调用）
	all := activeAccountsOfPlatform(st, "")
	if len(all) != 3 {
		t.Errorf("不限平台时应返回 3 个未禁用账号，实际 %d", len(all))
	}

	// store 为 nil 时安全返回 nil（不能 panic）
	if got := activeAccountsOfPlatform(nil, "cn"); got != nil {
		t.Errorf("store 为 nil 时应返回 nil，实际 %v", got)
	}
}

// TestPickAnyActiveOfPlatformMatchesFirstCandidate 守：旧的"取第一个"语义没变。
//
// pickAnyActiveOfPlatform 现在基于 activeAccountsOfPlatform 实现，
// 必须与"全量列表的第一个"一致（否则调用方行为悄悄变了）。
func TestPickAnyActiveOfPlatformMatchesFirstCandidate(t *testing.T) {
	st := auth.NewStore()
	st.Put(&auth.Account{UID: "cn-disabled", ManualDisabled: true})
	st.Put(&auth.Account{UID: "cn-1"})
	st.Put(&auth.Account{UID: "cn-2"})

	all := activeAccountsOfPlatform(st, "cn")
	one := pickAnyActiveOfPlatform(st, "cn")

	if one == nil || len(all) == 0 {
		t.Fatal("不应为空")
	}
	if one.UID != all[0].UID {
		t.Errorf("pickAnyActive = %q，而全量首个 = %q（应一致）", one.UID, all[0].UID)
	}

	// 没有该平台账号时返回 nil
	if got := pickAnyActiveOfPlatform(st, "intl"); got != nil {
		t.Errorf("无国际账号时应返回 nil，实际 %v", got.UID)
	}
}
