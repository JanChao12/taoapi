package workbuddy

import (
	"encoding/json"
	"testing"
)

// ─────────────────────────────────────────────────────────────
// 缓存字段透传的护栏测试（2026-10-06）
//
// 🔴 守的是一个**真实存在很久但没被发现的 bug**：
//
//	面板的"缓存命中率"一直是空的。根因是**整条链路缺这两个字段**：
//	  ① 上游 SSE 解析器（sse.go 的 upstreamUsage）**有**这两个字段
//	  ② 但 toProviderUsage 没往下传（canonical Usage 也没这两个字段）
//	  ③ 协议层 EventUsage 也没有
//	  ④ 记账器拿不到 ⇒ JSONL 里永远是 0 ⇒ 命中率恒为空
//
//	"字段在两端都有、中间断了"是典型的一类 bug：
//	单测各自覆盖两端，却没有任何测试走**全链路**，所以一直没暴露。
//
// 这些测试覆盖 ①②（本包内），③④ 在 app 包。
// ─────────────────────────────────────────────────────────────

// TestToProviderUsageCarriesCacheFields 守：上游 usage → canonical 不丢缓存字段。
func TestToProviderUsageCarriesCacheFields(t *testing.T) {
	u := &upstreamUsage{
		PromptTokens:          1000,
		CompletionTokens:      50,
		TotalTokens:           1050,
		PromptCacheHitTokens:  800,
		PromptCacheMissTokens: 200,
	}

	got := u.toProviderUsage()
	if got == nil {
		t.Fatal("转换结果不该为 nil")
	}
	if got.PromptCacheHitTokens != 800 {
		t.Errorf("PromptCacheHitTokens = %d，期望 800 —— "+
			"漏传这个字段会让面板命中率恒为空", got.PromptCacheHitTokens)
	}
	if got.PromptCacheMissTokens != 200 {
		t.Errorf("PromptCacheMissTokens = %d，期望 200", got.PromptCacheMissTokens)
	}
}

// TestToProviderUsageCacheZeroIsPreserved 守：上游没给缓存数据时保持 0。
//
// 0 在这里是**正确**的：表示"上游没报告缓存"（前端据分母为 0 显示 —），
// 而不是"命中率 0%"。所以这里不该把它变成别的值。
func TestToProviderUsageCacheZeroIsPreserved(t *testing.T) {
	u := &upstreamUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	got := u.toProviderUsage()
	if got.PromptCacheHitTokens != 0 || got.PromptCacheMissTokens != 0 {
		t.Errorf("无缓存数据时应保持 0，实际 hit=%d miss=%d",
			got.PromptCacheHitTokens, got.PromptCacheMissTokens)
	}
}

// TestUpstreamUsageParsesCacheFields 守：上游 JSON 字段名解析正确。
//
// 字段名是**实测**的（prompt_cache_hit_tokens / prompt_cache_miss_tokens）。
// 若上游改名，这条会红，而不是静默变成 0。
func TestUpstreamUsageParsesCacheFields(t *testing.T) {
	raw := []byte(`{
		"prompt_tokens": 500,
		"completion_tokens": 30,
		"total_tokens": 530,
		"prompt_cache_hit_tokens": 400,
		"prompt_cache_miss_tokens": 100
	}`)
	var u upstreamUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	if u.PromptCacheHitTokens != 400 {
		t.Errorf("解析 hit = %d，期望 400（字段名可能被上游改了）",
			u.PromptCacheHitTokens)
	}
	if u.PromptCacheMissTokens != 100 {
		t.Errorf("解析 miss = %d，期望 100", u.PromptCacheMissTokens)
	}
}
