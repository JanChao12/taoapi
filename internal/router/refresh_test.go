package router

import (
	"context"
	"errors"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// ═══════════════════════════════════════════════════════════════════
// Router.Refresh：运行期重新拉取模型目录的语义
//
// 🔴 为什么它与 Register 必须分开（2026-10-07，为「刷新模型列表」而加）：
//
//	Register 是**合并**语义（只写不删）⇒ 上游**下架**的模型会永远留着。
//	运行期刷新必须用 Refresh（先删该 provider 旧条目再写），
//	否则"刷新"反而让列表只增不减。
//
// 两条安全约定必须有护栏：
//   ① 拉取失败 ⇒ **不动**原有列表（否则一次网络抖动就把好列表清空）
//   ② 返回 0 条 ⇒ 按失败处理（上游改结构时不能把整平台清空）
// ═══════════════════════════════════════════════════════════════════

// stubProvider 是可编程的假 provider（按调用次数返回不同结果）。
type stubProvider struct {
	id string

	// calls 记录被调用次数
	calls int

	// modelsFor 按第 n 次调用（0-based）返回模型列表或错误
	modelsFor func(n int) ([]provider.Model, error)
}

func (s *stubProvider) ID() string { return s.id }

func (s *stubProvider) Models(_ context.Context) ([]provider.Model, error) {
	n := s.calls
	s.calls++
	if s.modelsFor == nil {
		return nil, nil
	}
	return s.modelsFor(n)
}

func (s *stubProvider) Chat(context.Context, provider.ChatRequest,
	func(provider.Event) error) error {
	return errors.New("not used")
}

func (s *stubProvider) Credit(context.Context, string) (provider.CreditResult, error) {
	return provider.CreditResult{}, errors.New("not used")
}

func (s *stubProvider) Checkin(context.Context, string) (provider.CheckinResult, error) {
	return provider.CheckinResult{}, errors.New("not used")
}

// mkModels 造一批模型，ID 前缀用**该 provider 的 ID** ——
// Router 是按 providerID 记账的，测试也必须让前缀与之对应，
// 否则 `idsIn` 无法按 provider 过滤（我第一版就写错了这一点）。
func mkModels(providerID string, ids ...string) []provider.Model {
	out := make([]provider.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, provider.Model{
			ID:         providerID + "/" + id,
			UpstreamID: id,
		})
	}
	return out
}

// idsIn 返回路由里指定 provider 的模型 ID 集合。
func idsIn(r *Router, providerID string) map[string]bool {
	out := map[string]bool{}
	for _, m := range r.ModelsWithoutAliases() {
		if len(m.ID) > len(providerID) && m.ID[:len(providerID)+1] == providerID+"/" {
			out[m.ID] = true
		}
	}
	return out
}

// TestRefreshRemovesDelistedModels 守：**下架的模型会被移除**（这是 Refresh 存在的理由）。
//
// 对照：Register 是合并且不清，所以下架模型会滞留 —— 用户选到它只会报错。
func TestRefreshRemovesDelistedModels(t *testing.T) {
	r := New()
	p := &stubProvider{id: "stub", modelsFor: func(n int) ([]provider.Model, error) {
		if n == 0 {
			return mkModels("stub", "a", "b", "c"), nil // 首次：三个
		}
		return mkModels("stub", "a"), nil // 刷新后：只剩 a（b/c 已下架）
	}}

	if err := r.Register(context.Background(), p); err != nil {
		t.Fatalf("初次注册失败: %v", err)
	}
	if err := r.Refresh(context.Background(), p); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}

	got := idsIn(r, "stub")
	if len(got) != 1 || !got["stub/a"] {
		t.Errorf("刷新后应为 {stub/a}，实际 %v —— "+
			"下架模型必须被移除（这正是 Refresh 与 Register 的区别）", got)
	}
}

// TestRefreshKeepsOtherProviders 守：刷新一个 provider **不影响**另一个。
func TestRefreshKeepsOtherProviders(t *testing.T) {
	r := New()
	p1 := &stubProvider{id: "one", modelsFor: func(int) ([]provider.Model, error) {
		return mkModels("one", "x"), nil
	}}
	p2 := &stubProvider{id: "two", modelsFor: func(n int) ([]provider.Model, error) {
		if n == 0 {
			return mkModels("two", "y"), nil
		}
		return mkModels("two", "y", "z"), nil
	}}

	if err := r.Register(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(context.Background(), p2); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(context.Background(), p2); err != nil {
		t.Fatal(err)
	}

	if got := idsIn(r, "one"); len(got) != 1 || !got["one/x"] {
		t.Errorf("刷新 two 后 one 的模型被动了: %v", got)
	}
	if got := idsIn(r, "two"); len(got) != 2 {
		t.Errorf("two 应为 2 个，实际 %v", got)
	}
}

// TestRefreshFailureKeepsExistingList 守：**拉取失败不清空原列表**。★
//
// 这是最重要的护栏：一次网络抖动不能把好列表清成空 ——
// 那会让用户看到"/v1/models 为空"，等于服务不可用。
func TestRefreshFailureKeepsExistingList(t *testing.T) {
	r := New()
	p := &stubProvider{id: "stub", modelsFor: func(n int) ([]provider.Model, error) {
		if n == 0 {
			return mkModels("stub", "a", "b"), nil
		}
		return nil, errors.New("上游 500")
	}}

	if err := r.Register(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	err := r.Refresh(context.Background(), p)
	if err == nil {
		t.Fatal("拉取失败应返回 error")
	}

	got := idsIn(r, "stub")
	if len(got) != 2 {
		t.Errorf("失败后原有列表被破坏: %v —— 必须原封不动", got)
	}
}

// TestRefreshEmptyResultIsFailure 守：返回 **0 条按失败处理**（不清空原列表）。★
//
// 理由：上游抖动/改结构时可能返回空目录；若照单全收，
// 整个平台的模型会消失 —— 用户可见的严重回归。
func TestRefreshEmptyResultIsFailure(t *testing.T) {
	r := New()
	p := &stubProvider{id: "stub", modelsFor: func(n int) ([]provider.Model, error) {
		if n == 0 {
			return mkModels("stub", "a", "b"), nil
		}
		return []provider.Model{}, nil // 成功但空
	}}

	if err := r.Register(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(context.Background(), p); err == nil {
		t.Fatal("返回 0 条应视为失败")
	}

	if got := idsIn(r, "stub"); len(got) != 2 {
		t.Errorf("空返回后原列表被清空了: %v —— 必须保留", got)
	}
}

// TestRefreshIsAtomicUnderConcurrentRead 守：刷新期间并发读**不会看到空列表**。
//
// ⚠️ 本机无 gcc，`go test -race` 用不了 ⇒ 这里只断言"读到的数量
// 要么是旧的、要么是新的，绝不为 0"（删除与写入在同一次持锁内完成）。
func TestRefreshIsAtomicUnderConcurrentRead(t *testing.T) {
	r := New()
	p := &stubProvider{id: "stub", modelsFor: func(n int) ([]provider.Model, error) {
		if n == 0 {
			return mkModels("stub", "a", "b"), nil
		}
		return mkModels("stub", "c"), nil
	}}
	if err := r.Register(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = r.Refresh(context.Background(), p)
		}
	}()

	sawEmpty := false
	for {
		select {
		case <-done:
			if sawEmpty {
				t.Error("并发读观测到**空列表** —— 说明删除与写入不在同一次持锁内完成")
			}
			return
		default:
			if len(idsIn(r, "stub")) == 0 {
				sawEmpty = true
			}
		}
	}
}

// TestCountByProviderMatchesVisibleModels 守：`CountByProvider` 与
// 面板看到的数量一致（口径必须与 ModelsWithoutAliases 相同）。
func TestCountByProviderMatchesVisibleModels(t *testing.T) {
	r := New()
	p := &stubProvider{id: "stub", modelsFor: func(int) ([]provider.Model, error) {
		return mkModels("stub", "a", "b", "c"), nil
	}}
	if err := r.Register(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := r.CountByProvider("stub"); got != 3 {
		t.Errorf("CountByProvider = %d，期望 3", got)
	}
	if got := r.CountByProvider("nope"); got != 0 {
		t.Errorf("不存在的 provider 应为 0，实际 %d", got)
	}
}
