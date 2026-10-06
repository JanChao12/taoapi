package router

import (
	"context"
	"testing"

	"workbuddy.local/workbuddy-api/internal/provider"
)

// registerFakeProvider 注册一个只带指定模型的假渠道（复用 router_test.go 的 fakeProvider）。
func registerFakeProvider(t *testing.T, r *Router, id string, modelIDs ...string) {
	t.Helper()
	models := make([]provider.Model, 0, len(modelIDs))
	for _, mid := range modelIDs {
		models = append(models, provider.Model{
			ID:         id + "/" + mid,
			UpstreamID: mid,
			Name:       mid,
		})
	}
	if err := r.Register(context.Background(), &fakeProvider{id: id, models: models}); err != nil {
		t.Fatalf("注册假渠道 %s 失败: %v", id, err)
	}
}

// ─────────────────────────────────────────────────────────────
// CanonicalModelID 的护栏测试（2026-10-06）
//
// 🔴 守的是一个真实的显示问题：同一个模型因为客户端写法不同，
// 在用量统计里被记成三条 ——
//
//	"dsf"                          ← 别名
//	"deepseek-v4.1-flash"          ← 裸 ID
//	"workbuddy/deepseek-v4.1-flash" ← 标准形态
//
// 委托方原话：
//
//	「这个dsf和deepseek-v4.1-flash使用的都是workbuddy/deepseek-v4.1-flash，
//	  为什么三种显示，都显示workbuddy/deepseek-v4.1-flash就行」
// ─────────────────────────────────────────────────────────────

// TestCanonicalModelIDNormalizesThreeForms 守：三种写法归一到同一个 ID。
func TestCanonicalModelIDNormalizesThreeForms(t *testing.T) {
	r := New()
	// 别名目标必须是**已注册**的模型（SetAliases 会校验存在性）
	registerFakeProvider(t, r, "workbuddy", "deepseek-v4.1-flash")

	const canonical = "workbuddy/deepseek-v4.1-flash"

	// 给别名 dsf → workbuddy/deepseek-v4.1-flash
	if err := r.SetAliases(map[string]string{"dsf": canonical}); err != nil {
		t.Fatalf("设置别名失败: %v", err)
	}

	cases := []struct{ in, why string }{
		{"dsf", "别名"},
		{"deepseek-v4.1-flash", "裸 ID（缺前缀）"},
		{canonical, "标准形态（已带前缀）"},
	}
	for _, c := range cases {
		got := r.CanonicalModelID(c.in)
		if got != canonical {
			t.Errorf("CanonicalModelID(%q) = %q，期望 %q（%s）",
				c.in, got, canonical, c.why)
		}
	}
}

// TestCanonicalModelIDKeepsOtherPrefixes 守：别的平台前缀不被改动。
//
// 🔴 为什么重要：委托方是**多平台聚合反代**，将来会有
// opencodezen/xxx、workbuddyai/xxx 之类的 ID。
// 本渠道的归一化绝不能把别人的前缀替换掉 —— 那会把统计归到错误的模型。
func TestCanonicalModelIDKeepsOtherPrefixes(t *testing.T) {
	r := New()
	registerFakeProvider(t, r, "workbuddy", "glm-5.3")

	for _, id := range []string{"opencodezen/big-pickle", "workbuddyai/some-model"} {
		if got := r.CanonicalModelID(id); got != id {
			t.Errorf("CanonicalModelID(%q) = %q，期望原样返回"+
				"（别的平台的前缀不该被本渠道改写）", id, got)
		}
	}
}

// TestCanonicalModelIDEmptyAndUnknown 守边界情况。
func TestCanonicalModelIDEmptyAndUnknown(t *testing.T) {
	r := New()
	registerFakeProvider(t, r, "workbuddy", "glm-5.3")

	if got := r.CanonicalModelID(""); got != "" {
		t.Errorf("空串应原样返回，实际 %q", got)
	}
	if got := r.CanonicalModelID("   "); got != "" {
		t.Errorf("纯空白应归一为空串，实际 %q", got)
	}
	// 未注册的裸 ID 仍补前缀（它可能就是本渠道的模型，只是没注册进路由）
	if got := r.CanonicalModelID("unregistered-model"); got != "workbuddy/unregistered-model" {
		t.Errorf("裸 ID 应补唯一渠道前缀，实际 %q", got)
	}
}

// TestCanonicalModelIDMultiProviderStillPrefixes 守：多渠道下**仍要**补前缀。
//
// 🔴 契约在 2026-10-06 变sharp了（委托方实测反馈）：
//
//	「图4中"deepseek-v4.1-flash"现在应该加上前缀了，因为以前没有
//	  接入国际版可以这样写，现在接入了国际版应该做好区分」
//
//	**旧实现**：判据是"系统里有几个渠道" —— 只要多于一个就不补。
//	  接入国际版后渠道变成两个 ⇒ **一律不补** ⇒ 老记录永远显示裸 ID。
//	  那条规则在单平台时代够用，多平台下就失效了。
//
//	**新实现**：判据是"**这个模型实际属于哪个渠道**"（查真实注册表）。
//	  `glm-5.3` 只注册在 workbuddy ⇒ 就该补成 `workbuddy/glm-5.3`，
//	  即使系统里还有别的渠道。
func TestCanonicalModelIDMultiProviderStillPrefixes(t *testing.T) {
	r := New()
	registerFakeProvider(t, r, "workbuddy", "glm-5.3")
	registerFakeProvider(t, r, "opencodezen", "big-pickle")

	// glm-5.3 只在 workbuddy 里 ⇒ 应补 workbuddy/ 前缀
	if got := r.CanonicalModelID("glm-5.3"); got != "workbuddy/glm-5.3" {
		t.Errorf("glm-5.3 只属于 workbuddy，应补前缀，实际 %q", got)
	}
	// big-pickle 只在 opencodezen 里 ⇒ 应补 opencodezen/
	if got := r.CanonicalModelID("big-pickle"); got != "opencodezen/big-pickle" {
		t.Errorf("big-pickle 只属于 opencodezen，应补前缀，实际 %q", got)
	}
}

// TestCanonicalModelIDAmbiguousStaysBare 守：**两渠道同名**时不猜。
//
// 这是真正该"保持裸 ID"的场景（与"有几个渠道"无关）：
// `glm-5.3` 在两个渠道都存在 ⇒ 补哪个都是猜 ⇒ 猜错会把统计
// 归到错误的平台。宁可显示裸 ID（用户能看出有歧义），
// 也不要给出一个看起来正确其实错的归属。
func TestCanonicalModelIDAmbiguousStaysBare(t *testing.T) {
	r := New()
	registerFakeProvider(t, r, "workbuddy", "glm-5.3")
	registerFakeProvider(t, r, "workbuddyai", "glm-5.3")

	if got := r.CanonicalModelID("glm-5.3"); got != "glm-5.3" {
		t.Errorf("两渠道同名时不应猜前缀，实际 %q", got)
	}
	// 但**带前缀**的仍原样返回（明确的归属不该被改写）
	if got := r.CanonicalModelID("workbuddy/glm-5.3"); got != "workbuddy/glm-5.3" {
		t.Errorf("带前缀的应原样返回，实际 %q", got)
	}
	if got := r.CanonicalModelID("workbuddyai/glm-5.3"); got != "workbuddyai/glm-5.3" {
		t.Errorf("带前缀的应原样返回，实际 %q", got)
	}
}
