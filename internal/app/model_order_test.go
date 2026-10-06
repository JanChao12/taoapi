package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/router"
	"workbuddy.local/workbuddy-api/internal/testutil"
)

// modelIDs 取当前路由里的模型 ID 列表。
func modelIDs(t *testing.T, r *router.Router) []string {
	t.Helper()
	models := r.Models()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// TestModelsOrderIsStable 验证模型列表顺序稳定且按 ID 升序。
//
// 背景（委托方实测反馈）：底层是 map，迭代顺序随机，
// 导致每次刷新模型列表顺序都在变。修法是 Router.Models() 排序。
func TestModelsOrderIsStable(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	fake.WithJSON("models-listing.json")

	r := router.New()
	if err := r.Register(context.Background(),
		newWorkbuddyProviderForTest(newWorkbuddyClientForTest(t, fake.URL))); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	first := modelIDs(t, r)
	if len(first) == 0 {
		t.Fatal("模型列表为空")
	}

	// 升序
	for i := 1; i < len(first); i++ {
		if first[i-1] > first[i] {
			t.Errorf("未按 ID 升序：%q 在 %q 之前", first[i-1], first[i])
		}
	}

	// 稳定性：再取 20 次必须完全一致
	for n := 0; n < 20; n++ {
		got := modelIDs(t, r)
		if len(got) != len(first) {
			t.Fatalf("第 %d 次长度变化：%d → %d", n, len(first), len(got))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("第 %d 次顺序变化：位置 %d 是 %q，期望 %q",
					n, i, got[i], first[i])
			}
		}
	}
}

// TestModelsEndpointOrderStable 验证 HTTP 层拿到的顺序也稳定。
func TestModelsEndpointOrderStable(t *testing.T) {
	_, _, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a")

	var first []string
	for n := 0; n < 5; n++ {
		resp, err := http.Get(srv.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()

		ids := make([]string, 0, len(out.Data))
		for _, m := range out.Data {
			ids = append(ids, m.ID)
		}
		if n == 0 {
			first = ids
			continue
		}
		if len(ids) != len(first) {
			t.Fatalf("第 %d 次长度变化", n)
		}
		for i := range ids {
			if ids[i] != first[i] {
				t.Fatalf("第 %d 次顺序变化：%q vs %q", n, ids[i], first[i])
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 「反代使用中」标记
// ─────────────────────────────────────────────────────────────

// acctViewForTest 是 /api/accounts 的最小反序列化结构。
type acctViewForTest struct {
	UID   string `json:"uid"`
	InUse bool   `json:"in_use"`
}

func fetchAccounts(t *testing.T, baseURL string) []acctViewForTest {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Accounts []acctViewForTest `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Accounts
}

// setExpiry 把某账号的额度快照换成单个指定到期日的包。
func setExpiry(t *testing.T, store *auth.Store, uid, expireAt string) {
	t.Helper()
	ok := store.Mutate(uid, func(x *auth.Account) bool {
		x.Credit.Packages = []auth.PackageSnapshot{
			{Name: "测试包", Remain: 100, ExpireAt: expireAt},
		}
		x.Credit.Known = true
		x.Credit.Remaining = 100
		return true
	})
	if !ok {
		t.Fatalf("账号 %s 不存在", uid)
	}
}

// TestAccountsInUseMarksExactlyOne 验证恰好一个账号被标 in_use，
// 且是"最早到期包"的那个（与调度规则一致）。
func TestAccountsInUseMarksExactlyOne(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a", "uid-b")

	setExpiry(t, store, "uid-a", "2027-06-01")
	setExpiry(t, store, "uid-b", "2027-01-01") // 更早 → 应被选中

	inUse := 0
	inUseUID := ""
	for _, a := range fetchAccounts(t, srv.URL) {
		if a.InUse {
			inUse++
			inUseUID = a.UID
		}
	}
	if inUse != 1 {
		t.Fatalf("应恰好 1 个账号标 in_use，实际 %d 个", inUse)
	}
	if inUseUID != "uid-b" {
		t.Errorf("in_use 应是更早到期的 uid-b，实际 %q", inUseUID)
	}
}

// TestAccountsInUseNoneWhenAllDisabled 验证全部禁用时无账号被标。
func TestAccountsInUseNoneWhenAllDisabled(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a", "uid-b")

	for _, uid := range []string{"uid-a", "uid-b"} {
		store.Mutate(uid, func(x *auth.Account) bool { x.ManualDisabled = true; return true })
	}

	for _, a := range fetchAccounts(t, srv.URL) {
		if a.InUse {
			t.Errorf("全部禁用时不该有 in_use，实际标记了 %s", a.UID)
		}
	}
}

// TestAccountsInUseMatchesSelector 验证 in_use 与 pool.Selector 结论一致。
//
// 这是防"面板显示与实际转发不一致"的关键断言。
func TestAccountsInUseMatchesSelector(t *testing.T) {
	_, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a", "uid-b")

	setExpiry(t, store, "uid-a", "2027-03-01")
	setExpiry(t, store, "uid-b", "2027-09-01")

	picked, err := pool.NewSelector().Pick(store.PoolAccounts(nowFunc()))
	if err != nil {
		t.Fatalf("调度器应能选出账号: %v", err)
	}

	for _, a := range fetchAccounts(t, srv.URL) {
		want := a.UID == picked.ID
		if a.InUse != want {
			t.Errorf("账号 %s：面板 in_use=%v，调度器结论=%v（不一致）",
				a.UID, a.InUse, want)
		}
	}
}
